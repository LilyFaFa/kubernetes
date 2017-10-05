/*
Copyright 2016 The Kubernetes Authors.

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

package main

import (
	"github.com/golang/glog"
	"github.com/spf13/pflag"
	"k8s.io/kubernetes/cmd/kube-dns/app"
	"k8s.io/kubernetes/cmd/kube-dns/app/options"
	_ "k8s.io/kubernetes/pkg/client/metrics/prometheus" // for client metric registration
	"k8s.io/kubernetes/pkg/util/flag"
	"k8s.io/kubernetes/pkg/util/logs"
	"k8s.io/kubernetes/pkg/version"
	_ "k8s.io/kubernetes/pkg/version/prometheus" // for version metric registration
	"k8s.io/kubernetes/pkg/version/verflag"
)

//kubedns
//监视k8s Service资源并更新DNS记录
//替换etcd，使用TreeCache数据结构保存DNS记录并实现SkyDNS的Backend接口
//接入SkyDNS，对dnsmasq提供DNS查询服务
//dnsmasq
//对集群提供DNS查询服务
//设置kubedns为upstream
//提供DNS缓存，降低kubedns负载，提高性能
func main() {
	config := options.NewKubeDNSConfig()
	//然后再通过AddFlags制定用户自己的参数，当然会覆盖上面的默认参数，代码如下
	config.AddFlags(pflag.CommandLine)

	flag.InitFlags()
	logs.InitLogs()
	defer logs.FlushLogs()

	verflag.PrintAndExitIfRequested()

	glog.V(0).Infof("version: %+v", version.Get())
	//创建服务，创建kubeDNS，并且创建service和endpoint的listwatch
	server := app.NewKubeDNSServerDefault(config)
	//启动服务
	server.Run()
}
