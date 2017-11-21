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

package genericapiserver

import (
	"fmt"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	systemd "github.com/coreos/go-systemd/daemon"
	"github.com/emicklei/go-restful"
	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/admission"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apimachinery"
	"k8s.io/kubernetes/pkg/apimachinery/registered"
	"k8s.io/kubernetes/pkg/apiserver"
	"k8s.io/kubernetes/pkg/client/restclient"
	genericmux "k8s.io/kubernetes/pkg/genericapiserver/mux"
	"k8s.io/kubernetes/pkg/genericapiserver/openapi/common"
	"k8s.io/kubernetes/pkg/genericapiserver/routes"
	"k8s.io/kubernetes/pkg/healthz"
	"k8s.io/kubernetes/pkg/runtime"
	utilnet "k8s.io/kubernetes/pkg/util/net"
	"k8s.io/kubernetes/pkg/util/sets"
)

// Info about an API group.
// API group的信息
type APIGroupInfo struct {
	// 该group的元数据信息
	// 主要包括Group的元信息，里面的成员RESTMapper，
	// 与APIGroupVersion一样，其实APIGroupVersion的RESTMapper直接取之于GroupMeta的RESTMapper.
	// 一个Group可能包含多个版本，存储在GroupVersion中，而GroupVersion是默认存储在etcd中的版本
	GroupMeta apimachinery.GroupMeta
	// Info about the resources in this group. Its a map from version to resource to the storage.
	// 版本信息，version-resource-storage，各个版本的各个资源的storage的map
	VersionedResourcesStorageMap map[string]map[string]rest.Storage
	// OptionsExternalVersion controls the APIVersion used for common objects in the
	// schema like api.Status, api.DeleteOptions, and api.ListOptions. Other implementors may
	// define a version "v1beta1" but want to use the Kubernetes "v1" internal objects.
	// If nil, defaults to groupMeta.GroupVersion.
	// TODO: Remove this when https://github.com/kubernetes/kubernetes/issues/19018 is fixed.
	// type GroupVersion struct {
	//	Group   string `protobuf:"bytes,1,opt,name=group"`
	//	Version string `protobuf:"bytes,2,opt,name=version"`
	// }
	OptionsExternalVersion *unversioned.GroupVersion

	// Scheme includes all of the types used by this group and how to convert between them (or
	// to convert objects from outside of this group that are accepted in this API).
	// TODO: replace with interfaces
	// core group的话，对应的就是api.Scheme
	// Scheme: 用于API资源之间的序列化、反序列化、版本转换。
	// Scheme里面还有好几个map，前面的结构体存储的都是unversioned.GroupVersionKind、unversioned.GroupVersion这些东西，
	// 这些东西本质上只是表示资源的字符串标识，Scheme存储了对应着标志的具体的API资源的结构体，即relect.Type
	Scheme *runtime.Scheme
	// NegotiatedSerializer controls how this group encodes and decodes data
	NegotiatedSerializer runtime.NegotiatedSerializer
	// ParameterCodec performs conversions for query parameters passed to API calls
	ParameterCodec runtime.ParameterCodec

	// SubresourceGroupVersionKind contains the GroupVersionKind overrides for each subresource that is
	// accessible from this API group version. The GroupVersionKind is that of the external version of
	// the subresource. The key of this map should be the path of the subresource. The keys here should
	// match the keys in the Storage map above for subresources.
	// 所有resources信息,key就是resource的path
	// type GroupVersionKind struct {
	//    Group   string `protobuf:"bytes,1,opt,name=group"`
	//    Version string `protobuf:"bytes,2,opt,name=version"`
	//    Kind    string `protobuf:"bytes,3,opt,name=kind"`
	// }
	SubresourceGroupVersionKind map[string]unversioned.GroupVersionKind
}

// GenericAPIServer contains state for a Kubernetes cluster api server.
type GenericAPIServer struct {
	// discoveryAddresses is used to build cluster IPs for discovery.
	discoveryAddresses DiscoveryAddresses

	// LoopbackClientConfig is a config for a privileged loopback connection to the API server
	LoopbackClientConfig *restclient.Config

	// minRequestTimeout is how short the request timeout can be.  This is used to build the RESTHandler
	minRequestTimeout time.Duration

	// enableSwaggerSupport indicates that swagger should be served.  This is currently separate because
	// the API group routes are created *after* initialization and you can't generate the swagger routes until
	// after those are available.
	// TODO eventually we should be able to factor this out to take place during initialization.
	enableSwaggerSupport bool

	// legacyAPIGroupPrefixes is used to set up URL parsing for authorization and for validating requests
	// to InstallLegacyAPIGroup
	legacyAPIGroupPrefixes sets.String

	// admissionControl is used to build the RESTStorage that backs an API Group.
	admissionControl admission.Interface

	// requestContextMapper provides a way to get the context for a request.  It may be nil.
	requestContextMapper api.RequestContextMapper

	// The registered APIs
	HandlerContainer *genericmux.APIContainer

	SecureServingInfo   *SecureServingInfo
	InsecureServingInfo *ServingInfo

	// numerical ports, set after listening
	effectiveSecurePort, effectiveInsecurePort int

	// ExternalAddress is the address (hostname or IP and port) that should be used in
	// external (public internet) URLs for this GenericAPIServer.
	ExternalAddress string

	// storage contains the RESTful endpoints exposed by this GenericAPIServer
	storage map[string]rest.Storage

	// Serializer controls how common API objects not in a group/version prefix are serialized for this server.
	// Individual APIGroups may define their own serializers.
	Serializer runtime.NegotiatedSerializer

	// "Outputs"
	Handler         http.Handler
	InsecureHandler http.Handler

	// Map storing information about all groups to be exposed in discovery response.
	// The map is from name to the group.
	apiGroupsForDiscoveryLock sync.RWMutex
	apiGroupsForDiscovery     map[string]unversioned.APIGroup

	// See Config.$name for documentation of these flags

	enableOpenAPISupport bool
	openAPIConfig        *common.Config

	// PostStartHooks are each called after the server has started listening, in a separate go func for each
	// with no guaranteee of ordering between them.  The map key is a name used for error reporting.
	// It may kill the process with a panic if it wishes to by returning an error
	postStartHookLock    sync.Mutex
	postStartHooks       map[string]postStartHookEntry
	postStartHooksCalled bool

	// healthz checks
	healthzLock    sync.Mutex
	healthzChecks  []healthz.HealthzChecker
	healthzCreated bool
}

func init() {
	// Send correct mime type for .svg files.
	// TODO: remove when https://github.com/golang/go/commit/21e47d831bafb59f22b1ea8098f709677ec8ce33
	// makes it into all of our supported go versions (only in v1.7.1 now).
	mime.AddExtensionType(".svg", "image/svg+xml")
}

// RequestContextMapper is exposed so that third party resource storage can be build in a different location.
// TODO refactor third party resource storage
func (s *GenericAPIServer) RequestContextMapper() api.RequestContextMapper {
	return s.requestContextMapper
}

// MinRequestTimeout is exposed so that third party resource storage can be build in a different location.
// TODO refactor third party resource storage
func (s *GenericAPIServer) MinRequestTimeout() time.Duration {
	return s.minRequestTimeout
}

type preparedGenericAPIServer struct {
	*GenericAPIServer
}

// PrepareRun does post API installation setup steps.
func (s *GenericAPIServer) PrepareRun() preparedGenericAPIServer {
	if s.enableSwaggerSupport {
		routes.Swagger{ExternalAddress: s.ExternalAddress}.Install(s.HandlerContainer)
	}
	if s.enableOpenAPISupport {
		routes.OpenAPI{
			Config: s.openAPIConfig,
		}.Install(s.HandlerContainer)
	}

	s.installHealthz()

	return preparedGenericAPIServer{s}
}

// Run spawns the http servers (secure and insecure). It only returns if stopCh is closed
// or one of the ports cannot be listened on initially.
func (s preparedGenericAPIServer) Run(stopCh <-chan struct{}) {
	if s.SecureServingInfo != nil && s.Handler != nil {
		if err := s.serveSecurely(stopCh); err != nil {
			glog.Fatal(err)
		}
	}

	if s.InsecureServingInfo != nil && s.InsecureHandler != nil {
		if err := s.serveInsecurely(stopCh); err != nil {
			glog.Fatal(err)
		}
	}

	s.RunPostStartHooks()

	// err == systemd.SdNotifyNoSocket when not running on a systemd system
	if err := systemd.SdNotify("READY=1\n"); err != nil && err != systemd.SdNotifyNoSocket {
		glog.Errorf("Unable to send systemd daemon successful start message: %v\n", err)
	}

	<-stopCh
}

// installAPIResources is a private method for installing the REST storage backing each api groupversionresource
func (s *GenericAPIServer) installAPIResources(apiPrefix string, apiGroupInfo *APIGroupInfo) error {
	// 遍历该group支持的version
	for _, groupVersion := range apiGroupInfo.GroupMeta.GroupVersions {
		apiGroupVersion, err := s.getAPIGroupVersion(apiGroupInfo, groupVersion, apiPrefix)
		if err != nil {
			return err
		}
		if apiGroupInfo.OptionsExternalVersion != nil {
			apiGroupVersion.OptionsExternalVersion = apiGroupInfo.OptionsExternalVersion
		}
		// 对该group version的资源进行注册
		// The registered APIs ==== HandlerContainer *genericmux.APIContainer
		// 看一下这个函数
		if err := apiGroupVersion.InstallREST(s.HandlerContainer.Container); err != nil {
			return fmt.Errorf("Unable to setup API %v: %v", apiGroupInfo, err)
		}
	}

	return nil
}

func (s *GenericAPIServer) InstallLegacyAPIGroup(apiPrefix string, apiGroupInfo *APIGroupInfo) error {
	if !s.legacyAPIGroupPrefixes.Has(apiPrefix) {
		return fmt.Errorf("%q is not in the allowed legacy API prefixes: %v", apiPrefix, s.legacyAPIGroupPrefixes.List())
	}
	//看一下这个函数，这个是注册资源的过程
	if err := s.installAPIResources(apiPrefix, apiGroupInfo); err != nil {
		return err
	}

	// setup discovery
	apiVersions := []string{}
	for _, groupVersion := range apiGroupInfo.GroupMeta.GroupVersions {
		apiVersions = append(apiVersions, groupVersion.Version)
	}
	// Install the version handler.
	// Add a handler at /<apiPrefix> to enumerate the supported api versions.
	apiserver.AddApiWebService(s.Serializer, s.HandlerContainer.Container, apiPrefix, func(req *restful.Request) *unversioned.APIVersions {
		clientIP := utilnet.GetClientIP(req.Request)

		apiVersionsForDiscovery := unversioned.APIVersions{
			ServerAddressByClientCIDRs: s.discoveryAddresses.ServerAddressByClientCIDRs(clientIP),
			Versions:                   apiVersions,
		}
		return &apiVersionsForDiscovery
	})
	return nil
}

// Exposes the given api group in the API.
// 这个函数是在某一个for循环中的，每一次循环都会对一个 APIGroupInfo 进行注册
func (s *GenericAPIServer) InstallAPIGroup(apiGroupInfo *APIGroupInfo) error {
	// Do not register empty group or empty version.  Doing so claims /apis/ for the wrong entity to be returned.
	// Catching these here places the error  much closer to its origin
	// 判断该group的名字是否是空
	if len(apiGroupInfo.GroupMeta.GroupVersion.Group) == 0 {
		return fmt.Errorf("cannot register handler with an empty group for %#v", *apiGroupInfo)
	}
	// 判断该group的默认version是否是空
	if len(apiGroupInfo.GroupMeta.GroupVersion.Version) == 0 {
		return fmt.Errorf("cannot register handler with an empty version for %#v", *apiGroupInfo)
	}
	// 定义的APIGroupPrefix = "/apis"，这里为什么只有apis一个呢？不知道
	// 看一下这里的installAPIResources资源注册函数
	if err := s.installAPIResources(APIGroupPrefix, apiGroupInfo); err != nil {
		return err
	}

	// setup discovery
	// Install the version handler.
	// Add a handler at /apis/<groupName> to enumerate all versions supported by this group.
	apiVersionsForDiscovery := []unversioned.GroupVersionForDiscovery{}
	for _, groupVersion := range apiGroupInfo.GroupMeta.GroupVersions {
		// Check the config to make sure that we elide versions that don't have any resources
		if len(apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version]) == 0 {
			continue
		}
		apiVersionsForDiscovery = append(apiVersionsForDiscovery, unversioned.GroupVersionForDiscovery{
			GroupVersion: groupVersion.String(),
			Version:      groupVersion.Version,
		})
	}
	preferedVersionForDiscovery := unversioned.GroupVersionForDiscovery{
		GroupVersion: apiGroupInfo.GroupMeta.GroupVersion.String(),
		Version:      apiGroupInfo.GroupMeta.GroupVersion.Version,
	}
	apiGroup := unversioned.APIGroup{
		Name:             apiGroupInfo.GroupMeta.GroupVersion.Group,
		Versions:         apiVersionsForDiscovery,
		PreferredVersion: preferedVersionForDiscovery,
	}

	s.AddAPIGroupForDiscovery(apiGroup)
	s.HandlerContainer.Add(apiserver.NewGroupWebService(s.Serializer, APIGroupPrefix+"/"+apiGroup.Name, apiGroup))

	return nil
}

func (s *GenericAPIServer) AddAPIGroupForDiscovery(apiGroup unversioned.APIGroup) {
	s.apiGroupsForDiscoveryLock.Lock()
	defer s.apiGroupsForDiscoveryLock.Unlock()

	s.apiGroupsForDiscovery[apiGroup.Name] = apiGroup
}

func (s *GenericAPIServer) RemoveAPIGroupForDiscovery(groupName string) {
	s.apiGroupsForDiscoveryLock.Lock()
	defer s.apiGroupsForDiscoveryLock.Unlock()

	delete(s.apiGroupsForDiscovery, groupName)
}

func (s *GenericAPIServer) getAPIGroupVersion(apiGroupInfo *APIGroupInfo, groupVersion unversioned.GroupVersion, apiPrefix string) (*apiserver.APIGroupVersion, error) {
	storage := make(map[string]rest.Storage)
	for k, v := range apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version] {
		storage[strings.ToLower(k)] = v
	}
	version, err := s.newAPIGroupVersion(apiGroupInfo, groupVersion)
	version.Root = apiPrefix
	version.Storage = storage
	return version, err
}

//创建一个APIGroupVersion，需要APIGroupInfo、 apiGroupInfo.Scheme、apiGroupInfo.GroupMeta参数
func (s *GenericAPIServer) newAPIGroupVersion(apiGroupInfo *APIGroupInfo, groupVersion unversioned.GroupVersion) (*apiserver.APIGroupVersion, error) {
	return &apiserver.APIGroupVersion{
		GroupVersion: groupVersion,

		ParameterCodec: apiGroupInfo.ParameterCodec,
		Serializer:     apiGroupInfo.NegotiatedSerializer,
		Creater:        apiGroupInfo.Scheme,
		Convertor:      apiGroupInfo.Scheme,
		Copier:         apiGroupInfo.Scheme,
		Typer:          apiGroupInfo.Scheme,
		SubresourceGroupVersionKind: apiGroupInfo.SubresourceGroupVersionKind,
		Linker: apiGroupInfo.GroupMeta.SelfLinker,
		Mapper: apiGroupInfo.GroupMeta.RESTMapper,

		Admit:             s.admissionControl,
		Context:           s.RequestContextMapper(),
		MinRequestTimeout: s.minRequestTimeout,
	}, nil
}

// DynamicApisDiscovery returns a webservice serving api group discovery.
// Note: during the server runtime apiGroupsForDiscovery might change.
func (s *GenericAPIServer) DynamicApisDiscovery() *restful.WebService {
	return apiserver.NewApisWebService(s.Serializer, APIGroupPrefix, func(req *restful.Request) []unversioned.APIGroup {
		s.apiGroupsForDiscoveryLock.RLock()
		defer s.apiGroupsForDiscoveryLock.RUnlock()

		// sort to have a deterministic order
		sortedGroups := []unversioned.APIGroup{}
		groupNames := make([]string, 0, len(s.apiGroupsForDiscovery))
		for groupName := range s.apiGroupsForDiscovery {
			groupNames = append(groupNames, groupName)
		}
		sort.Strings(groupNames)
		for _, groupName := range groupNames {
			sortedGroups = append(sortedGroups, s.apiGroupsForDiscovery[groupName])
		}

		clientIP := utilnet.GetClientIP(req.Request)
		serverCIDR := s.discoveryAddresses.ServerAddressByClientCIDRs(clientIP)
		groups := make([]unversioned.APIGroup, len(sortedGroups))
		for i := range sortedGroups {
			groups[i] = sortedGroups[i]
			groups[i].ServerAddressByClientCIDRs = serverCIDR
		}
		return groups
	})
}

// NewDefaultAPIGroupInfo returns an APIGroupInfo stubbed with "normal" values
// exposed for easier composition from other packages
func NewDefaultAPIGroupInfo(group string) APIGroupInfo {
	groupMeta := registered.GroupOrDie(group)

	return APIGroupInfo{
		GroupMeta:                    *groupMeta,
		VersionedResourcesStorageMap: map[string]map[string]rest.Storage{},
		OptionsExternalVersion:       &registered.GroupOrDie(api.GroupName).GroupVersion,
		Scheme:                       api.Scheme,
		ParameterCodec:               api.ParameterCodec,
		NegotiatedSerializer:         api.Codecs,
	}
}
