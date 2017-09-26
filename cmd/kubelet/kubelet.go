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

// The kubelet binary is responsible for maintaining a set of containers on a particular host VM.
// It syncs data from both configuration file(s) as well as from a quorum of etcd servers.
// It then queries Docker to see what is currently running.  It synchronizes the configuration data,
// with the running set of containers by starting or stopping Docker containers.
package main

import (
	"fmt"
	"os"

	"k8s.io/kubernetes/cmd/kubelet/app"
	"k8s.io/kubernetes/cmd/kubelet/app/options"
	_ "k8s.io/kubernetes/pkg/client/metrics/prometheus" // for client metric registration
	"k8s.io/kubernetes/pkg/util/flag"
	"k8s.io/kubernetes/pkg/util/logs"
	_ "k8s.io/kubernetes/pkg/version/prometheus" // for version metric registration
	"k8s.io/kubernetes/pkg/version/verflag"

	"github.com/spf13/pflag"
)

func main() {
	//创建一个kubeletServer，kubeletServer对kubelet运行的所有参数进行了封装。
	s := options.NewKubeletServer()
	s.AddFlags(pflag.CommandLine)

	flag.InitFlags()
	logs.InitLogs()
	defer logs.FlushLogs()

	verflag.PrintAndExitIfRequested()
	//运行实际的kubelet，这个方法会一直运行，正常情况下不会退出。
	if err := app.Run(s, nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
