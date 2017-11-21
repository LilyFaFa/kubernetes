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

package registry

import (
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/registry/generic"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/storage"
	etcdstorage "k8s.io/kubernetes/pkg/storage/etcd"
	"k8s.io/kubernetes/pkg/storage/storagebackend"
	"k8s.io/kubernetes/pkg/storage/storagebackend/factory"
)

// Creates a cacher based given storageConfig.
func StorageWithCacher(
	storageConfig *storagebackend.Config,
	capacity int,
	objectType runtime.Object,
	resourcePrefix string,
	scopeStrategy rest.NamespaceScopedStrategy,
	newListFunc func() runtime.Object,
	triggerFunc storage.TriggerPublisherFunc) (storage.Interface, factory.DestroyFunc) {
	// storageConfig是后端存储的config，定义了存储类型，存储服务器List，TLS证书信息，Cache大小等。
	// 该接口就是generic.UndecoratedStorage()接口的实现，StorageWithCacher()接口就是多了下面的cacher操作
	// 创建和一个存储后端，看一下这个函数
	s, d := generic.NewRawStorage(storageConfig)
	// TODO: we would change this later to make storage always have cacher and hide low level KV layer inside.
	// Currently it has two layers of same storage interface -- cacher and low level kv.
	cacherConfig := storage.CacherConfig{
		CacheCapacity:        capacity,
		Storage:              s,
		Versioner:            etcdstorage.APIObjectVersioner{},
		Type:                 objectType,
		ResourcePrefix:       resourcePrefix,
		NewListFunc:          newListFunc,
		TriggerPublisherFunc: triggerFunc,
		Codec:                storageConfig.Codec,
	}
	// 根据是否有namespace来进行区分赋值
	// KeyFunc函数用于获取该object的Key:
	// 有namespace的话，key的格式：prefix + "/" + Namespace + "/" + name
	// 无namespace的话，key的格式：prefix + "/" + name
	if scopeStrategy.NamespaceScoped() {
		cacherConfig.KeyFunc = func(obj runtime.Object) (string, error) {
			return storage.NamespaceKeyFunc(resourcePrefix, obj)
		}
	} else {
		cacherConfig.KeyFunc = func(obj runtime.Object) (string, error) {
			return storage.NoNamespaceKeyFunc(resourcePrefix, obj)
		}
	}
	// 根据之前初始化的Cacher的config，进行cacher创建
	// 比较关键，后面进行介绍
	cacher := storage.NewCacherFromConfig(cacherConfig)
	destroyFunc := func() {
		cacher.Stop()
		d()
	}

	return cacher, destroyFunc
}
