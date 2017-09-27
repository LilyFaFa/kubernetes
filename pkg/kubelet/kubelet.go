/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubelet

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	cadvisorapi "github.com/google/cadvisor/info/v1"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/apis/componentconfig"
	componentconfigv1alpha1 "k8s.io/kubernetes/pkg/apis/componentconfig/v1alpha1"
	"k8s.io/kubernetes/pkg/client/cache"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/fields"
	internalApi "k8s.io/kubernetes/pkg/kubelet/api"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	dockerremote "k8s.io/kubernetes/pkg/kubelet/dockershim/remote"
	"k8s.io/kubernetes/pkg/kubelet/dockertools"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/network"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/remote"
	"k8s.io/kubernetes/pkg/kubelet/rkt"
	"k8s.io/kubernetes/pkg/kubelet/server"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/security/apparmor"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/bandwidth"
	"k8s.io/kubernetes/pkg/util/clock"
	utilconfig "k8s.io/kubernetes/pkg/util/config"
	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	utilexec "k8s.io/kubernetes/pkg/util/exec"
	"k8s.io/kubernetes/pkg/util/flowcontrol"
	"k8s.io/kubernetes/pkg/util/integer"
	kubeio "k8s.io/kubernetes/pkg/util/io"
	utilipt "k8s.io/kubernetes/pkg/util/iptables"
	"k8s.io/kubernetes/pkg/util/mount"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/util/procfs"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
)

const (
	// Max amount of time to wait for the container runtime to come up.
	maxWaitForContainerRuntime = 5 * time.Minute

	// nodeStatusUpdateRetry specifies how many times kubelet retries when posting node status failed.
	nodeStatusUpdateRetry = 5

	// Location of container logs.
	ContainerLogsDir = "/var/log/containers"

	// max backoff period, exported for the e2e test
	MaxContainerBackOff = 300 * time.Second

	// Capacity of the channel for storing pods to kill. A small number should
	// suffice because a goroutine is dedicated to check the channel and does
	// not block on anything else.
	podKillingChannelCapacity = 50

	// Period for performing global cleanup tasks.
	housekeepingPeriod = time.Second * 2

	// Period for performing eviction monitoring.
	// TODO ensure this is in sync with internal cadvisor housekeeping.
	evictionMonitoringPeriod = time.Second * 10

	// The path in containers' filesystems where the hosts file is mounted.
	etcHostsPath = "/etc/hosts"

	// Capacity of the channel for receiving pod lifecycle events. This number
	// is a bit arbitrary and may be adjusted in the future.
	plegChannelCapacity = 1000

	// Generic PLEG relies on relisting for discovering container events.
	// A longer period means that kubelet will take longer to detect container
	// changes and to update pod status. On the other hand, a shorter period
	// will cause more frequent relisting (e.g., container runtime operations),
	// leading to higher cpu usage.
	// Note that even though we set the period to 1s, the relisting itself can
	// take more than 1s to finish if the container runtime responds slowly
	// and/or when there are many container changes in one cycle.
	plegRelistPeriod = time.Second * 1

	// backOffPeriod is the period to back off when pod syncing results in an
	// error. It is also used as the base period for the exponential backoff
	// container restarts and image pulls.
	backOffPeriod = time.Second * 10

	// Period for performing container garbage collection.
	ContainerGCPeriod = time.Minute
	// Period for performing image garbage collection.
	ImageGCPeriod = 5 * time.Minute

	// Minimum number of dead containers to keep in a pod
	minDeadContainerInPod = 1
)

// SyncHandler is an interface implemented by Kubelet, for testability
type SyncHandler interface {
	HandlePodAdditions(pods []*api.Pod)
	HandlePodUpdates(pods []*api.Pod)
	HandlePodRemoves(pods []*api.Pod)
	HandlePodReconcile(pods []*api.Pod)
	HandlePodSyncs(pods []*api.Pod)
	HandlePodCleanups() error
}

// Option is a functional option type for Kubelet
type Option func(*Kubelet)

// bootstrapping interface for kubelet, targets the initialization protocol
type KubeletBootstrap interface {
	GetConfiguration() componentconfig.KubeletConfiguration
	BirthCry()
	StartGarbageCollection()
	ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableDebuggingHandlers bool)
	ListenAndServeReadOnly(address net.IP, port uint)
	Run(<-chan kubetypes.PodUpdate)
	RunOnce(<-chan kubetypes.PodUpdate) ([]RunPodResult, error)
}

// create and initialize a Kubelet instance
type KubeletBuilder func(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps, standaloneMode bool) (KubeletBootstrap, error)

// KubeletDeps is a bin for things we might consider "injected dependencies" -- objects constructed
// at runtime that are necessary for running the Kubelet. This is a temporary solution for grouping
// these objects while we figure out a more comprehensive dependency injection story for the Kubelet.
type KubeletDeps struct {
	// TODO(mtaufen): KubeletBuilder:
	//                Mesos currently uses this as a hook to let them make their own call to
	//                let them wrap the KubeletBootstrap that CreateAndInitKubelet returns with
	//                their own KubeletBootstrap. It's a useful hook. I need to think about what
	//                a nice home for it would be. There seems to be a trend, between this and
	//                the Options fields below, of providing hooks where you can add extra functionality
	//                to the Kubelet for your solution. Maybe we should centralize these sorts of things?
	Builder KubeletBuilder

	// TODO(mtaufen): ContainerRuntimeOptions and Options:
	//                Arrays of functions that can do arbitrary things to the Kubelet and the Runtime
	//                seem like a difficult path to trace when it's time to debug something.
	//                I'm leaving these fields here for now, but there is likely an easier-to-follow
	//                way to support their intended use cases. E.g. ContainerRuntimeOptions
	//                is used by Mesos to set an environment variable in containers which has
	//                some connection to their container GC. It seems that Mesos intends to use
	//                Options to add additional node conditions that are updated as part of the
	//                Kubelet lifecycle (see https://github.com/kubernetes/kubernetes/pull/21521).
	//                We should think about providing more explicit ways of doing these things.
	ContainerRuntimeOptions []kubecontainer.Option
	Options                 []Option

	// Injected Dependencies
	Auth server.AuthInterface
	//提供 cAdvisor 接口功能的组件，用来获取监控信息
	CAdvisorInterface cadvisor.Interface
	Cloud             cloudprovider.Interface
	ContainerManager  cm.ContainerManager
	//用于和docker做交互
	DockerClient dockertools.DockerInterface
	//和事件机制有关
	EventClient *clientset.Clientset
	//用于和apiserver做交互
	KubeClient *clientset.Clientset
	//执行mount相关的操作
	Mounter mount.Interface
	//网络插件，执行网络设置任务
	NetworkPlugins []network.NetworkPlugin
	OOMAdjuster    *oom.OOMAdjuster
	OSInterface    kubecontainer.OSInterface
	PodConfig      *config.PodConfig
	//事件机制，看一下Recorder
	Recorder record.EventRecorder
	Writer   kubeio.Writer
	//volum插件，用于执行volum设置任务
	VolumePlugins []volume.VolumePlugin
	TLSOptions    *server.TLSOptions

	//上述组件可以看到，这些组件要么需要和第三方交互（kubeClient、DockerClient）
	//要么有副作用（ Mounter、NetworkPlugins、VolumePlugins），
	//在进行单元测试的时候一般都会编写对应的 Fake 对象，只要满足响应的接口，就能正常工作。
}

// makePodSourceConfig creates a config.PodConfig from the given
// KubeletConfiguration or returns an error.
// 这里用到了Mux这一套框架，podcfg实现了Mux接口，
// Mux框架用来合并多个数据源，当任何一个数据源有更新时，都会促发Mux的Merge函数用来合并更新的信息，
// 这里podCfg有是哪个数据源，
// 即kubetypes.FileSource、kubetypes.HTTPSource和kubetypes.ApiserverSource。
func makePodSourceConfig(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps, nodeName types.NodeName) (*config.PodConfig, error) {
	manifestURLHeader := make(http.Header)
	if kubeCfg.ManifestURLHeader != "" {
		pieces := strings.Split(kubeCfg.ManifestURLHeader, ":")
		if len(pieces) != 2 {
			return nil, fmt.Errorf("manifest-url-header must have a single ':' key-value separator, got %q", kubeCfg.ManifestURLHeader)
		}
		manifestURLHeader.Set(pieces[0], pieces[1])
	}

	// source of all configuration
	// 在NewPodConfig 函数中，创建了一个容量为50的channel,该channel后面传给了main loop的处理函数
	cfg := config.NewPodConfig(config.PodConfigNotificationIncremental, kubeDeps.Recorder)

	// define file config source
	if kubeCfg.PodManifestPath != "" {
		glog.Infof("Adding manifest file: %v", kubeCfg.PodManifestPath)
		//cfg.Channel
		//这里用到了Mux这一套框架，podcfg实现了Mux接口，
		//Mux框架用来合并多个数据源，当任何一个数据源有更新时，
		//都会促发Mux的Merge函数用来合并更新的信息，这里podCfg有是哪个数据源，
		//即kubetypes.FileSource、kubetypes.HTTPSource和kubetypes.ApiserverSource
		//cfg.Channel
		config.NewSourceFile(kubeCfg.PodManifestPath, nodeName, kubeCfg.FileCheckFrequency.Duration, cfg.Channel(kubetypes.FileSource))
	}

	// define url config source
	if kubeCfg.ManifestURL != "" {
		glog.Infof("Adding manifest url %q with HTTP header %v", kubeCfg.ManifestURL, manifestURLHeader)
		config.NewSourceURL(kubeCfg.ManifestURL, manifestURLHeader, nodeName, kubeCfg.HTTPCheckFrequency.Duration, cfg.Channel(kubetypes.HTTPSource))
	}
	if kubeDeps.KubeClient != nil {
		glog.Infof("Watching apiserver")
		//以从apiserver数据源为例分析
		config.NewSourceApiserver(kubeDeps.KubeClient, nodeName, cfg.Channel(kubetypes.ApiserverSource))
	}
	return cfg, nil
}

//创建两个service以支持CRI
func getRuntimeAndImageServices(config *componentconfig.KubeletConfiguration) (internalApi.RuntimeService, internalApi.ImageManagerService, error) {
	//创建runtimeService，将config.RemoteRuntimeEndpoint为参数传过去，以建立gRPC连接
	rs, err := remote.NewRemoteRuntimeService(config.RemoteRuntimeEndpoint, config.RuntimeRequestTimeout.Duration)
	if err != nil {
		return nil, nil, err
	}
	is, err := remote.NewRemoteImageService(config.RemoteImageEndpoint, config.RuntimeRequestTimeout.Duration)
	if err != nil {
		return nil, nil, err
	}
	return rs, is, err
}

// NewMainKubelet instantiates a new Kubelet object along with all the required internal modules.
// No initialization of Kubelet and its modules should happen here.、／

//创建kubelet这个对象，包括kubelet运行所需要的全部对象，并且对各个对象进行初始化和赋值的过程
func NewMainKubelet(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps, standaloneMode bool) (*Kubelet, error) {
	if kubeCfg.RootDirectory == "" {
		return nil, fmt.Errorf("invalid root directory %q", kubeCfg.RootDirectory)
	}
	if kubeCfg.SyncFrequency.Duration <= 0 {
		return nil, fmt.Errorf("invalid sync frequency %d", kubeCfg.SyncFrequency.Duration)
	}

	if kubeCfg.MakeIPTablesUtilChains {
		if kubeCfg.IPTablesMasqueradeBit > 31 || kubeCfg.IPTablesMasqueradeBit < 0 {
			return nil, fmt.Errorf("iptables-masquerade-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit > 31 || kubeCfg.IPTablesDropBit < 0 {
			return nil, fmt.Errorf("iptables-drop-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit == kubeCfg.IPTablesMasqueradeBit {
			return nil, fmt.Errorf("iptables-masquerade-bit and iptables-drop-bit must be different")
		}
	}

	hostname := nodeutil.GetHostname(kubeCfg.HostnameOverride)
	// Query the cloud provider for our node name, default to hostname
	nodeName := types.NodeName(hostname)
	if kubeDeps.Cloud != nil {
		var err error
		instances, ok := kubeDeps.Cloud.Instances()
		if !ok {
			return nil, fmt.Errorf("failed to get instances from cloud provider")
		}

		nodeName, err = instances.CurrentNodeName(hostname)
		if err != nil {
			return nil, fmt.Errorf("error fetching current instance name from cloud provider: %v", err)
		}

		glog.V(2).Infof("cloud provider determined current node name to be %s", nodeName)
	}

	// TODO: KubeletDeps.KubeClient should be a client interface, but client interface misses certain methods
	// used by kubelet. Since NewMainKubelet expects a client interface, we need to make sure we are not passing
	// a nil pointer to it when what we really want is a nil interface.
	var kubeClient clientset.Interface
	if kubeDeps.KubeClient != nil {
		kubeClient = kubeDeps.KubeClient
		// TODO: remove this when we've refactored kubelet to only use clientset.
	}
	// 1、PodConfig 非常重要，它是 pod 信息的来源，
	// kubelet 支持文件、URL 和 apiserver 三种渠道，
	// 例如使用kubeadm创建apiserver等master节点的pod时就时从/etc/kubernentes/manifests目录读取
	// yaml然后进行安装
	// PodConfig 将它们汇聚到一起，通过管道来传递,PodConfig.Channel是合并后的数据源
	// 这个对象里面会从文件、网络和 apiserver 三个来源中汇聚节点要运行的 pod 信息，并通过管道发送出来，
	// 读取这个管道就能获取实时的 pod 最新配置
	if kubeDeps.PodConfig == nil {
		var err error
		//创建PodConfig
		//进这个函数看一下
		//在NewPodConfig 函数中，创建了一个容量为50的channel"updates"。
		//该channel"updates"后面传给了main loop的处理函数
		kubeDeps.PodConfig, err = makePodSourceConfig(kubeCfg, kubeDeps, nodeName)
		if err != nil {
			return nil, err
		}
	}
	//2、初始化创建容器 GC 的策略
	containerGCPolicy := kubecontainer.ContainerGCPolicy{
		MinAge:             kubeCfg.MinimumGCAge.Duration,
		MaxPerPodContainer: int(kubeCfg.MaxPerPodContainerCount),
		MaxContainers:      int(kubeCfg.MaxContainerCount),
	}

	daemonEndpoints := &api.NodeDaemonEndpoints{
		KubeletEndpoint: api.DaemonEndpoint{Port: kubeCfg.Port},
	}
	//初始化创建镜像的GC策略
	imageGCPolicy := images.ImageGCPolicy{
		MinAge:               kubeCfg.ImageMinimumGCAge.Duration,
		HighThresholdPercent: int(kubeCfg.ImageGCHighThresholdPercent),
		LowThresholdPercent:  int(kubeCfg.ImageGCLowThresholdPercent),
	}
	//磁盘空间策略
	diskSpacePolicy := DiskSpacePolicy{
		DockerFreeDiskMB: int(kubeCfg.LowDiskSpaceThresholdMB),
		RootFreeDiskMB:   int(kubeCfg.LowDiskSpaceThresholdMB),
	}

	thresholds, err := eviction.ParseThresholdConfig(kubeCfg.EvictionHard, kubeCfg.EvictionSoft, kubeCfg.EvictionSoftGracePeriod, kubeCfg.EvictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	evictionConfig := eviction.Config{
		PressureTransitionPeriod: kubeCfg.EvictionPressureTransitionPeriod.Duration,
		MaxPodGracePeriodSeconds: int64(kubeCfg.EvictionMaxPodGracePeriod),
		Thresholds:               thresholds,
		KernelMemcgNotification:  kubeCfg.ExperimentalKernelMemcgNotification,
	}

	reservation, err := ParseReservation(kubeCfg.KubeReserved, kubeCfg.SystemReserved)
	if err != nil {
		return nil, err
	}
	// exec 处理函数，进入到容器中执行命令的方式。
	// 之前使用的是 nsenter 命令行的方式，
	// 后来 docker 提供了 `docker exec` 命令，默认是后者
	var dockerExecHandler dockertools.ExecHandler
	switch kubeCfg.DockerExecHandlerName {
	case "native":
		dockerExecHandler = &dockertools.NativeExecHandler{}
	case "nsenter":
		dockerExecHandler = &dockertools.NsenterExecHandler{}
	default:
		glog.Warningf("Unknown Docker exec handler %q; defaulting to native", kubeCfg.DockerExecHandlerName)
		dockerExecHandler = &dockertools.NativeExecHandler{}
	}
	// 使用 reflector 把 ListWatch 得到的服务信息实时同步到 serviceStore 对象中
	serviceStore := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if kubeClient != nil {
		serviceLW := cache.NewListWatchFromClient(kubeClient.Core().RESTClient(), "services", api.NamespaceAll, fields.Everything())
		cache.NewReflector(serviceLW, &api.Service{}, serviceStore, 0).Run()
	}
	//3、serviceLister能够读取 kubernetes 中服务信息
	serviceLister := &cache.StoreToServiceLister{Indexer: serviceStore}

	// 使用 reflector 把 ListWatch 得到的节点信息实时同步到  nodeStore 对象中
	nodeStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if kubeClient != nil {
		fieldSelector := fields.Set{api.ObjectNameField: string(nodeName)}.AsSelector()
		nodeLW := cache.NewListWatchFromClient(kubeClient.Core().RESTClient(), "nodes", api.NamespaceAll, fieldSelector)
		cache.NewReflector(nodeLW, &api.Node{}, nodeStore, 0).Run()
	}
	//3、nodeLister能够读取 kubernetes 中节点的信息
	nodeLister := &cache.StoreToNodeLister{Store: nodeStore}

	nodeInfo := &predicates.CachedNodeInfo{StoreToNodeLister: nodeLister}

	// TODO: get the real node object of ourself,
	// and use the real node name and UID.
	// TODO: what is namespace for node?
	nodeRef := &api.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName),
		Namespace: "",
	}
	//4、diskSpaceManager返回容器存储空间的信息
	diskSpaceManager, err := newDiskSpaceManager(kubeDeps.CAdvisorInterface, diskSpacePolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize disk manager: %v", err)
	}
	containerRefManager := kubecontainer.NewRefManager()

	//将kuebDeps.Recorder传给oomWatcher
	oomWatcher := NewOOMWatcher(kubeDeps.CAdvisorInterface, kubeDeps.Recorder)

	// 根据配置信息和各种对象创建 Kubelet 实例，后面还有参数的填充
	klet := &Kubelet{
		hostname:            hostname,
		nodeName:            nodeName,
		dockerClient:        kubeDeps.DockerClient,
		kubeClient:          kubeClient,
		rootDirectory:       kubeCfg.RootDirectory,
		resyncInterval:      kubeCfg.SyncFrequency.Duration,
		containerRefManager: containerRefManager,
		httpClient:          &http.Client{},
		sourcesReady:        config.NewSourcesReady(kubeDeps.PodConfig.SeenAllSources),
		registerNode:        kubeCfg.RegisterNode,
		registerSchedulable: kubeCfg.RegisterSchedulable,
		standaloneMode:      standaloneMode,
		clusterDomain:       kubeCfg.ClusterDomain,
		clusterDNS:          net.ParseIP(kubeCfg.ClusterDNS),
		//ServiceLister：能够读取 kubernetes 中服务信息
		serviceLister: serviceLister,
		//nodeLister：能够读取 apiserver 中节点的信息
		nodeLister:                     nodeLister,
		nodeInfo:                       nodeInfo,
		masterServiceNamespace:         kubeCfg.MasterServiceNamespace,
		streamingConnectionIdleTimeout: kubeCfg.StreamingConnectionIdleTimeout.Duration,
		recorder:                       kubeDeps.Recorder,
		cadvisor:                       kubeDeps.CAdvisorInterface,
		//diskSpaceManager：返回容器存储空间的信息
		diskSpaceManager: diskSpaceManager,
		cloud:            kubeDeps.Cloud,
		autoDetectCloudProvider:   (componentconfigv1alpha1.AutoDetectCloudProvider == kubeCfg.CloudProvider),
		nodeRef:                   nodeRef,
		nodeLabels:                kubeCfg.NodeLabels,
		nodeStatusUpdateFrequency: kubeCfg.NodeStatusUpdateFrequency.Duration,
		os:                kubeDeps.OSInterface,
		oomWatcher:        oomWatcher,
		cgroupsPerQOS:     kubeCfg.ExperimentalCgroupsPerQOS,
		cgroupRoot:        kubeCfg.CgroupRoot,
		mounter:           kubeDeps.Mounter,
		writer:            kubeDeps.Writer,
		nonMasqueradeCIDR: kubeCfg.NonMasqueradeCIDR,
		maxPods:           int(kubeCfg.MaxPods),
		podsPerCore:       int(kubeCfg.PodsPerCore),
		nvidiaGPUs:        int(kubeCfg.NvidiaGPUs),
		syncLoopMonitor:   atomic.Value{},
		resolverConfig:    kubeCfg.ResolverConfig,
		cpuCFSQuota:       kubeCfg.CPUCFSQuota,
		daemonEndpoints:   daemonEndpoints,
		containerManager:  kubeDeps.ContainerManager,
		nodeIP:            net.ParseIP(kubeCfg.NodeIP),
		clock:             clock.RealClock{},
		outOfDiskTransitionFrequency:            kubeCfg.OutOfDiskTransitionFrequency.Duration,
		reservation:                             *reservation,
		enableCustomMetrics:                     kubeCfg.EnableCustomMetrics,
		babysitDaemons:                          kubeCfg.BabysitDaemons,
		enableControllerAttachDetach:            kubeCfg.EnableControllerAttachDetach,
		iptClient:                               utilipt.New(utilexec.New(), utildbus.New(), utilipt.ProtocolIpv4),
		makeIPTablesUtilChains:                  kubeCfg.MakeIPTablesUtilChains,
		iptablesMasqueradeBit:                   int(kubeCfg.IPTablesMasqueradeBit),
		iptablesDropBit:                         int(kubeCfg.IPTablesDropBit),
		experimentalHostUserNamespaceDefaulting: utilconfig.DefaultFeatureGate.ExperimentalHostUserNamespaceDefaulting(),
	}

	if klet.experimentalHostUserNamespaceDefaulting {
		glog.Infof("Experimental host user namespace defaulting is enabled.")
	}

	if mode, err := effectiveHairpinMode(componentconfig.HairpinMode(kubeCfg.HairpinMode), kubeCfg.ContainerRuntime, kubeCfg.NetworkPluginName); err != nil {
		// This is a non-recoverable error. Returning it up the callstack will just
		// lead to retries of the same failure, so just fail hard.
		glog.Fatalf("Invalid hairpin mode: %v", err)
	} else {
		klet.hairpinMode = mode
	}
	glog.Infof("Hairpin mode set to %q", klet.hairpinMode)

	// 网络插件的初始化工作
	if plug, err := network.InitNetworkPlugin(kubeDeps.NetworkPlugins, kubeCfg.NetworkPluginName, &criNetworkHost{&networkHost{klet}}, klet.hairpinMode, klet.nonMasqueradeCIDR, int(kubeCfg.NetworkPluginMTU)); err != nil {
		return nil, err
	} else {
		klet.networkPlugin = plug
	}

	// 从 cAdvisor 获取当前机器的信息
	machineInfo, err := klet.GetCachedMachineInfo()
	if err != nil {
		return nil, err
	}

	procFs := procfs.NewProcFS()
	imageBackOff := flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)

	klet.livenessManager = proberesults.NewManager()

	// 4、podManager 负责管理当前节点上的 pod 信息，它保存了所有 pod 的内容，包括 static pod。
	// kubelet 从本地文件、网络地址和 apiserver 三个地方获取 pod 的内容，
	// 会保存当前节点的pod的状态
	klet.podCache = kubecontainer.NewCache()
	klet.podManager = kubepod.NewBasicPodManager(kubepod.NewBasicMirrorClient(klet.kubeClient))

	if kubeCfg.RemoteRuntimeEndpoint != "" {
		// kubeCfg.RemoteImageEndpoint is same as kubeCfg.RemoteRuntimeEndpoint if not explicitly specified
		if kubeCfg.RemoteImageEndpoint == "" {
			kubeCfg.RemoteImageEndpoint = kubeCfg.RemoteRuntimeEndpoint
		}
	}

	// TODO: These need to become arguments to a standalone docker shim.
	binDir := kubeCfg.CNIBinDir
	if binDir == "" {
		binDir = kubeCfg.NetworkPluginDir
	}
	pluginSettings := dockershim.NetworkPluginSettings{
		HairpinMode:       klet.hairpinMode,
		NonMasqueradeCIDR: klet.nonMasqueradeCIDR,
		PluginName:        kubeCfg.NetworkPluginName,
		PluginConfDir:     kubeCfg.CNIConfDir,
		PluginBinDir:      binDir,
		MTU:               int(kubeCfg.NetworkPluginMTU),
	}

	// Remote runtime shim just cannot talk back to kubelet, so it doesn't
	// support bandwidth shaping or hostports till #35457. To enable legacy
	// features, replace with networkHost.
	var nl *noOpLegacyHost
	pluginSettings.LegacyRuntimeHost = nl

	// 检测是否使用CRI接口与runtime进行交互，如果不则使用dockermanger
	if kubeCfg.EnableCRI {
		// kubelet defers to the runtime shim to setup networking. Setting
		// this to nil will prevent it from trying to invoke the plugin.
		// It's easier to always probe and initialize plugins till cri
		// becomes the default.
		klet.networkPlugin = nil

		var runtimeService internalApi.RuntimeService
		var imageService internalApi.ImageManagerService
		var err error

		switch kubeCfg.ContainerRuntime {
		case "docker":
			streamingConfig := getStreamingConfig(kubeCfg, kubeDeps)
			// Use the new CRI shim for docker.
			ds, err := dockershim.NewDockerService(klet.dockerClient, kubeCfg.SeccompProfileRoot, kubeCfg.PodInfraContainerImage, streamingConfig, &pluginSettings, kubeCfg.RuntimeCgroups)
			if err != nil {
				return nil, err
			}
			// TODO: Once we switch to grpc completely, we should move this
			// call to the grpc server start.
			if err := ds.Start(); err != nil {
				return nil, err
			}

			klet.criHandler = ds
			rs := ds.(internalApi.RuntimeService)
			is := ds.(internalApi.ImageManagerService)
			// This is an internal knob to switch between grpc and non-grpc
			// integration.
			// TODO: Remove this knob once we switch to using GRPC completely.
			overGRPC := true
			if overGRPC {
				const (
					// The unix socket for kubelet <-> dockershim communication.
					ep = "/var/run/dockershim.sock"
				)
				kubeCfg.RemoteRuntimeEndpoint = ep
				kubeCfg.RemoteImageEndpoint = ep

				server := dockerremote.NewDockerServer(ep, ds)
				glog.V(2).Infof("Starting the GRPC server for the docker CRI shim.")
				err := server.Start()
				if err != nil {
					return nil, err
				}
				rs, is, err = getRuntimeAndImageServices(kubeCfg)
				if err != nil {
					return nil, err
				}
			}
			// Use DockerLegacyService directly to work around unimplemented
			// functions in CRI.
			// TODO: Remove this hack after CRI is fully implemented.
			// TODO: Move the instrumented interface wrapping into kuberuntime.
			runtimeService = kuberuntime.NewInstrumentedRuntimeService(rs)
			imageService = is
			// 如果支持其他的runtime，新建两个service，CRI的protocol buffers API包含了两个gRPC服务：ImageService和RuntimeService
		case "remote":
			// ImageService提供了从镜像仓库拉取、查看、和移除镜像的RPC。
			// RuntimeSerivce包含了Pods和容器生命周期管理的RPC，以及跟容器交互的调用(exec/attach/port-forward)。
			runtimeService, imageService, err = getRuntimeAndImageServices(kubeCfg)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported CRI runtime: %q", kubeCfg.ContainerRuntime)
		}
		//runtime：容器运行时，对容器引擎（docker 或者 rkt）的一层封装，
		//负责调用容器引擎接口管理容器的状态，比如启动、暂停、杀死容器等
		runtime, err := kuberuntime.NewKubeGenericRuntimeManager(
			kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
			klet.livenessManager,
			containerRefManager,
			machineInfo,
			klet.podManager,
			kubeDeps.OSInterface,
			klet.networkPlugin,
			klet,
			klet.httpClient,
			imageBackOff,
			kubeCfg.SerializeImagePulls,
			float32(kubeCfg.RegistryPullQPS),
			int(kubeCfg.RegistryBurst),
			klet.cpuCFSQuota,
			runtimeService,
			kuberuntime.NewInstrumentedImageManagerService(imageService),
		)
		if err != nil {
			return nil, err
		}
		klet.containerRuntime = runtime
		klet.runner = runtime
		// 如果不同CRI则查看是使用docker还是rkt
	} else {
		switch kubeCfg.ContainerRuntime {
		case "docker":
			runtime := dockertools.NewDockerManager(
				kubeDeps.DockerClient,
				kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
				klet.livenessManager,
				containerRefManager,
				klet.podManager,
				machineInfo,
				kubeCfg.PodInfraContainerImage,
				float32(kubeCfg.RegistryPullQPS),
				int(kubeCfg.RegistryBurst),
				ContainerLogsDir,
				kubeDeps.OSInterface,
				klet.networkPlugin,
				klet,
				klet.httpClient,
				dockerExecHandler,
				kubeDeps.OOMAdjuster,
				procFs,
				klet.cpuCFSQuota,
				imageBackOff,
				kubeCfg.SerializeImagePulls,
				kubeCfg.EnableCustomMetrics,
				// If using "kubenet", the Kubernetes network plugin that wraps
				// CNI's bridge plugin, it knows how to set the hairpin veth flag
				// so we tell the container runtime to back away from setting it.
				// If the kubelet is started with any other plugin we can't be
				// sure it handles the hairpin case so we instruct the docker
				// runtime to set the flag instead.
				klet.hairpinMode == componentconfig.HairpinVeth && kubeCfg.NetworkPluginName != "kubenet",
				kubeCfg.SeccompProfileRoot,
				kubeDeps.ContainerRuntimeOptions...,
			)
			klet.containerRuntime = runtime
			klet.runner = kubecontainer.DirectStreamingRunner(runtime)
		case "rkt":
			// TODO: Include hairpin mode settings in rkt?
			conf := &rkt.Config{
				Path:            kubeCfg.RktPath,
				Stage1Image:     kubeCfg.RktStage1Image,
				InsecureOptions: "image,ondisk",
			}
			runtime, err := rkt.New(
				kubeCfg.RktAPIEndpoint,
				conf,
				klet,
				kubeDeps.Recorder,
				containerRefManager,
				klet.podManager,
				klet.livenessManager,
				klet.httpClient,
				klet.networkPlugin,
				klet.hairpinMode == componentconfig.HairpinVeth,
				utilexec.New(),
				kubecontainer.RealOS{},
				imageBackOff,
				kubeCfg.SerializeImagePulls,
				float32(kubeCfg.RegistryPullQPS),
				int(kubeCfg.RegistryBurst),
				kubeCfg.RuntimeRequestTimeout.Duration,
			)
			if err != nil {
				return nil, err
			}
			klet.containerRuntime = runtime
			klet.runner = kubecontainer.DirectStreamingRunner(runtime)
		default:
			return nil, fmt.Errorf("unsupported container runtime %q specified", kubeCfg.ContainerRuntime)
		}
	}

	// TODO: Factor out "StatsProvider" from Kubelet so we don't have a cyclic dependency
	klet.resourceAnalyzer = stats.NewResourceAnalyzer(klet, kubeCfg.VolumeStatsAggPeriod.Duration, klet.containerRuntime)

	klet.pleg = pleg.NewGenericPLEG(klet.containerRuntime, plegChannelCapacity, plegRelistPeriod, klet.podCache, clock.RealClock{})
	klet.runtimeState = newRuntimeState(maxWaitForContainerRuntime)
	klet.runtimeState.addHealthCheck("PLEG", klet.pleg.Healthy)
	klet.updatePodCIDR(kubeCfg.PodCIDR)

	// setup containerGC
	// 创建 containerGC 垃圾回收对象，进行周期性的容器清理工作，containerGCPolicy是前面初始化的回收策略
	containerGC, err := kubecontainer.NewContainerGC(klet.containerRuntime, containerGCPolicy)
	if err != nil {
		return nil, err
	}
	klet.containerGC = containerGC
	klet.containerDeletor = newPodContainerDeletor(klet.containerRuntime, integer.IntMax(containerGCPolicy.MaxPerPodContainer, minDeadContainerInPod))

	// setup imageManager
	// 5、创建 imageManager 对象，管理镜像
	// kubeDeps.Recorder说明这个manager可以写事件
	imageManager, err := images.NewImageGCManager(klet.containerRuntime, kubeDeps.CAdvisorInterface, kubeDeps.Recorder, nodeRef, imageGCPolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize image manager: %v", err)
	}
	klet.imageManager = imageManager

	// 6、statusManager 实时检测节点上 pod 的状态，并更新到 apiserver 对应的 pod
	//statusManager 负责维护状态信息，并把 pod 状态更新到 apiserver，
	//但是它并不负责监控 pod 状态的变化，而是提供对应的接口供其他组件调用，比如 probeManager
	klet.statusManager = status.NewManager(kubeClient, klet.podManager)

	// 7、probeManager 检测 pod 的状态，并通过 statusManager 进行更新
	// 那么 probeManager 会定时检查 pod 是否正常工作，
	// 并通过 statusManager 向 apiserver 更新 pod 的状态
	// probeManager 会定时去监控 pod 中容器的健康状况，
	// 一旦发现状态发生变化，就调用 statusManager 提供的方法更新 pod 的状态。
	// kubeDeps.Recorder可以用于写事件
	// 例：如果节点的pod发生健康状况改变或者容器发生变化，既要更新apiserver的pod的状态又要
	// 将相关的Events给apiserver以便差错
	klet.probeManager = prober.NewManager(
		klet.statusManager,
		klet.livenessManager,
		klet.runner,
		containerRefManager,
		kubeDeps.Recorder)

	// 8、volumeManager 管理节点上 volume；检测某个 volume 是否已经 mount、获取 pod 使用的 volume 等
	klet.volumePluginMgr, err =
		NewInitializedVolumePluginMgr(klet, kubeDeps.VolumePlugins)
	if err != nil {
		return nil, err
	}

	// If the experimentalMounterPathFlag is set, we do not want to
	// check node capabilities since the mount path is not the default
	if len(kubeCfg.ExperimentalMounterPath) != 0 {
		kubeCfg.ExperimentalCheckNodeCapabilitiesBeforeMount = false
	}
	// setup volumeManager
	// setup volumeManager
	//kubeDeps.Recorder，说明该manager可以写消息
	klet.volumeManager, err = volumemanager.NewVolumeManager(
		kubeCfg.EnableControllerAttachDetach,
		nodeName,
		klet.podManager,
		klet.kubeClient,
		klet.volumePluginMgr,
		klet.containerRuntime,
		kubeDeps.Mounter,
		klet.getPodsDir(),
		kubeDeps.Recorder,
		kubeCfg.ExperimentalCheckNodeCapabilitiesBeforeMount)

	// 保存了节点上正在运行的 pod 信息
	runtimeCache, err := kubecontainer.NewRuntimeCache(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	klet.runtimeCache = runtimeCache
	klet.reasonCache = NewReasonCache()
	klet.workQueue = queue.NewBasicWorkQueue(klet.clock)

	// 9、podWorkers 是具体的执行者，每次有 pod 需要更新的时候都会发送给它
	klet.podWorkers = newPodWorkers(klet.syncPod, kubeDeps.Recorder, klet.workQueue, klet.resyncInterval, backOffPeriod, klet.podCache)

	klet.backOff = flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)
	klet.podKillingCh = make(chan *kubecontainer.PodPair, podKillingChannelCapacity)
	klet.setNodeStatusFuncs = klet.defaultNodeStatusFuncs()

	// setup eviction manager
	evictionManager, evictionAdmitHandler, err := eviction.NewManager(klet.resourceAnalyzer, evictionConfig, killPodNow(klet.podWorkers, kubeDeps.Recorder), klet.imageManager, kubeDeps.Recorder, nodeRef, klet.clock)

	if err != nil {
		return nil, fmt.Errorf("failed to initialize eviction manager: %v", err)
	}
	klet.evictionManager = evictionManager
	klet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	// add sysctl admission
	runtimeSupport, err := sysctl.NewRuntimeAdmitHandler(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	safeWhitelist, err := sysctl.NewWhitelist(sysctl.SafeSysctlWhitelist(), api.SysctlsPodAnnotationKey)
	if err != nil {
		return nil, err
	}
	// Safe, whitelisted sysctls can always be used as unsafe sysctls in the spec
	// Hence, we concatenate those two lists.
	safeAndUnsafeSysctls := append(sysctl.SafeSysctlWhitelist(), kubeCfg.AllowedUnsafeSysctls...)
	unsafeWhitelist, err := sysctl.NewWhitelist(safeAndUnsafeSysctls, api.UnsafeSysctlsPodAnnotationKey)
	if err != nil {
		return nil, err
	}
	klet.admitHandlers.AddPodAdmitHandler(runtimeSupport)
	klet.admitHandlers.AddPodAdmitHandler(safeWhitelist)
	klet.admitHandlers.AddPodAdmitHandler(unsafeWhitelist)

	// enable active deadline handler
	activeDeadlineHandler, err := newActiveDeadlineHandler(klet.statusManager, kubeDeps.Recorder, klet.clock)
	if err != nil {
		return nil, err
	}
	klet.AddPodSyncLoopHandler(activeDeadlineHandler)
	klet.AddPodSyncHandler(activeDeadlineHandler)

	klet.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(klet.getNodeAnyWay))
	// apply functional Option's
	for _, opt := range kubeDeps.Options {
		opt(klet)
	}

	klet.appArmorValidator = apparmor.NewValidator(kubeCfg.ContainerRuntime)
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator))

	// Finally, put the most recent version of the config on the Kubelet, so
	// people can see how it was configured.
	klet.kubeletConfiguration = *kubeCfg
	return klet, nil
}

type serviceLister interface {
	List(labels.Selector) ([]*api.Service, error)
}

type nodeLister interface {
	List() (machines api.NodeList, err error)
}

// Kubelet is the main kubelet implementation.
// kubelet运行实例结构体
type Kubelet struct {
	kubeletConfiguration componentconfig.KubeletConfiguration

	hostname string
	nodeName types.NodeName
	//与docker进行交互
	dockerClient dockertools.DockerInterface
	runtimeCache kubecontainer.RuntimeCache
	//与apiServer进行交互
	kubeClient    clientset.Interface
	iptClient     utilipt.Interface
	rootDirectory string

	// podWorkers handle syncing Pods in response to events.
	// 处理updates中的对pod的操作
	podWorkers PodWorkers

	// resyncInterval is the interval between periodic full reconciliations of
	// pods on this node.
	resyncInterval time.Duration

	// sourcesReady records the sources seen by the kubelet, it is thread-safe.
	sourcesReady config.SourcesReady

	// podManager is a facade that abstracts away the various sources of pods
	// this Kubelet services.
	podManager kubepod.Manager

	// Needed to observe and respond to situations that could impact node stability
	evictionManager eviction.Manager

	// Needed to report events for containers belonging to deleted/modified pods.
	// Tracks references for reporting events
	containerRefManager *kubecontainer.RefManager

	// Optional, defaults to /logs/ from /var/log
	logServer http.Handler
	// Optional, defaults to simple Docker implementation
	runner kubecontainer.ContainerCommandRunner
	// Optional, client for http requests, defaults to empty client
	httpClient kubetypes.HttpGetter

	// cAdvisor used for container information.
	// 监控容器信息
	cadvisor cadvisor.Interface

	// Set to true to have the node register itself with the apiserver.
	registerNode bool
	// Set to true to have the node register itself as schedulable.
	registerSchedulable bool
	// for internal book keeping; access only from within registerWithApiserver
	registrationCompleted bool

	// Set to true if the kubelet is in standalone mode (i.e. setup without an apiserver)
	standaloneMode bool

	// If non-empty, use this for container DNS search.
	clusterDomain string

	// If non-nil, use this for container DNS server.
	clusterDNS net.IP

	// masterServiceNamespace is the namespace that the master service is exposed in.
	masterServiceNamespace string
	// serviceLister knows how to list services
	//listWatch监听service信息
	serviceLister serviceLister
	// nodeLister knows how to list nodes
	//listwatch监听node信息
	nodeLister nodeLister
	// nodeInfo knows how to get information about the node for this kubelet.
	nodeInfo predicates.NodeInfo

	// a list of node labels to register
	nodeLabels map[string]string

	// Last timestamp when runtime responded on ping.
	// Mutex is used to protect this value.
	runtimeState *runtimeState

	// Volume plugins.
	volumePluginMgr *volume.VolumePluginMgr

	// Network plugin.
	networkPlugin network.NetworkPlugin

	// Handles container probing.
	probeManager prober.Manager
	// Manages container health check results.
	livenessManager proberesults.Manager

	// How long to keep idle streaming command execution/port forwarding
	// connections open before terminating them
	streamingConnectionIdleTimeout time.Duration

	// The EventRecorder to use
	recorder record.EventRecorder

	// Policy for handling garbage collection of dead containers.
	containerGC kubecontainer.ContainerGC

	// Manager for image garbage collection.
	//主要是镜像垃圾回收
	imageManager images.ImageGCManager

	// Diskspace manager.
	//磁盘空间管理
	diskSpaceManager diskSpaceManager

	// Cached MachineInfo returned by cadvisor.
	machineInfo *cadvisorapi.MachineInfo

	// Syncs pods statuses with apiserver; also used as a cache of statuses.
	statusManager status.Manager

	// VolumeManager runs a set of asynchronous loops that figure out which
	// volumes need to be attached/mounted/unmounted/detached based on the pods
	// scheduled on this node and makes it so.
	//volume管理
	volumeManager volumemanager.VolumeManager

	// Cloud provider interface.
	cloud                   cloudprovider.Interface
	autoDetectCloudProvider bool

	// Reference to this node.
	nodeRef *api.ObjectReference

	// Container runtime.
	containerRuntime kubecontainer.Runtime

	// reasonCache caches the failure reason of the last creation of all containers, which is
	// used for generating ContainerStatus.
	reasonCache *ReasonCache

	// nodeStatusUpdateFrequency specifies how often kubelet posts node status to master.
	// Note: be cautious when changing the constant, it must work with nodeMonitorGracePeriod
	// in nodecontroller. There are several constraints:
	// 1. nodeMonitorGracePeriod must be N times more than nodeStatusUpdateFrequency, where
	//    N means number of retries allowed for kubelet to post node status. It is pointless
	//    to make nodeMonitorGracePeriod be less than nodeStatusUpdateFrequency, since there
	//    will only be fresh values from Kubelet at an interval of nodeStatusUpdateFrequency.
	//    The constant must be less than podEvictionTimeout.
	// 2. nodeStatusUpdateFrequency needs to be large enough for kubelet to generate node
	//    status. Kubelet may fail to update node status reliably if the value is too small,
	//    as it takes time to gather all necessary node information.
	nodeStatusUpdateFrequency time.Duration

	// Generates pod events.
	pleg pleg.PodLifecycleEventGenerator

	// Store kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache

	// os is a facade for various syscalls that need to be mocked during testing.
	os kubecontainer.OSInterface

	// Watcher of out of memory events.
	oomWatcher OOMWatcher

	// Monitor resource usage
	resourceAnalyzer stats.ResourceAnalyzer

	// Whether or not we should have the QOS cgroup hierarchy for resource management
	cgroupsPerQOS bool

	// If non-empty, pass this to the container runtime as the root cgroup.
	cgroupRoot string

	// Mounter to use for volumes.
	mounter mount.Interface

	// Writer interface to use for volumes.
	writer kubeio.Writer

	// Manager of non-Runtime containers.
	containerManager cm.ContainerManager
	nodeConfig       cm.NodeConfig

	// Traffic to IPs outside this range will use IP masquerade.
	nonMasqueradeCIDR string

	// Maximum Number of Pods which can be run by this Kubelet
	maxPods int

	// Number of NVIDIA GPUs on this node
	nvidiaGPUs int

	// Monitor Kubelet's sync loop
	syncLoopMonitor atomic.Value

	// Container restart Backoff
	backOff *flowcontrol.Backoff

	// Channel for sending pods to kill.
	podKillingCh chan *kubecontainer.PodPair

	// The configuration file used as the base to generate the container's
	// DNS resolver configuration file. This can be used in conjunction with
	// clusterDomain and clusterDNS.
	resolverConfig string

	// Optionally shape the bandwidth of a pod
	// TODO: remove when kubenet plugin is ready
	shaper bandwidth.BandwidthShaper

	// True if container cpu limits should be enforced via cgroup CFS quota
	cpuCFSQuota bool

	// Information about the ports which are opened by daemons on Node running this Kubelet server.
	daemonEndpoints *api.NodeDaemonEndpoints

	// A queue used to trigger pod workers.
	workQueue queue.WorkQueue

	// oneTimeInitializer is used to initialize modules that are dependent on the runtime to be up.
	oneTimeInitializer sync.Once

	// If non-nil, use this IP address for the node
	nodeIP net.IP

	// clock is an interface that provides time related functionality in a way that makes it
	// easy to test the code.
	clock clock.Clock

	// outOfDiskTransitionFrequency specifies the amount of time the kubelet has to be actually
	// not out of disk before it can transition the node condition status from out-of-disk to
	// not-out-of-disk. This prevents a pod that causes out-of-disk condition from repeatedly
	// getting rescheduled onto the node.
	outOfDiskTransitionFrequency time.Duration

	// reservation specifies resources which are reserved for non-pod usage, including kubernetes and
	// non-kubernetes system processes.
	reservation kubetypes.Reservation

	// support gathering custom metrics.
	enableCustomMetrics bool

	// How the Kubelet should setup hairpin NAT. Can take the values: "promiscuous-bridge"
	// (make cbr0 promiscuous), "hairpin-veth" (set the hairpin flag on veth interfaces)
	// or "none" (do nothing).
	hairpinMode componentconfig.HairpinMode

	// The node has babysitter process monitoring docker and kubelet
	babysitDaemons bool

	// handlers called during the tryUpdateNodeStatus cycle
	setNodeStatusFuncs []func(*api.Node) error

	// TODO: think about moving this to be centralized in PodWorkers in follow-on.
	// the list of handlers to call during pod admission.
	admitHandlers lifecycle.PodAdmitHandlers

	// softAdmithandlers are applied to the pod after it is admitted by the Kubelet, but before it is
	// run. A pod rejected by a softAdmitHandler will be left in a Pending state indefinitely. If a
	// rejected pod should not be recreated, or the scheduler is not aware of the rejection rule, the
	// admission rule should be applied by a softAdmitHandler.
	softAdmitHandlers lifecycle.PodAdmitHandlers

	// the list of handlers to call during pod sync loop.
	lifecycle.PodSyncLoopHandlers

	// the list of handlers to call during pod sync.
	lifecycle.PodSyncHandlers

	// the number of allowed pods per core
	podsPerCore int

	// enableControllerAttachDetach indicates the Attach/Detach controller
	// should manage attachment/detachment of volumes scheduled to this node,
	// and disable kubelet from executing any attach/detach operations
	enableControllerAttachDetach bool

	// trigger deleting containers in a pod
	containerDeletor *podContainerDeletor

	// config iptables util rules
	makeIPTablesUtilChains bool

	// The bit of the fwmark space to mark packets for SNAT.
	iptablesMasqueradeBit int

	// The bit of the fwmark space to mark packets for dropping.
	iptablesDropBit int

	// The AppArmor validator for checking whether AppArmor is supported.
	appArmorValidator apparmor.Validator

	// The handler serving CRI streaming calls (exec/attach/port-forward).
	criHandler http.Handler

	// experimentalHostUserNamespaceDefaulting sets userns=true when users request host namespaces (pid, ipc, net),
	// are using non-namespaced capabilities (mknod, sys_time, sys_module), the pod contains a privileged container,
	// or using host path volumes.
	// This should only be enabled when the container runtime is performing user remapping AND if the
	// experimental behavior is desired.
	experimentalHostUserNamespaceDefaulting bool
}

// setupDataDirs creates:
// 1.  the root directory
// 2.  the pods directory
// 3.  the plugins directory
func (kl *Kubelet) setupDataDirs() error {
	kl.rootDirectory = path.Clean(kl.rootDirectory)
	if err := os.MkdirAll(kl.getRootDir(), 0750); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pods directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPluginsDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins directory: %v", err)
	}
	return nil
}

// Starts garbage collection threads.
// 对宿主机上的容器和镜像进行回收，防止占用过多的磁盘空间，docker本身不会对镜像和容器资源
// 进行性清理，所以kubelet进行控制清理，节省节点空间
// 由于容器和镜像资源不能盲目清理，所以有一定的清理规则
func (kl *Kubelet) StartGarbageCollection() {
	loggedContainerGCFailure := false
	//容器GC的Goroutine
	go wait.Until(func() {
		//调用GC函数
		if err := kl.containerGC.GarbageCollect(kl.sourcesReady.AllReady()); err != nil {
			glog.Errorf("Container garbage collection failed: %v", err)
			kl.recorder.Eventf(kl.nodeRef, api.EventTypeWarning, events.ContainerGCFailed, err.Error())
			loggedContainerGCFailure = true
		} else {
			var vLevel glog.Level = 4
			if loggedContainerGCFailure {
				vLevel = 1
				loggedContainerGCFailure = false
			}

			glog.V(vLevel).Infof("Container garbage collection succeeded")
		}
	}, ContainerGCPeriod, wait.NeverStop)

	loggedImageGCFailure := false
	//镜像GC的Goroutine
	go wait.Until(func() {
		if err := kl.imageManager.GarbageCollect(); err != nil {
			glog.Errorf("Image garbage collection failed: %v", err)
			kl.recorder.Eventf(kl.nodeRef, api.EventTypeWarning, events.ImageGCFailed, err.Error())
			loggedImageGCFailure = true
		} else {
			var vLevel glog.Level = 4
			if loggedImageGCFailure {
				vLevel = 1
				loggedImageGCFailure = false
			}

			glog.V(vLevel).Infof("Image garbage collection succeeded")
		}
	}, ImageGCPeriod, wait.NeverStop)
}

// initializeModules will initialize internal modules that do not require the container runtime to be up.
// Note that the modules here must not depend on modules that are not initialized here.
func (kl *Kubelet) initializeModules() error {
	// Step 1: Promethues metrics.
	metrics.Register(kl.runtimeCache)

	// Step 2: Setup filesystem directories.
	if err := kl.setupDataDirs(); err != nil {
		return err
	}

	// Step 3: If the container logs directory does not exist, create it.
	if _, err := os.Stat(ContainerLogsDir); err != nil {
		if err := kl.os.MkdirAll(ContainerLogsDir, 0755); err != nil {
			glog.Errorf("Failed to create directory %q: %v", ContainerLogsDir, err)
		}
	}

	// Step 4: Start the image manager.
	if err := kl.imageManager.Start(); err != nil {
		return fmt.Errorf("Failed to start ImageManager, images may not be garbage collected: %v", err)
	}

	// Step 5: Start container manager.
	node, err := kl.getNodeAnyWay()
	if err != nil {
		glog.Errorf("Cannot get Node info: %v", err)
		return fmt.Errorf("Kubelet failed to get node info.")
	}

	if err := kl.containerManager.Start(node); err != nil {
		return fmt.Errorf("Failed to start ContainerManager %v", err)
	}

	// Step 6: Start out of memory watcher.
	if err := kl.oomWatcher.Start(kl.nodeRef); err != nil {
		return fmt.Errorf("Failed to start OOM watcher %v", err)
	}

	// Step 7: Start resource analyzer
	kl.resourceAnalyzer.Start()

	return nil
}

// initializeRuntimeDependentModules will initialize internal modules that require the container runtime to be up.
func (kl *Kubelet) initializeRuntimeDependentModules() {
	if err := kl.cadvisor.Start(); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		// TODO(random-liu): Add backoff logic in the babysitter
		glog.Fatalf("Failed to start cAdvisor %v", err)
	}
	// eviction manager must start after cadvisor because it needs to know if the container runtime has a dedicated imagefs
	if err := kl.evictionManager.Start(kl, kl.getActivePods, evictionMonitoringPeriod); err != nil {
		kl.runtimeState.setInternalError(fmt.Errorf("failed to start eviction manager %v", err))
	}
}

// Run starts the kubelet reacting to config updates
// 参数是一个管道参数，会实时地发送过来 pod 最新的配置信息
// 会将各种组件的启动，每个组件都是以 goroutine 运行的，
func (kl *Kubelet) Run(updates <-chan kubetypes.PodUpdate) {
	if kl.logServer == nil {
		kl.logServer = http.StripPrefix("/logs/", http.FileServer(http.Dir("/var/log/")))
	}
	if kl.kubeClient == nil {
		glog.Warning("No api server defined - no node status update will be sent.")
	}
	if err := kl.initializeModules(); err != nil {
		kl.recorder.Eventf(kl.nodeRef, api.EventTypeWarning, events.KubeletSetupFailed, err.Error())
		glog.Error(err)
		kl.runtimeState.setInitError(err)
	}

	// Start volume manager
	go kl.volumeManager.Run(kl.sourcesReady, wait.NeverStop)

	// 定时向 apiserver 更新 node 信息，用作调度时的重要参考，kubeClient是用于和spiserver交互的
	if kl.kubeClient != nil {
		// Start syncing node status immediately, this may set up things the runtime needs to run.
		go wait.Until(kl.syncNodeStatus, kl.nodeStatusUpdateFrequency, wait.NeverStop)
	}
	go wait.Until(kl.syncNetworkStatus, 30*time.Second, wait.NeverStop)
	go wait.Until(kl.updateRuntimeUp, 5*time.Second, wait.NeverStop)

	// Start loop to sync iptables util rules
	if kl.makeIPTablesUtilChains {
		go wait.Until(kl.syncNetworkUtil, 1*time.Minute, wait.NeverStop)
	}

	// Start a goroutine responsible for killing pods (that are not properly
	// handled by pod workers).
	// 删除 podWorker 没有正常处理的 pod
	go wait.Until(kl.podKiller, 1*time.Second, wait.NeverStop)

	// Start component sync loops.
	// 管理 pod 和容器的状态
	// statusManager负责维护状态信息，并将pod状态更新到apiserver，但不负责监控pod
	// 状态变化，而是提供对应的接口共其他组件调用
	// probemanager负责定时监控pod中的容器健康状况，一旦发生变化，就调用statusManager
	// 提供的方法更新pod
	// 看一下这个方法
	kl.statusManager.Start()
	kl.probeManager.Start()

	// Start the pod lifecycle event generator.
	kl.pleg.Start()
	// kubectl的入口，是处理所有 pod 更新的主循环，
	// 获取 pod 的变化（新建、修改和删除），调用对应的处理函数保证节点上的容器符合 pod 的配置。
	// 分析这个函数
	// updates这个管道作为输入值
	// 为什么将kl作参数穿进去？直接用不就好了？？？？
	kl.syncLoop(updates, kl)
}

// GetKubeClient returns the Kubernetes client.
// TODO: This is currently only required by network plugins. Replace
// with more specific methods.
func (kl *Kubelet) GetKubeClient() clientset.Interface {
	return kl.kubeClient
}

// GetClusterDNS returns a list of the DNS servers and a list of the DNS search
// domains of the cluster.
func (kl *Kubelet) GetClusterDNS(pod *api.Pod) ([]string, []string, error) {
	var hostDNS, hostSearch []string
	// Get host DNS settings
	if kl.resolverConfig != "" {
		f, err := os.Open(kl.resolverConfig)
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()

		hostDNS, hostSearch, err = kl.parseResolvConf(f)
		if err != nil {
			return nil, nil, err
		}
	}
	useClusterFirstPolicy := pod.Spec.DNSPolicy == api.DNSClusterFirst
	if useClusterFirstPolicy && kl.clusterDNS == nil {
		// clusterDNS is not known.
		// pod with ClusterDNSFirst Policy cannot be created
		kl.recorder.Eventf(pod, api.EventTypeWarning, "MissingClusterDNS", "kubelet does not have ClusterDNS IP configured and cannot create Pod using %q policy. Falling back to DNSDefault policy.", pod.Spec.DNSPolicy)
		log := fmt.Sprintf("kubelet does not have ClusterDNS IP configured and cannot create Pod using %q policy. pod: %q. Falling back to DNSDefault policy.", pod.Spec.DNSPolicy, format.Pod(pod))
		kl.recorder.Eventf(kl.nodeRef, api.EventTypeWarning, "MissingClusterDNS", log)

		// fallback to DNSDefault
		useClusterFirstPolicy = false
	}

	if !useClusterFirstPolicy {
		// When the kubelet --resolv-conf flag is set to the empty string, use
		// DNS settings that override the docker default (which is to use
		// /etc/resolv.conf) and effectively disable DNS lookups. According to
		// the bind documentation, the behavior of the DNS client library when
		// "nameservers" are not specified is to "use the nameserver on the
		// local machine". A nameserver setting of localhost is equivalent to
		// this documented behavior.
		if kl.resolverConfig == "" {
			hostDNS = []string{"127.0.0.1"}
			hostSearch = []string{"."}
		}
		return hostDNS, hostSearch, nil
	}

	// for a pod with DNSClusterFirst policy, the cluster DNS server is the only nameserver configured for
	// the pod. The cluster DNS server itself will forward queries to other nameservers that is configured to use,
	// in case the cluster DNS server cannot resolve the DNS query itself
	dns := []string{kl.clusterDNS.String()}

	var dnsSearch []string
	if kl.clusterDomain != "" {
		nsSvcDomain := fmt.Sprintf("%s.svc.%s", pod.Namespace, kl.clusterDomain)
		svcDomain := fmt.Sprintf("svc.%s", kl.clusterDomain)
		dnsSearch = append([]string{nsSvcDomain, svcDomain, kl.clusterDomain}, hostSearch...)
	} else {
		dnsSearch = hostSearch
	}
	return dns, dnsSearch, nil
}

// syncPod is the transaction script for the sync of a single pod.
//
// Arguments:
//
// o - the SyncPodOptions for this invocation
//
// The workflow is:
// * If the pod is being created, record pod worker start latency
// * Call generateAPIPodStatus to prepare an api.PodStatus for the pod
// * If the pod is being seen as running for the first time, record pod
//   start latency
// * Update the status of the pod in the status manager
// * Kill the pod if it should not be running
// * Create a mirror pod if the pod is a static pod, and does not
//   already have a mirror pod
// * Create the data directories for the pod if they do not exist
// * Wait for volumes to attach/mount
// * Fetch the pull secrets for the pod
// * Call the container runtime's SyncPod callback
// * Update the traffic shaping for the pod's ingress and egress limits
//
// If any step if this workflow errors, the error is returned, and is repeated
// on the next syncPod call.
func (kl *Kubelet) syncPod(o syncPodOptions) error {
	// pull out the required options
	pod := o.pod
	mirrorPod := o.mirrorPod
	podStatus := o.podStatus
	updateType := o.updateType

	// if we want to kill a pod, do it now!
	//如果是删除 pod，立即执行并返回
	if updateType == kubetypes.SyncPodKill {
		killPodOptions := o.killPodOptions
		if killPodOptions == nil || killPodOptions.PodStatusFunc == nil {
			return fmt.Errorf("kill pod options are required if update type is kill")
		}
		apiPodStatus := killPodOptions.PodStatusFunc(pod, podStatus)
		kl.statusManager.SetPodStatus(pod, apiPodStatus)
		// we kill the pod with the specified grace period since this is a termination
		if err := kl.killPod(pod, nil, podStatus, killPodOptions.PodTerminationGracePeriodSecondsOverride); err != nil {
			// there was an error killing the pod, so we return that error directly
			utilruntime.HandleError(err)
			return err
		}
		return nil
	}

	// Latency measurements for the main workflow are relative to the
	// first time the pod was seen by the API server.
	var firstSeenTime time.Time
	if firstSeenTimeStr, ok := pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]; ok {
		firstSeenTime = kubetypes.ConvertToTimestamp(firstSeenTimeStr).Get()
	}

	// Record pod worker start latency if being created
	// TODO: make pod workers record their own latencies
	if updateType == kubetypes.SyncPodCreate {
		if !firstSeenTime.IsZero() {
			// This is the first time we are syncing the pod. Record the latency
			// since kubelet first saw the pod if firstSeenTime is set.
			metrics.PodWorkerStartLatency.Observe(metrics.SinceInMicroseconds(firstSeenTime))
		} else {
			glog.V(3).Infof("First seen time not recorded for pod %q", pod.UID)
		}
	}

	// Generate final API pod status with pod and status manager status
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus)
	// The pod IP may be changed in generateAPIPodStatus if the pod is using host network. (See #24576)
	// TODO(random-liu): After writing pod spec into container labels, check whether pod is using host network, and
	// set pod IP to hostIP directly in runtime.GetPodStatus
	podStatus.IP = apiPodStatus.PodIP

	// Record the time it takes for the pod to become running.
	existingStatus, ok := kl.statusManager.GetPodStatus(pod.UID)
	if !ok || existingStatus.Phase == api.PodPending && apiPodStatus.Phase == api.PodRunning &&
		!firstSeenTime.IsZero() {
		metrics.PodStartLatency.Observe(metrics.SinceInMicroseconds(firstSeenTime))
	}

	//检查 pod 是否能运行在本节点，
	//主要是权限检查（是否能使用主机网络模式，是否可以以 privileged 权限运行等）。
	//如果没有权限，就删除本地旧的 pod 并返回错误信息
	runnable := kl.canRunPod(pod)
	if !runnable.Admit {
		// Pod is not runnable; update the Pod and Container statuses to why.
		apiPodStatus.Reason = runnable.Reason
		apiPodStatus.Message = runnable.Message
		// Waiting containers are not creating.
		const waitingReason = "Blocked"
		for _, cs := range apiPodStatus.InitContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
		for _, cs := range apiPodStatus.ContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
	}

	// Update status in the status manager
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// Kill pod if it should not be running
	if !runnable.Admit || pod.DeletionTimestamp != nil || apiPodStatus.Phase == api.PodFailed {
		var syncErr error
		if err := kl.killPod(pod, nil, podStatus, nil); err != nil {
			syncErr = fmt.Errorf("error killing pod: %v", err)
			utilruntime.HandleError(syncErr)
		} else {
			if !runnable.Admit {
				// There was no error killing the pod, but the pod cannot be run.
				// Return an error to signal that the sync loop should back off.
				syncErr = fmt.Errorf("pod cannot be run: %s", runnable.Message)
			}
		}
		return syncErr
	}

	// If the network plugin is not ready, only start the pod if it uses the host network
	if rs := kl.runtimeState.networkErrors(); len(rs) != 0 && !podUsesHostNetwork(pod) {
		return fmt.Errorf("network is not ready: %v", rs)
	}

	// Create Cgroups for the pod and apply resource parameters
	// to them if cgroup-per-qos flag is enabled.
	pcm := kl.containerManager.NewPodContainerManager()
	// If pod has already been terminated then we need not create
	// or update the pod's cgroup
	if !kl.podIsTerminated(pod) {
		// When the kubelet is restarted with the cgroup-per-qos
		// flag enabled, all the pod's running containers
		// should be killed intermittently and brought back up
		// under the qos cgroup hierarchy.
		// Check if this is the pod's first sync
		firstSync := true
		for _, containerStatus := range apiPodStatus.ContainerStatuses {
			if containerStatus.State.Running != nil {
				firstSync = false
				break
			}
		}
		// Don't kill containers in pod if pod's cgroups already
		// exists or the pod is running for the first time
		podKilled := false
		if !pcm.Exists(pod) && !firstSync {
			kl.killPod(pod, nil, podStatus, nil)
			podKilled = true
		}
		// Create and Update pod's Cgroups
		// Don't create cgroups for run once pod if it was killed above
		// The current policy is not to restart the run once pods when
		// the kubelet is restarted with the new flag as run once pods are
		// expected to run only once and if the kubelet is restarted then
		// they are not expected to run again.
		// We don't create and apply updates to cgroup if its a run once pod and was killed above
		if !(podKilled && pod.Spec.RestartPolicy == api.RestartPolicyNever) {
			if err := pcm.EnsureExists(pod); err != nil {
				return fmt.Errorf("failed to ensure that the pod: %v cgroups exist and are correctly applied: %v", pod.UID, err)
			}
		}
	}

	// Create Mirror Pod for Static Pod if it doesn't already exist
	//如果是 static Pod，就创建或者更新对应的 mirrorPod
	if kubepod.IsStaticPod(pod) {
		podFullName := kubecontainer.GetPodFullName(pod)
		deleted := false
		if mirrorPod != nil {
			if mirrorPod.DeletionTimestamp != nil || !kl.podManager.IsMirrorPodOf(mirrorPod, pod) {
				// The mirror pod is semantically different from the static pod. Remove
				// it. The mirror pod will get recreated later.
				glog.Warningf("Deleting mirror pod %q because it is outdated", format.Pod(mirrorPod))
				if err := kl.podManager.DeleteMirrorPod(podFullName); err != nil {
					glog.Errorf("Failed deleting mirror pod %q: %v", format.Pod(mirrorPod), err)
				} else {
					deleted = true
				}
			}
		}
		if mirrorPod == nil || deleted {
			glog.V(3).Infof("Creating a mirror pod for static pod %q", format.Pod(pod))
			if err := kl.podManager.CreateMirrorPod(pod); err != nil {
				glog.Errorf("Failed creating a mirror pod for %q: %v", format.Pod(pod), err)
			}
		}
	}

	// Make data directories for the pod
	// 创建 pod 的数据目录，存放 volume 和 plugin 信息
	if err := kl.makePodDataDirs(pod); err != nil {
		glog.Errorf("Unable to make pod data directories for pod %q: %v", format.Pod(pod), err)
		return err
	}

	// Wait for volumes to attach/mount
	// 如果定义了 PV，等待所有的 volume mount 完成（volumeManager 会在后台做这些事情）
	if err := kl.volumeManager.WaitForAttachAndMount(pod); err != nil {
		kl.recorder.Eventf(pod, api.EventTypeWarning, events.FailedMountVolume, "Unable to mount volumes for pod %q: %v", format.Pod(pod), err)
		glog.Errorf("Unable to mount volumes for pod %q: %v; skipping pod", format.Pod(pod), err)
		return err
	}

	//如果有 image secrets，去 apiserver 获取对应的 secrets 数据
	// Fetch the pull secrets for the pod
	pullSecrets, err := kl.getPullSecretsForPod(pod)
	if err != nil {
		glog.Errorf("Unable to get pull secrets for pod %q: %v", format.Pod(pod), err)
		return err
	}

	// Call the container runtime's SyncPod callback
	//调用 container runtime 的 SyncPod 方法，去实现真正的容器创建逻辑
	//这里是真正创建容器，前面在做准备工作，这里调用 runtime 执行具体容器的创建
	//如果是docker的话就是调用(dm *DockerManager) SyncPod
	//代码位置：pkg/kubelet/dockertools/docker_manager.go
	//看这里的代码
	result := kl.containerRuntime.SyncPod(pod, apiPodStatus, podStatus, pullSecrets, kl.backOff)
	kl.reasonCache.Update(pod.UID, result)
	if err = result.Error(); err != nil {
		return err
	}

	// early successful exit if pod is not bandwidth-constrained
	if !kl.shapingEnabled() {
		return nil
	}

	// Update the traffic shaping for the pod's ingress and egress limits
	ingress, egress, err := bandwidth.ExtractPodBandwidthResources(pod.Annotations)
	if err != nil {
		return err
	}
	if egress != nil || ingress != nil {
		if podUsesHostNetwork(pod) {
			kl.recorder.Event(pod, api.EventTypeWarning, events.HostNetworkNotSupported, "Bandwidth shaping is not currently supported on the host network")
		} else if kl.shaper != nil {
			if len(apiPodStatus.PodIP) > 0 {
				err = kl.shaper.ReconcileCIDR(fmt.Sprintf("%s/32", apiPodStatus.PodIP), egress, ingress)
			}
		} else {
			kl.recorder.Event(pod, api.EventTypeWarning, events.UndefinedShaper, "Pod requests bandwidth shaping, but the shaper is undefined")
		}
	}

	return nil
}

// Get pods which should be resynchronized. Currently, the following pod should be resynchronized:
//   * pod whose work is ready.
//   * internal modules that request sync of a pod.
func (kl *Kubelet) getPodsToSync() []*api.Pod {
	allPods := kl.podManager.GetPods()
	podUIDs := kl.workQueue.GetWork()
	podUIDSet := sets.NewString()
	for _, podUID := range podUIDs {
		podUIDSet.Insert(string(podUID))
	}
	var podsToSync []*api.Pod
	for _, pod := range allPods {
		if podUIDSet.Has(string(pod.UID)) {
			// The work of the pod is ready
			podsToSync = append(podsToSync, pod)
			continue
		}
		for _, podSyncLoopHandler := range kl.PodSyncLoopHandlers {
			if podSyncLoopHandler.ShouldSync(pod) {
				podsToSync = append(podsToSync, pod)
				break
			}
		}
	}
	return podsToSync
}

// deletePod deletes the pod from the internal state of the kubelet by:
// 1.  stopping the associated pod worker asynchronously
// 2.  signaling to kill the pod by sending on the podKillingCh channel
//
// deletePod returns an error if not all sources are ready or the pod is not
// found in the runtime cache.
func (kl *Kubelet) deletePod(pod *api.Pod) error {
	if pod == nil {
		return fmt.Errorf("deletePod does not allow nil pod")
	}
	if !kl.sourcesReady.AllReady() {
		// If the sources aren't ready, skip deletion, as we may accidentally delete pods
		// for sources that haven't reported yet.
		return fmt.Errorf("skipping delete because sources aren't ready yet")
	}
	kl.podWorkers.ForgetWorker(pod.UID)

	// Runtime cache may not have been updated to with the pod, but it's okay
	// because the periodic cleanup routine will attempt to delete again later.
	runningPods, err := kl.runtimeCache.GetPods()
	if err != nil {
		return fmt.Errorf("error listing containers: %v", err)
	}
	runningPod := kubecontainer.Pods(runningPods).FindPod("", pod.UID)
	if runningPod.IsEmpty() {
		return fmt.Errorf("pod not found")
	}
	podPair := kubecontainer.PodPair{APIPod: pod, RunningPod: &runningPod}

	kl.podKillingCh <- &podPair
	// TODO: delete the mirror pod here?

	// We leave the volume/directory cleanup to the periodic cleanup routine.
	return nil
}

// handleOutOfDisk detects if pods can't fit due to lack of disk space.
func (kl *Kubelet) isOutOfDisk() bool {
	// Check disk space once globally and reject or accept all new pods.
	withinBounds, err := kl.diskSpaceManager.IsRuntimeDiskSpaceAvailable()
	// Assume enough space in case of errors.
	if err != nil {
		glog.Errorf("Failed to check if disk space is available for the runtime: %v", err)
	} else if !withinBounds {
		return true
	}

	withinBounds, err = kl.diskSpaceManager.IsRootDiskSpaceAvailable()
	// Assume enough space in case of errors.
	if err != nil {
		glog.Errorf("Failed to check if disk space is available on the root partition: %v", err)
	} else if !withinBounds {
		return true
	}
	return false
}

// rejectPod records an event about the pod with the given reason and message,
// and updates the pod to the failed phase in the status manage.
func (kl *Kubelet) rejectPod(pod *api.Pod, reason, message string) {
	kl.recorder.Eventf(pod, api.EventTypeWarning, reason, message)
	kl.statusManager.SetPodStatus(pod, api.PodStatus{
		Phase:   api.PodFailed,
		Reason:  reason,
		Message: "Pod " + message})
}

// canAdmitPod determines if a pod can be admitted, and gives a reason if it
// cannot. "pod" is new pod, while "pods" are all admitted pods
// The function returns a boolean value indicating whether the pod
// can be admitted, a brief single-word reason and a message explaining why
// the pod cannot be admitted.
func (kl *Kubelet) canAdmitPod(pods []*api.Pod, pod *api.Pod) (bool, string, string) {
	// the kubelet will invoke each pod admit handler in sequence
	// if any handler rejects, the pod is rejected.
	// TODO: move out of disk check into a pod admitter
	// TODO: out of resource eviction should have a pod admitter call-out
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod, OtherPods: pods}
	//如果有podAdmitHandler拒绝那么就拒绝pod，并且返回拒绝理由
	for _, podAdmitHandler := range kl.admitHandlers {
		if result := podAdmitHandler.Admit(attrs); !result.Admit {
			return false, result.Reason, result.Message
		}
	}
	// TODO: When disk space scheduling is implemented (#11976), remove the out-of-disk check here and
	// add the disk space predicate to predicates.GeneralPredicates.
	if kl.isOutOfDisk() {
		glog.Warningf("Failed to admit pod %v - %s", format.Pod(pod), "predicate fails due to OutOfDisk")
		return false, "OutOfDisk", "cannot be started due to lack of disk space."
	}

	return true, "", ""
}

func (kl *Kubelet) canRunPod(pod *api.Pod) lifecycle.PodAdmitResult {
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod}
	// Get "OtherPods". Rejected pods are failed, so only include admitted pods that are alive.
	attrs.OtherPods = kl.filterOutTerminatedPods(kl.podManager.GetPods())

	for _, handler := range kl.softAdmitHandlers {
		if result := handler.Admit(attrs); !result.Admit {
			return result
		}
	}

	// TODO: Refactor as a soft admit handler.
	if err := canRunPod(pod); err != nil {
		return lifecycle.PodAdmitResult{
			Admit:   false,
			Reason:  "Forbidden",
			Message: err.Error(),
		}
	}

	return lifecycle.PodAdmitResult{Admit: true}
}

// syncLoop is the main loop for processing changes. It watches for changes from
// three channels (file, apiserver, and http) and creates a union of them. For
// any new change seen, will run a sync against desired state and running state. If
// no changes are seen to the configuration, will synchronize the last known desired
// state every sync-frequency seconds. Never returns.

//是 kubelet 的主循环方法，它从不同的管道（文件、URL 和 apiserver）监听变化，并把它们汇聚起来。
//这些资源监听的变化都写进了updates中
//当有新的变化发生时，它会调用对应的处理函数，保证 pod 处于期望的状态。
//如果 pod 没有变化，它也会定期保证所有的容器和最新的期望状态保持一致。
//这个方法是 for 循环，不会退出。
func (kl *Kubelet) syncLoop(updates <-chan kubetypes.PodUpdate, handler SyncHandler) {
	glog.Info("Starting kubelet main sync loop.")
	// The resyncTicker wakes up kubelet to checks if there are any pod workers
	// that need to be sync'd. A one-second period is sufficient because the
	// sync interval is defaulted to 10s.

	// 创建两个定时器resyncTicker和housekeepingTicker
	// 即使没有需要更新的 pod 配置，kubelet 也会定时去做同步和清理工作。
	// 如果在每次循环过程中出现比较严重的错误，kubelet 会记录到runtimeState中
	syncTicker := time.NewTicker(time.Second)
	defer syncTicker.Stop()
	housekeepingTicker := time.NewTicker(housekeepingPeriod)
	defer housekeepingTicker.Stop()
	plegCh := kl.pleg.Watch()
	//for循环不会退出，反复执行
	for {
		//遇到错误就等待 5 秒中继续循环。
		if rs := kl.runtimeState.runtimeErrors(); len(rs) != 0 {
			glog.Infof("skipping pod synchronization - %v", rs)
			time.Sleep(5 * time.Second)
			continue
		}
		// handler是一个接口，用于处理不同情况的接口，其余几个参数全是channel
		if !kl.syncLoopIteration(updates, handler, syncTicker.C, housekeepingTicker.C, plegCh) {
			break
		}
	}
}

// syncLoopIteration reads from various channels and dispatches pods to the
// given handler.
//
// Arguments:
// 1.  configCh:       a channel to read config events from
// 2.  handler:        the SyncHandler to dispatch pods to
// 3.  syncCh:         a channel to read periodic sync events from
// 4.  houseKeepingCh: a channel to read housekeeping events from
// 5.  plegCh:         a channel to read PLEG updates from
//
// Events are also read from the kubelet liveness manager's update channel.
//
// The workflow is to read from one of the channels, handle that event, and
// update the timestamp in the sync loop monitor.
//
// Here is an appropriate place to note that despite the syntactical
// similarity to the switch statement, the case statements in a select are
// evaluated in a pseudorandom order if there are multiple channels ready to
// read from when the select is evaluated.  In other words, case statements
// are evaluated in random order, and you can not assume that the case
// statements evaluate in order if multiple channels have events.
//
// With that in mind, in truly no particular order, the different channels
// are handled as follows:
//
// * configCh: dispatch the pods for the config change to the appropriate
//             handler callback for the event type
// * plegCh: update the runtime cache; sync pod
// * syncCh: sync all pods waiting for sync
// * houseKeepingCh: trigger cleanup of pods
// * liveness manager: sync pods that have failed or in which one or more
//                     containers have failed liveness checks
// 这个方法就是对多个管道做遍历，发现任何一个管道有消息就交给 handler 去处理。
// configCh：读取配置事件的管道，就是之前讲过的通过文件、URL 和 apiserver 汇聚起来的事件
// syncCh：定时器管道，每次隔一段事件去同步最新保存的 pod 状态
// houseKeepingCh：housekeeping 事件的管道，做 pod 清理工作
// plegCh：PLEG 状态，如果 pod 的状态发生改变（因为某些情况被杀死，被暂停等），kubelet 也要做处理
// livenessManager.Updates()：健康检查发现某个 pod 不可用，一般也要对它进行重启
func (kl *Kubelet) syncLoopIteration(configCh <-chan kubetypes.PodUpdate, handler SyncHandler,
	syncCh <-chan time.Time, housekeepingCh <-chan time.Time, plegCh <-chan *pleg.PodLifecycleEvent) bool {
	kl.syncLoopMonitor.Store(kl.clock.Now())
	select {
	//configCh：读取配置事件的管道，就是之前讲过的通过文件、URL 和 apiserver 汇聚起来的事
	case u, open := <-configCh:
		// Update from a config source; dispatch it to the right handler
		// callback.
		if !open {
			glog.Errorf("Update channel is closed. Exiting the sync loop.")
			return false
		}

		switch u.Op {
		case kubetypes.ADD:
			glog.V(2).Infof("SyncLoop (ADD, %q): %q", u.Source, format.Pods(u.Pods))
			// After restarting, kubelet will get all existing pods through
			// ADD as if they are new pods. These pods will then go through the
			// admission process and *may* be rejected. This can be resolved
			// once we have checkpointing.
			// 每个管道的处理思路大同小异，我们只分析用户通过 apiserver 添加新 pod 的情况
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.UPDATE:
			glog.V(2).Infof("SyncLoop (UPDATE, %q): %q", u.Source, format.PodsWithDeletiontimestamps(u.Pods))
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.REMOVE:
			glog.V(2).Infof("SyncLoop (REMOVE, %q): %q", u.Source, format.Pods(u.Pods))
			handler.HandlePodRemoves(u.Pods)
		case kubetypes.RECONCILE:
			glog.V(4).Infof("SyncLoop (RECONCILE, %q): %q", u.Source, format.Pods(u.Pods))
			handler.HandlePodReconcile(u.Pods)
		case kubetypes.DELETE:
			glog.V(2).Infof("SyncLoop (DELETE, %q): %q", u.Source, format.Pods(u.Pods))
			// DELETE is treated as a UPDATE because of graceful deletion.
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.SET:
			// TODO: Do we want to support this?
			glog.Errorf("Kubelet does not support snapshot update")
		}

		// Mark the source ready after receiving at least one update from the
		// source. Once all the sources are marked ready, various cleanup
		// routines will start reclaiming resources. It is important that this
		// takes place only after kubelet calls the update handler to process
		// the update to ensure the internal pod cache is up-to-date.
		// 收到消息之后就把对应的来源标记为 ready 状态
		kl.sourcesReady.AddSource(u.Source)

	//PLEG 状态，如果 pod 的状态发生改变（因为某些情况被杀死，被暂停等），kubelet 也要做处理
	case e := <-plegCh:
		if isSyncPodWorthy(e) {
			// PLEG event for a pod; sync it.
			if pod, ok := kl.podManager.GetPodByUID(e.ID); ok {
				glog.V(2).Infof("SyncLoop (PLEG): %q, event: %#v", format.Pod(pod), e)
				handler.HandlePodSyncs([]*api.Pod{pod})
			} else {
				// If the pod no longer exists, ignore the event.
				glog.V(4).Infof("SyncLoop (PLEG): ignore irrelevant event: %#v", e)
			}
		}

		if e.Type == pleg.ContainerDied {
			if containerID, ok := e.Data.(string); ok {
				kl.cleanUpContainersInPod(e.ID, containerID)
			}
		}
	//syncCh：定时器管道，每次隔一段事件去同步最新保存的 pod 状态
	case <-syncCh:
		// Sync pods waiting for sync
		podsToSync := kl.getPodsToSync()
		if len(podsToSync) == 0 {
			break
		}
		glog.V(4).Infof("SyncLoop (SYNC): %d pods; %s", len(podsToSync), format.Pods(podsToSync))
		kl.HandlePodSyncs(podsToSync)
	//livenessManager.Updates()：健康检查发现某个 pod 不可用，一般也要对它进行重启
	case update := <-kl.livenessManager.Updates():
		if update.Result == proberesults.Failure {
			// The liveness manager detected a failure; sync the pod.

			// We should not use the pod from livenessManager, because it is never updated after
			// initialization.
			pod, ok := kl.podManager.GetPodByUID(update.PodUID)
			if !ok {
				// If the pod no longer exists, ignore the update.
				glog.V(4).Infof("SyncLoop (container unhealthy): ignore irrelevant update: %#v", update)
				break
			}
			glog.V(1).Infof("SyncLoop (container unhealthy): %q", format.Pod(pod))
			handler.HandlePodSyncs([]*api.Pod{pod})
		}
	//livenessManager.Updates()：健康检查发现某个 pod 不可用，一般也要对它进行重启
	case <-housekeepingCh:
		if !kl.sourcesReady.AllReady() {
			// If the sources aren't ready or volume manager has not yet synced the states,
			// skip housekeeping, as we may accidentally delete pods from unready sources.
			glog.V(4).Infof("SyncLoop (housekeeping, skipped): sources aren't ready yet.")
		} else {
			glog.V(4).Infof("SyncLoop (housekeeping)")
			if err := handler.HandlePodCleanups(); err != nil {
				glog.Errorf("Failed cleaning pods: %v", err)
			}
		}
	}
	kl.syncLoopMonitor.Store(kl.clock.Now())
	return true
}

// dispatchWork starts the asynchronous sync of the pod in a pod worker.
// If the pod is terminated, dispatchWork
// dispatchWork,它的作用就是根据 pod 把任务发送给特定的执行者podworker
// 类型由syncType决定
func (kl *Kubelet) dispatchWork(pod *api.Pod, syncType kubetypes.SyncPodType, mirrorPod *api.Pod, start time.Time) {
	if kl.podIsTerminated(pod) {
		if pod.DeletionTimestamp != nil {
			// If the pod is in a terminated state, there is no pod worker to
			// handle the work item. Check if the DeletionTimestamp has been
			// set, and force a status update to trigger a pod deletion request
			// to the apiserver.
			kl.statusManager.TerminatePod(pod)
		}
		return
	}
	// Run the sync in an async worker.
	// 将接收到的参数封装成 UpdatePodOptions，调用 kl.podWorkers.UpdatePod 方法
	// 看一下UpdatePod方法
	//update方法通过识别要做的操作
	//podWorkers有一个锁机制，用于所有的worker之间的资源互斥
	//每个pod，podWorker都会对应一个后台routine
	kl.podWorkers.UpdatePod(&UpdatePodOptions{
		Pod:        pod,
		MirrorPod:  mirrorPod,
		UpdateType: syncType,
		OnCompleteFunc: func(err error) {
			if err != nil {
				metrics.PodWorkerLatency.WithLabelValues(syncType.String()).Observe(metrics.SinceInMicroseconds(start))
			}
		},
	})
	// Note the number of containers for new pods.
	// 记录创建的pod中有几个container
	if syncType == kubetypes.SyncPodCreate {
		metrics.ContainersPerPodCount.Observe(float64(len(pod.Spec.Containers)))
	}
}

// TODO: handle mirror pods in a separate component (issue #17251)
func (kl *Kubelet) handleMirrorPod(mirrorPod *api.Pod, start time.Time) {
	// Mirror pod ADD/UPDATE/DELETE operations are considered an UPDATE to the
	// corresponding static pod. Send update to the pod worker if the static
	// pod exists.
	if pod, ok := kl.podManager.GetPodByMirrorPod(mirrorPod); ok {
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodAdditions is the callback in SyncHandler for pods being added from
// a config source.
// kubelet从通信管道中获取到了创建pod的消息，然后调用这个处理函数
func (kl *Kubelet) HandlePodAdditions(pods []*api.Pod) {
	start := kl.clock.Now()

	if utilconfig.DefaultFeatureGate.ExperimentalCriticalPodAnnotation() {
		// Pass critical pods through admission check first.
		var criticalPods []*api.Pod
		var nonCriticalPods []*api.Pod
		for _, p := range pods {
			if kubetypes.IsCriticalPod(p) {
				criticalPods = append(criticalPods, p)
			} else {
				nonCriticalPods = append(nonCriticalPods, p)
			}
		}
		//把所有的 pod 按照创建日期进行排序，保证最先创建的 pod 会最先被处理
		sort.Sort(sliceutils.PodsByCreationTime(criticalPods))
		sort.Sort(sliceutils.PodsByCreationTime(nonCriticalPods))
		pods = append(criticalPods, nonCriticalPods...)

	} else {
		sort.Sort(sliceutils.PodsByCreationTime(pods))
	}
	//循环处理事件中的pod
	for _, pod := range pods {
		// podManager是kubelet 的 source of truth,所有被管理的 pod 都要出现在里面。
		// 如果没出现则说明被删除了
		existingPods := kl.podManager.GetPods()
		// Always add the pod to the pod manager. Kubelet relies on the pod
		// manager as the source of truth for the desired state. If a pod does
		// not exist in the pod manager, it means that it has been deleted in
		// the apiserver and no action (other than cleanup) is required.
		// 将pod添加到podManager，podManager有pod的列表，不做具体操作，写进自己的数据结构中
		// 同样删除pod的时候也会从数据结构中删除
		kl.podManager.AddPod(pod)
		// 如果是mirror pod调用其单独的方法
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}

		if !kl.podIsTerminated(pod) {
			// Only go through the admission process if the pod is not
			// terminated.

			// We failed pods that we rejected, so activePods include all admitted
			// pods that are alive.
			activePods := kl.filterOutTerminatedPods(existingPods)

			// Check if we can admit the pod; if not, reject it.
			// 查看本机是否可以运行这个pod
			if ok, reason, message := kl.canAdmitPod(activePods, pod); !ok {
				//验证 pod 是否能在该节点运行，如果不可以直接拒绝
				kl.rejectPod(pod, reason, message)
				continue
			}
		}
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		// 把 pod 分配给给 worker 做异步处理
		// 告诉woker要创建的pod的信息，要对pod的做的操作是create。
		// 看一下这个处理方法
		kl.dispatchWork(pod, kubetypes.SyncPodCreate, mirrorPod, start)
		// 在probeManger中添加 pod
		// 如果 pod 中定义了 readiness 和 liveness 健康检查，启动 goroutine 定期进行检测
		kl.probeManager.AddPod(pod)
	}
}

// HandlePodUpdates is the callback in the SyncHandler interface for pods
// being updated from a config source.
func (kl *Kubelet) HandlePodUpdates(pods []*api.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		kl.podManager.UpdatePod(pod)
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}
		// TODO: Evaluate if we need to validate and reject updates.

		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodRemoves is the callback in the SyncHandler interface for pods
// being removed from a config source.
func (kl *Kubelet) HandlePodRemoves(pods []*api.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		kl.podManager.DeletePod(pod)
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}
		// Deletion is allowed to fail because the periodic cleanup routine
		// will trigger deletion again.
		if err := kl.deletePod(pod); err != nil {
			glog.V(2).Infof("Failed to delete pod %q, err: %v", format.Pod(pod), err)
		}
		kl.probeManager.RemovePod(pod)
	}
}

// HandlePodReconcile is the callback in the SyncHandler interface for pods
// that should be reconciled.
func (kl *Kubelet) HandlePodReconcile(pods []*api.Pod) {
	for _, pod := range pods {
		// Update the pod in pod manager, status manager will do periodically reconcile according
		// to the pod manager.
		kl.podManager.UpdatePod(pod)

		// After an evicted pod is synced, all dead containers in the pod can be removed.
		if eviction.PodIsEvicted(pod.Status) {
			if podStatus, err := kl.podCache.Get(pod.UID); err == nil {
				kl.containerDeletor.deleteContainersInPod("", podStatus, true)
			}
		}
	}
}

// HandlePodSyncs is the callback in the syncHandler interface for pods
// that should be dispatched to pod workers for sync.
func (kl *Kubelet) HandlePodSyncs(pods []*api.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodSync, mirrorPod, start)
	}
}

// LatestLoopEntryTime returns the last time in the sync loop monitor.
func (kl *Kubelet) LatestLoopEntryTime() time.Time {
	val := kl.syncLoopMonitor.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

// updateRuntimeUp calls the container runtime status callback, initializing
// the runtime dependent modules when the container runtime first comes up,
// and returns an error if the status check fails.  If the status check is OK,
// update the container runtime uptime in the kubelet runtimeState.
func (kl *Kubelet) updateRuntimeUp() {
	s, err := kl.containerRuntime.Status()
	if err != nil {
		glog.Errorf("Container runtime sanity check failed: %v", err)
		return
	}
	// Only check specific conditions when runtime integration type is cri,
	// because the old integration doesn't populate any runtime condition.
	if kl.kubeletConfiguration.EnableCRI {
		if s == nil {
			glog.Errorf("Container runtime status is nil")
			return
		}
		// Periodically log the whole runtime status for debugging.
		// TODO(random-liu): Consider to send node event when optional
		// condition is unmet.
		glog.V(4).Infof("Container runtime status: %v", s)
		networkReady := s.GetRuntimeCondition(kubecontainer.NetworkReady)
		if networkReady == nil || !networkReady.Status {
			glog.Errorf("Container runtime network not ready: %v", networkReady)
			kl.runtimeState.setNetworkState(fmt.Errorf("runtime network not ready: %v", networkReady))
		} else {
			// Set nil if the containe runtime network is ready.
			kl.runtimeState.setNetworkState(nil)
		}
		// TODO(random-liu): Add runtime error in runtimeState, and update it
		// when runtime is not ready, so that the information in RuntimeReady
		// condition will be propagated to NodeReady condition.
		runtimeReady := s.GetRuntimeCondition(kubecontainer.RuntimeReady)
		// If RuntimeReady is not set or is false, report an error.
		if runtimeReady == nil || !runtimeReady.Status {
			glog.Errorf("Container runtime not ready: %v", runtimeReady)
			return
		}
	}
	kl.oneTimeInitializer.Do(kl.initializeRuntimeDependentModules)
	kl.runtimeState.setRuntimeSync(kl.clock.Now())
}

// updateCloudProviderFromMachineInfo updates the node's provider ID field
// from the given cadvisor machine info.
func (kl *Kubelet) updateCloudProviderFromMachineInfo(node *api.Node, info *cadvisorapi.MachineInfo) {
	if info.CloudProvider != cadvisorapi.UnknownProvider &&
		info.CloudProvider != cadvisorapi.Baremetal {
		// The cloud providers from pkg/cloudprovider/providers/* that update ProviderID
		// will use the format of cloudprovider://project/availability_zone/instance_name
		// here we only have the cloudprovider and the instance name so we leave project
		// and availability zone empty for compatibility.
		node.Spec.ProviderID = strings.ToLower(string(info.CloudProvider)) +
			":////" + string(info.InstanceID)
	}
}

// GetConfiguration returns the KubeletConfiguration used to configure the kubelet.
func (kl *Kubelet) GetConfiguration() componentconfig.KubeletConfiguration {
	return kl.kubeletConfiguration
}

// BirthCry sends an event that the kubelet has started up.
func (kl *Kubelet) BirthCry() {
	// Make an event that kubelet restarted.
	kl.recorder.Eventf(kl.nodeRef, api.EventTypeNormal, events.StartingKubelet, "Starting kubelet.")
}

// StreamingConnectionIdleTimeout returns the timeout for streaming connections to the HTTP server.
func (kl *Kubelet) StreamingConnectionIdleTimeout() time.Duration {
	return kl.streamingConnectionIdleTimeout
}

// ResyncInterval returns the interval used for periodic syncs.
func (kl *Kubelet) ResyncInterval() time.Duration {
	return kl.resyncInterval
}

// ListenAndServe runs the kubelet HTTP server.
func (kl *Kubelet) ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableDebuggingHandlers bool) {
	server.ListenAndServeKubeletServer(kl, kl.resourceAnalyzer, address, port, tlsOptions, auth, enableDebuggingHandlers, kl.containerRuntime, kl.criHandler)
}

// ListenAndServeReadOnly runs the kubelet HTTP server in read-only mode.
func (kl *Kubelet) ListenAndServeReadOnly(address net.IP, port uint) {
	server.ListenAndServeKubeletReadOnlyServer(kl, kl.resourceAnalyzer, address, port, kl.containerRuntime)
}

// Delete the eligible dead container instances in a pod. Depending on the configuration, the latest dead containers may be kept around.
func (kl *Kubelet) cleanUpContainersInPod(podId types.UID, exitedContainerID string) {
	if podStatus, err := kl.podCache.Get(podId); err == nil {
		removeAll := false
		if syncedPod, ok := kl.podManager.GetPodByUID(podId); ok {
			// When an evicted pod has already synced, all containers can be removed.
			removeAll = eviction.PodIsEvicted(syncedPod.Status)
		}
		kl.containerDeletor.deleteContainersInPod(exitedContainerID, podStatus, removeAll)
	}
}

// isSyncPodWorthy filters out events that are not worthy of pod syncing
func isSyncPodWorthy(event *pleg.PodLifecycleEvent) bool {
	// ContatnerRemoved doesn't affect pod state
	return event.Type != pleg.ContainerRemoved
}

// parseResourceList parses the given configuration map into an API
// ResourceList or returns an error.
func parseResourceList(m utilconfig.ConfigurationMap) (api.ResourceList, error) {
	rl := make(api.ResourceList)
	for k, v := range m {
		switch api.ResourceName(k) {
		// Only CPU and memory resources are supported.
		case api.ResourceCPU, api.ResourceMemory:
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return nil, err
			}
			if q.Sign() == -1 {
				return nil, fmt.Errorf("resource quantity for %q cannot be negative: %v", k, v)
			}
			rl[api.ResourceName(k)] = q
		default:
			return nil, fmt.Errorf("cannot reserve %q resource", k)
		}
	}
	return rl, nil
}

// ParseReservation parses the given kubelet- and system- reservations
// configuration maps into an internal Reservation instance or returns an
// error.
func ParseReservation(kubeReserved, systemReserved utilconfig.ConfigurationMap) (*kubetypes.Reservation, error) {
	reservation := new(kubetypes.Reservation)
	if rl, err := parseResourceList(kubeReserved); err != nil {
		return nil, err
	} else {
		reservation.Kubernetes = rl
	}
	if rl, err := parseResourceList(systemReserved); err != nil {
		return nil, err
	} else {
		reservation.System = rl
	}
	return reservation, nil
}

// Gets the streaming server configuration to use with in-process CRI shims.
func getStreamingConfig(kubeCfg *componentconfig.KubeletConfiguration, kubeDeps *KubeletDeps) *streaming.Config {
	config := &streaming.Config{
		// Use a relative redirect (no scheme or host).
		BaseURL: &url.URL{
			Path: "/cri/",
		},
		StreamIdleTimeout:     kubeCfg.StreamingConnectionIdleTimeout.Duration,
		StreamCreationTimeout: streaming.DefaultConfig.StreamCreationTimeout,
		SupportedProtocols:    streaming.DefaultConfig.SupportedProtocols,
	}
	if kubeDeps.TLSOptions != nil {
		config.TLSConfig = kubeDeps.TLSOptions.Config
	}
	return config
}
