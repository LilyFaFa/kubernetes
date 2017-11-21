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

// Package install installs the v1 monolithic api, making it available as an
// option to all of the API encoding/decoding machinery.
package install

import (
	"fmt"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/apimachinery"
	"k8s.io/kubernetes/pkg/apimachinery/registered"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/sets"
)

// 这个 go文件是用于初始化core这个group的，通过init函数，通过在import_known_version.go
// 中通过 import _的方式初始化调用
const importPrefix = "k8s.io/kubernetes/pkg/api"

var accessor = meta.NewAccessor()

// availableVersions lists all known external versions for this group from most preferred to least preferred
// version就是v1，availableVersions定义了一个GroupName为空，Version是'v1'的GroupVersion
var availableVersions = []unversioned.GroupVersion{v1.SchemeGroupVersion}

func init() {
	// 将version进行注册，存入APIRegistrationManager.registeredVersions中
	registered.RegisterVersions(availableVersions)
	externalVersions := []unversioned.GroupVersion{}
	for _, v := range availableVersions {
		if registered.IsAllowedVersion(v) {
			externalVersions = append(externalVersions, v)
		}
	}
	if len(externalVersions) == 0 {
		glog.V(4).Infof("No version is registered for group %v", api.GroupName)
		return
	}
	// 将version进行enable，其实就是存入APIRegistrationManager.enabledVersions
	if err := registered.EnableVersions(externalVersions...); err != nil {
		glog.V(4).Infof("%v", err)
		return
	}
	// 看一下 enableVersions，这个比较重要
	if err := enableVersions(externalVersions); err != nil {
		glog.V(4).Infof("%v", err)
		return
	}
}

// TODO: enableVersions should be centralized rather than spread in each API
// group.
// We can combine registered.RegisterVersions, registered.EnableVersions and
// registered.RegisterGroup once we have moved enableVersions there.
func enableVersions(externalVersions []unversioned.GroupVersion) error {
	// 字面意思：将所有的Versions添加到Scheme
	// 又牵扯到Scheme，后面会介绍Scheme的初始化
	// 越深入看牵扯出的概念越多，该接口也很重要，需要耐心层层挖掘
	addVersionsToScheme(externalVersions...)
	// 将group其中的一个GroupVersion作为默认的,即'/api/v1'
	preferredExternalVersion := externalVersions[0]
	// 就是这里！ 进行了GroupMeta的初始化
	groupMeta := apimachinery.GroupMeta{
		// 默认的group version
		GroupVersion: preferredExternalVersion,
		// 所有的group version
		GroupVersions: externalVersions,
		// RESTMapper也是关键所在，会介绍
		RESTMapper:    newRESTMapper(externalVersions),
		SelfLinker:    runtime.SelfLinker(accessor),
		InterfacesFor: interfacesFor,
	}
	// 前面都是register和enable了versions，这里才是进行了Group的register
	// 该接口其实就是以第一个GroupVersion的groupName为key，groupMeta为value
	// 对APIRegistrationManager的groupMetaMap,进行了赋值
	if err := registered.RegisterGroup(groupMeta); err != nil {
		return err
	}
	return nil
}

func newRESTMapper(externalVersions []unversioned.GroupVersion) meta.RESTMapper {
	// the list of kinds that are scoped at the root of the api hierarchy
	// if a kind is not enumerated here, it is assumed to have a namespace scope
	rootScoped := sets.NewString(
		"Node",
		"Namespace",
		"PersistentVolume",
		"ComponentStatus",
	)

	// these kinds should be excluded from the list of resources
	ignoredKinds := sets.NewString(
		"ListOptions",
		"DeleteOptions",
		"Status",
		"PodLogOptions",
		"PodExecOptions",
		"PodAttachOptions",
		"PodProxyOptions",
		"NodeProxyOptions",
		"ServiceProxyOptions",
		"ThirdPartyResource",
		"ThirdPartyResourceData",
		"ThirdPartyResourceList")

	mapper := api.NewDefaultRESTMapper(externalVersions, interfacesFor, importPrefix, ignoredKinds, rootScoped)

	return mapper
}

// InterfacesFor returns the default Codec and ResourceVersioner for a given version
// string, or an error if the version is not known.
func interfacesFor(version unversioned.GroupVersion) (*meta.VersionInterfaces, error) {
	switch version {
	case v1.SchemeGroupVersion:
		return &meta.VersionInterfaces{
			ObjectConvertor:  api.Scheme,
			MetadataAccessor: accessor,
		}, nil
	default:
		g, _ := registered.Group(api.GroupName)
		return nil, fmt.Errorf("unsupported storage version: %s (valid: %v)", version, g.GroupVersions)
	}
}

func addVersionsToScheme(externalVersions ...unversioned.GroupVersion) {
	// add the internal version to Scheme
	// AddToScheme(api.Scheme)都是将GroupVersion加入到api.Scheme
	if err := api.AddToScheme(api.Scheme); err != nil {
		// Programmer error, detect immediately
		panic(err)
	}
	// add the enabled external versions to Scheme
	for _, v := range externalVersions {
		if !registered.IsEnabledVersion(v) {
			glog.Errorf("Version %s is not enabled, so it will not be added to the Scheme.", v)
			continue
		}
		switch v {
		case v1.SchemeGroupVersion:
			if err := v1.AddToScheme(api.Scheme); err != nil {
				// Programmer error, detect immediately
				panic(err)
			}
		}
	}
}
