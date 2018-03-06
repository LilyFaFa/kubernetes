/*
Copyright 2014 The Kubernetes Authors.

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

package master

import (
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	apiv1 "k8s.io/kubernetes/pkg/api/v1"
	appsapi "k8s.io/kubernetes/pkg/apis/apps/v1beta1"
	authenticationv1beta1 "k8s.io/kubernetes/pkg/apis/authentication/v1beta1"
	authorizationapiv1beta1 "k8s.io/kubernetes/pkg/apis/authorization/v1beta1"
	autoscalingapiv1 "k8s.io/kubernetes/pkg/apis/autoscaling/v1"
	batchapiv1 "k8s.io/kubernetes/pkg/apis/batch/v1"
	certificatesapiv1alpha1 "k8s.io/kubernetes/pkg/apis/certificates/v1alpha1"
	extensionsapiv1beta1 "k8s.io/kubernetes/pkg/apis/extensions/v1beta1"
	policyapiv1beta1 "k8s.io/kubernetes/pkg/apis/policy/v1beta1"
	rbacapi "k8s.io/kubernetes/pkg/apis/rbac/v1alpha1"
	storageapiv1beta1 "k8s.io/kubernetes/pkg/apis/storage/v1beta1"
	coreclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/core/internalversion"
	"k8s.io/kubernetes/pkg/genericapiserver"
	"k8s.io/kubernetes/pkg/genericapiserver/options"
	"k8s.io/kubernetes/pkg/healthz"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/master/thirdparty"
	utilnet "k8s.io/kubernetes/pkg/util/net"
	nodeutil "k8s.io/kubernetes/pkg/util/node"

	"k8s.io/kubernetes/pkg/registry/generic"
	"k8s.io/kubernetes/pkg/registry/generic/registry"
	"k8s.io/kubernetes/pkg/routes"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"

	// RESTStorage installers
	appsrest "k8s.io/kubernetes/pkg/registry/apps/rest"
	authenticationrest "k8s.io/kubernetes/pkg/registry/authentication/rest"
	authorizationrest "k8s.io/kubernetes/pkg/registry/authorization/rest"
	autoscalingrest "k8s.io/kubernetes/pkg/registry/autoscaling/rest"
	batchrest "k8s.io/kubernetes/pkg/registry/batch/rest"
	certificatesrest "k8s.io/kubernetes/pkg/registry/certificates/rest"
	corerest "k8s.io/kubernetes/pkg/registry/core/rest"
	extensionsrest "k8s.io/kubernetes/pkg/registry/extensions/rest"
	policyrest "k8s.io/kubernetes/pkg/registry/policy/rest"
	rbacrest "k8s.io/kubernetes/pkg/registry/rbac/rest"
	storagerest "k8s.io/kubernetes/pkg/registry/storage/rest"
)

const (
	// DefaultEndpointReconcilerInterval is the default amount of time for how often the endpoints for
	// the kubernetes Service are reconciled.
	DefaultEndpointReconcilerInterval = 10 * time.Second
)

type Config struct {
	GenericConfig *genericapiserver.Config

	StorageFactory           genericapiserver.StorageFactory
	EnableWatchCache         bool
	EnableCoreControllers    bool
	EndpointReconcilerConfig EndpointReconcilerConfig
	DeleteCollectionWorkers  int
	EventTTL                 time.Duration
	KubeletClientConfig      kubeletclient.KubeletClientConfig

	// Used to start and monitor tunneling
	Tunneler          genericapiserver.Tunneler
	EnableUISupport   bool
	EnableLogsSupport bool
	ProxyTransport    http.RoundTripper

	// Values to build the IP addresses used by discovery
	// The range of IPs to be assigned to services with type=ClusterIP or greater
	ServiceIPRange net.IPNet
	// The IP address for the GenericAPIServer service (must be inside ServiceIPRange)
	APIServerServiceIP net.IP
	// Port for the apiserver service.
	APIServerServicePort int

	// TODO, we can probably group service related items into a substruct to make it easier to configure
	// the API server items and `Extra*` fields likely fit nicely together.

	// The range of ports to be assigned to services with type=NodePort or greater
	ServiceNodePortRange utilnet.PortRange
	// Additional ports to be exposed on the GenericAPIServer service
	// extraServicePorts is injectable in the event that more ports
	// (other than the default 443/tcp) are exposed on the GenericAPIServer
	// and those ports need to be load balanced by the GenericAPIServer
	// service because this pkg is linked by out-of-tree projects
	// like openshift which want to use the GenericAPIServer but also do
	// more stuff.
	ExtraServicePorts []api.ServicePort
	// Additional ports to be exposed on the GenericAPIServer endpoints
	// Port names should align with ports defined in ExtraServicePorts
	ExtraEndpointPorts []api.EndpointPort
	// If non-zero, the "kubernetes" services uses this port as NodePort.
	// TODO(sttts): move into master
	KubernetesServiceNodePort int

	// Number of masters running; all masters must be started with the
	// same value for this field. (Numbers > 1 currently untested.)
	MasterCount int
}

// EndpointReconcilerConfig holds the endpoint reconciler and endpoint reconciliation interval to be
// used by the master.
type EndpointReconcilerConfig struct {
	Reconciler EndpointReconciler
	Interval   time.Duration
}

// Master contains state for a Kubernetes cluster master/api server.
type Master struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
}

type completedConfig struct {
	*Config
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (c *Config) Complete() completedConfig {
	c.GenericConfig.Complete()

	serviceIPRange, apiServerServiceIP, err := genericapiserver.DefaultServiceIPRange(c.ServiceIPRange)
	if err != nil {
		glog.Fatalf("Error determining service IP ranges: %v", err)
	}
	if c.ServiceIPRange.IP == nil {
		c.ServiceIPRange = serviceIPRange
	}
	if c.APIServerServiceIP == nil {
		c.APIServerServiceIP = apiServerServiceIP
	}

	discoveryAddresses := genericapiserver.DefaultDiscoveryAddresses{DefaultAddress: c.GenericConfig.ExternalAddress}
	discoveryAddresses.DiscoveryCIDRRules = append(discoveryAddresses.DiscoveryCIDRRules,
		genericapiserver.DiscoveryCIDRRule{IPRange: c.ServiceIPRange, Address: net.JoinHostPort(c.APIServerServiceIP.String(), strconv.Itoa(c.APIServerServicePort))})
	c.GenericConfig.DiscoveryAddresses = discoveryAddresses

	if c.ServiceNodePortRange.Size == 0 {
		// TODO: Currently no way to specify an empty range (do we need to allow this?)
		// We should probably allow this for clouds that don't require NodePort to do load-balancing (GCE)
		// but then that breaks the strict nestedness of ServiceType.
		// Review post-v1
		c.ServiceNodePortRange = options.DefaultServiceNodePortRange
		glog.Infof("Node port range unspecified. Defaulting to %v.", c.ServiceNodePortRange)
	}

	// enable swagger UI only if general UI support is on
	c.GenericConfig.EnableSwaggerUI = c.GenericConfig.EnableSwaggerUI && c.EnableUISupport

	if c.EndpointReconcilerConfig.Interval == 0 {
		c.EndpointReconcilerConfig.Interval = DefaultEndpointReconcilerInterval
	}

	if c.EndpointReconcilerConfig.Reconciler == nil {
		// use a default endpoint reconciler if nothing is set
		endpointClient := coreclient.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig)
		c.EndpointReconcilerConfig.Reconciler = NewMasterCountEndpointReconciler(c.MasterCount, endpointClient)
	}

	// this has always been hardcoded true in the past
	c.GenericConfig.EnableMetrics = true

	return completedConfig{c}
}

// SkipComplete provides a way to construct a server instance without config completion.
func (c *Config) SkipComplete() completedConfig {
	return completedConfig{c}
}

// New returns a new instance of Master from the given config.
// Certain config fields will be set to a default value if unset.
// Certain config fields must be specified, including:
//   KubeletClientConfig
func (c completedConfig) New() (*Master, error) {
	if reflect.DeepEqual(c.KubeletClientConfig, kubeletclient.KubeletClientConfig{}) {
		return nil, fmt.Errorf("Master.New() called with empty config.KubeletClientConfig")
	}
	// 这个New函数可以看一下
	// 生成了server这个参数，也就是GenericAPIServer
	s, err := c.Config.GenericConfig.SkipComplete().New() // completion is done in Complete, no need for a second time
	if err != nil {
		return nil, err
	}

	if c.EnableUISupport {
		routes.UIRedirect{}.Install(s.HandlerContainer)
	}
	if c.EnableLogsSupport {
		routes.Logs{}.Install(s.HandlerContainer)
	}

	m := &Master{
		GenericAPIServer: s,
	}

	restOptionsFactory := restOptionsFactory{
		// 最大的删除资源的携程数
		deleteCollectionWorkers: c.DeleteCollectionWorkers,
		// 是否使能GC
		enableGarbageCollection: c.GenericConfig.EnableGarbageCollection,
		storageFactory:          c.StorageFactory,
	}
	// 判断是否使能了用于Watch的Cache
	// 有无cache赋值的是不同的接口实现
	// restOptionsFactory.storageDecorator ：是一个各个资源的REST interface(CRUD)装饰者
	// 后面调用NewStorage()时会用到该接口，并输出对应的CRUD接口及销毁接口。
	// 可以参考pkg/registry/core/pod/etcd/etcd.go中的NewStorage()
	// 其实这里有无cache的接口差异就在于：有cache的话，就提供操作cache的接口；无cache的话，就提供直接操作etcd的接口
	if c.EnableWatchCache {
		restOptionsFactory.storageDecorator = registry.StorageWithCacher
	} else {
		restOptionsFactory.storageDecorator = generic.UndecoratedStorage
	}

	// install legacy rest storage
	// /api这个资源的注册
	if c.GenericConfig.APIResourceConfigSource.AnyResourcesForVersionEnabled(apiv1.SchemeGroupVersion) {
		legacyRESTStorageProvider := corerest.LegacyRESTStorageProvider{
			StorageFactory:       c.StorageFactory,
			ProxyTransport:       c.ProxyTransport,
			KubeletClientConfig:  c.KubeletClientConfig,
			EventTTL:             c.EventTTL,
			ServiceIPRange:       c.ServiceIPRange,
			ServiceNodePortRange: c.ServiceNodePortRange,
			LoopbackClientConfig: c.GenericConfig.LoopbackClientConfig,
		}
		//restOptionsFactory.NewFor接口一直被往下传，直到NewLegacyRESTStorage()接口中被调用然后创建了opts，
		m.InstallLegacyAPI(c.Config, restOptionsFactory.NewFor, legacyRESTStorageProvider)
	}
	// /apis这个资源的注册
	restStorageProviders := []genericapiserver.RESTStorageProvider{
		appsrest.RESTStorageProvider{},
		authenticationrest.RESTStorageProvider{Authenticator: c.GenericConfig.Authenticator},
		authorizationrest.RESTStorageProvider{Authorizer: c.GenericConfig.Authorizer},
		autoscalingrest.RESTStorageProvider{},
		batchrest.RESTStorageProvider{},
		certificatesrest.RESTStorageProvider{},
		extensionsrest.RESTStorageProvider{ResourceInterface: thirdparty.NewThirdPartyResourceServer(s, c.StorageFactory)},
		policyrest.RESTStorageProvider{},
		rbacrest.RESTStorageProvider{AuthorizerRBACSuperUser: c.GenericConfig.AuthorizerRBACSuperUser},
		storagerest.RESTStorageProvider{},
	}
	m.InstallAPIs(c.Config.GenericConfig.APIResourceConfigSource, restOptionsFactory.NewFor, restStorageProviders...)

	if c.Tunneler != nil {
		m.installTunneler(c.Tunneler, coreclient.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig).Nodes())
	}

	return m, nil
}

// /api资源的注册这个是 /api资源注册使用的，下面是 /apis注册使用的
// 第二个参数就是restOptionFactory.NewFor
func (m *Master) InstallLegacyAPI(c *Config, restOptionsGetter genericapiserver.RESTOptionsGetter, legacyRESTStorageProvider corerest.LegacyRESTStorageProvider) {
	// 这个地方是向etcd 建立注册资源关系，创建storage，需要看一下
	legacyRESTStorage, apiGroupInfo, err := legacyRESTStorageProvider.NewLegacyRESTStorage(restOptionsGetter)
	if err != nil {
		glog.Fatalf("Error building core storage: %v", err)
	}

	if c.EnableCoreControllers {
		serviceClient := coreclient.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig)
		bootstrapController := c.NewBootstrapController(legacyRESTStorage, serviceClient)
		if err := m.GenericAPIServer.AddPostStartHook("bootstrap-controller", bootstrapController.PostStartHook); err != nil {
			glog.Fatalf("Error registering PostStartHook %q: %v", "bootstrap-controller", err)
		}
	}
	// 这个是install函数，用于注册core group的资源，也就是 /api资源，进去之后会检测这个前缀
	if err := m.GenericAPIServer.InstallLegacyAPIGroup(genericapiserver.DefaultLegacyAPIPrefix, &apiGroupInfo); err != nil {
		glog.Fatalf("Error in registering group versions: %v", err)
	}
}

func (m *Master) installTunneler(tunneler genericapiserver.Tunneler, nodeClient coreclient.NodeInterface) {
	tunneler.Run(nodeAddressProvider{nodeClient}.externalAddresses)
	m.GenericAPIServer.AddHealthzChecks(healthz.NamedCheck("SSH Tunnel Check", genericapiserver.TunnelSyncHealthChecker(tunneler)))
	prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "apiserver_proxy_tunnel_sync_latency_secs",
		Help: "The time since the last successful synchronization of the SSH tunnels for proxy requests.",
	}, func() float64 { return float64(tunneler.SecondsSinceSync()) })
}

// InstallAPIs will install the APIs for the restStorageProviders if they are enabled.
func (m *Master) InstallAPIs(apiResourceConfigSource genericapiserver.APIResourceConfigSource, restOptionsGetter genericapiserver.RESTOptionsGetter, restStorageProviders ...genericapiserver.RESTStorageProvider) {
	apiGroupsInfo := []genericapiserver.APIGroupInfo{}

	// for循环便利restStorageProviders，虽然还是不是很清楚这个的作用
	// 但应该是在循环建立支持的Group
	for _, restStorageBuilder := range restStorageProviders {
		// restStorageBuilder应该是实现了某个接口。
		groupName := restStorageBuilder.GroupName()
		// apiResourceConfigSource应该是陈列了一些支持的group，如果在该集合中的应该是一些
		// enabled API,所以这里判断一下这个group是否需要跳过不进行注册
		if !apiResourceConfigSource.AnyResourcesForGroupEnabled(groupName) {
			glog.V(1).Infof("Skipping disabled API group %q.", groupName)
			continue
		}
		// 创建一个该group的apiGroupInfo，groupInfo是对这个group的描述，包含其所有的资源
		// 同时创建了资源的Storage，storage都在变量 apiGroupInfo 中
		apiGroupInfo, enabled := restStorageBuilder.NewRESTStorage(apiResourceConfigSource, restOptionsGetter)
		if !enabled {
			glog.Warningf("Problem initializing API group %q, skipping.", groupName)
			continue
		}
		glog.V(1).Infof("Enabling API group %q.", groupName)
		//这个看不太懂，Hook的作用是啥？
		if postHookProvider, ok := restStorageBuilder.(genericapiserver.PostStartHookProvider); ok {
			name, hook, err := postHookProvider.PostStartHook()
			if err != nil {
				glog.Fatalf("Error building PostStartHook: %v", err)
			}
			if err := m.GenericAPIServer.AddPostStartHook(name, hook); err != nil {
				glog.Fatalf("Error registering PostStartHook %q: %v", name, err)
			}
		}

		apiGroupsInfo = append(apiGroupsInfo, apiGroupInfo)
	}
	//过滤之后是支持的groupInfo，然后开始一一注册的过程，看一下这个注册的过程
	for i := range apiGroupsInfo {
		if err := m.GenericAPIServer.InstallAPIGroup(&apiGroupsInfo[i]); err != nil {
			glog.Fatalf("Error in registering group versions: %v", err)
		}
	}
}

type restOptionsFactory struct {
	// 最大回收资源的协程数目
	deleteCollectionWorkers int
	// 是否使能GC
	enableGarbageCollection bool
	// StorageFactory
	storageFactory   genericapiserver.StorageFactory
	storageDecorator generic.StorageDecorator
}

func (f restOptionsFactory) NewFor(resource unversioned.GroupResource) generic.RESTOptions {
	// 创建该资源的Storage Config
	storageConfig, err := f.storageFactory.NewConfig(resource)
	if err != nil {
		glog.Fatalf("Unable to find storage destination for %v, due to %v", resource, err.Error())
	}
	// 最终返回的就是RESTOptions, 就是前面的opts的类型
	// 需要关注f.storageDecorator的由来
	// 该接口比较简单，初始化了一个generic.RESTOptions变量，即opts。我们需要找出opts.Decorator的由来,就只需要看下上一个接口判断EnableWatchCache时就明白了。
	// opts.Decorator该接口最终返回了storage的interface和清除操作资源的接口。可以想一下带缓冲和不带缓冲的接口实现肯定不一致，所以这里需要进行区分:
	// - registry.StorageWithCacher：该接口是返回了操作cache的接口，和清除cache的操作接口
	// - generic.UndecoratedStorage: 该接口会根据你配置的后端类型(etcd2/etcd3等)，来返回不同的etcd操作接口，其实是为所有的资源对象创建了etcd的链接，然后通过该链接发送不同的命令，最后还返回了断开该链接的接口。
	// 所以实现完全不一样，一个操作cache，一个操作实际的etcd。
	return generic.RESTOptions{
		StorageConfig:           storageConfig,
		Decorator:               f.storageDecorator,
		DeleteCollectionWorkers: f.deleteCollectionWorkers,
		EnableGarbageCollection: f.enableGarbageCollection,
		ResourcePrefix:          f.storageFactory.ResourcePrefix(resource),
	}
}

type nodeAddressProvider struct {
	nodeClient coreclient.NodeInterface
}

func (n nodeAddressProvider) externalAddresses() (addresses []string, err error) {
	preferredAddressTypes := []api.NodeAddressType{
		api.NodeExternalIP,
		api.NodeLegacyHostIP,
	}
	nodes, err := n.nodeClient.List(api.ListOptions{})
	if err != nil {
		return nil, err
	}
	addrs := []string{}
	for ix := range nodes.Items {
		node := &nodes.Items[ix]
		addr, err := nodeutil.GetPreferredNodeAddress(node, preferredAddressTypes)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

func DefaultAPIResourceConfigSource() *genericapiserver.ResourceConfig {
	ret := genericapiserver.NewResourceConfig()
	ret.EnableVersions(
		apiv1.SchemeGroupVersion,
		extensionsapiv1beta1.SchemeGroupVersion,
		batchapiv1.SchemeGroupVersion,
		authenticationv1beta1.SchemeGroupVersion,
		autoscalingapiv1.SchemeGroupVersion,
		appsapi.SchemeGroupVersion,
		policyapiv1beta1.SchemeGroupVersion,
		rbacapi.SchemeGroupVersion,
		storageapiv1beta1.SchemeGroupVersion,
		certificatesapiv1alpha1.SchemeGroupVersion,
		authorizationapiv1beta1.SchemeGroupVersion,
	)

	// all extensions resources except these are disabled by default
	ret.EnableResources(
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("daemonsets"),
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("deployments"),
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("horizontalpodautoscalers"),
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("ingresses"),
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("jobs"),
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("networkpolicies"),
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("replicasets"),
		extensionsapiv1beta1.SchemeGroupVersion.WithResource("thirdpartyresources"),
	)

	return ret
}
