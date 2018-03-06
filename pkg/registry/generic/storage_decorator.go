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

package generic

import (
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/storage"
	"k8s.io/kubernetes/pkg/storage/storagebackend"
	"k8s.io/kubernetes/pkg/storage/storagebackend/factory"
)

// StorageDecorator is a function signature for producing
// a storage.Interface from given parameters.
// 资源接口的装饰者
type StorageDecorator func(
	config *storagebackend.Config,
	capacity int,
	objectType runtime.Object,
	resourcePrefix string,
	scopeStrategy rest.NamespaceScopedStrategy,
	newListFunc func() runtime.Object,
	trigger storage.TriggerPublisherFunc) (storage.Interface, factory.DestroyFunc)

// Returns given 'storageInterface' without any decoration.
func UndecoratedStorage(
	config *storagebackend.Config,
	capacity int,
	objectType runtime.Object,
	resourcePrefix string,
	scopeStrategy rest.NamespaceScopedStrategy,
	newListFunc func() runtime.Object,
	trigger storage.TriggerPublisherFunc) (storage.Interface, factory.DestroyFunc) {
	return NewRawStorage(config)
}

// NewRawStorage creates the low level kv storage. This is a work-around for current
// two layer of same storage interface.
// TODO: Once cacher is enabled on all registries (event registry is special), we will remove this method.
// 创建了一个存储后端在UndecoratedStorage和StorageWithCacher都会使用
// 一旦所有的registry的cacher都被使能，那么就会放弃UndecoratedStorage
func NewRawStorage(config *storagebackend.Config) (storage.Interface, factory.DestroyFunc) {
	//看一下这个创建函数
	// 主要看使用的是etcd2还是etcd3
	s, d, err := factory.Create(*config)
	if err != nil {
		glog.Fatalf("Unable to create storage backend: config (%v), err (%v)", config, err)
	}
	return s, d
}
