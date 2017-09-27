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

package prober

import (
	"math/rand"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/util/runtime"
)

// worker handles the periodic probing of its assigned container. Each worker has a go-routine
// associated with it which runs the probe loop until the container permanently terminates, or the
// stop channel is closed. The worker uses the probe Manager's statusManager to get up-to-date
// container IDs.
// worker会周期性的在自己需要维护的容器中执行探测程序
// 每个worker会有一个goroutine
// 如果pod支持两种探针，那么每个container会有两个worker
type worker struct {
	// Channel for stopping the probe.
	stopCh chan struct{}

	// The pod containing this probe (read-only)
	pod *api.Pod

	// The container to probe (read-only)
	container api.Container

	// Describes the probe configuration (read-only)
	spec *api.Probe

	// The type of the worker.
	probeType probeType

	// The probe value during the initial delay.
	initialValue results.Result

	// Where to store this workers results.
	// 在创建pod的时候每个容器对应一个worker，用作容器的探针，保存worker的探索结果
	// 其中包含一个status参数
	resultsManager results.Manager
	probeManager   *manager

	// The last known container ID for this worker.
	containerID kubecontainer.ContainerID
	// The last probe result for this worker.
	lastResult results.Result
	// How many times in a row the probe has returned the same result.
	resultRun int

	// If set, skip probing.
	onHold bool
}

// Creates and starts a new probe worker.
// 创建一个worker，给定探针类型，contaienr所属pod以及探针所属conatienr
func newWorker(
	m *manager,
	probeType probeType,
	pod *api.Pod,
	container api.Container) *worker {

	w := &worker{
		stopCh:       make(chan struct{}, 1), // Buffer so stop() can be non-blocking.
		pod:          pod,
		container:    container,
		probeType:    probeType,
		probeManager: m,
	}

	switch probeType {
	case readiness:
		w.spec = container.ReadinessProbe
		w.resultsManager = m.readinessManager
		w.initialValue = results.Failure
	case liveness:
		w.spec = container.LivenessProbe
		w.resultsManager = m.livenessManager
		w.initialValue = results.Success
	}

	return w
}

// run periodically probes the container.
// 容器探针
func (w *worker) run() {
	probeTickerPeriod := time.Duration(w.spec.PeriodSeconds) * time.Second
	probeTicker := time.NewTicker(probeTickerPeriod)

	defer func() {
		// Clean up.
		probeTicker.Stop()
		if !w.containerID.IsEmpty() {
			w.resultsManager.Remove(w.containerID)
		}

		w.probeManager.removeWorker(w.pod.UID, w.container.Name, w.probeType)
	}()

	// If kubelet restarted the probes could be started in rapid succession.
	// Let the worker wait for a random portion of tickerPeriod before probing.
	time.Sleep(time.Duration(rand.Float64() * float64(probeTickerPeriod)))

probeLoop:
	//循环性的执行探针程序查看这个方法
	for w.doProbe() {
		// Wait for next probe tick.
		select {
		case <-w.stopCh:
			break probeLoop
		case <-probeTicker.C:
			// continue
		}
	}
}

// stop stops the probe worker. The worker handles cleanup and removes itself from its manager.
// It is safe to call stop multiple times.
func (w *worker) stop() {
	select {
	case w.stopCh <- struct{}{}:
	default: // Non-blocking.
	}
}

// doProbe probes the container once and records the result.
// Returns whether the worker should continue.
func (w *worker) doProbe() (keepGoing bool) {
	defer func() { recover() }() // Actually eat panics (HandleCrash takes care of logging)
	defer runtime.HandleCrash(func(_ interface{}) { keepGoing = true })

	// 获取pod的状态
	status, ok := w.probeManager.statusManager.GetPodStatus(w.pod.UID)
	if !ok {
		// Either the pod has not been created yet, or it was already deleted.
		glog.V(3).Infof("No status for pod: %v", format.Pod(w.pod))
		return true
	}

	// Worker should terminate if pod is terminated.
	// pod 已经退出（不管是成功还是失败），直接返回，并终止 worker
	if status.Phase == api.PodFailed || status.Phase == api.PodSucceeded {
		glog.V(3).Infof("Pod %v %v, exiting probe worker",
			format.Pod(w.pod), status.Phase)
		return false
	}
	// 容器没有创建，或者已经删除了，直接返回，并继续检测，等待更多的信息
	// 查看容器的状态信息
	c, ok := api.GetContainerStatus(status.ContainerStatuses, w.container.Name)
	if !ok || len(c.ContainerID) == 0 {
		// Either the container has not been created yet, or it was deleted.
		glog.V(3).Infof("Probe target container not found: %v - %v",
			format.Pod(w.pod), w.container.Name)
		return true // Wait for more information.
	}
	// worker的containerID和pod中的container已经不一致
	// 说明 pod 更新过容器，使用最新的容器信息
	if w.containerID.String() != c.ContainerID {
		if !w.containerID.IsEmpty() {
			// 从resultsManager移出实效的container
			w.resultsManager.Remove(w.containerID)
		}
		//更新容器的id
		w.containerID = kubecontainer.ParseContainerID(c.ContainerID)
		//初始化该容器的resultsManager
		w.resultsManager.Set(w.containerID, w.initialValue, w.pod)
		// We've got a new container; resume probing.
		w.onHold = false
	}

	if w.onHold {
		// Worker is on hold until there is a new container.
		return true
	}

	if c.State.Running == nil {
		glog.V(3).Infof("Non-running container probed: %v - %v",
			format.Pod(w.pod), w.container.Name)
		if !w.containerID.IsEmpty() {
			w.resultsManager.Set(w.containerID, results.Failure, w.pod)
		}
		// Abort if the container will not be restarted.
		// 容器失败退出，并且不会再重启，终止 worker
		return c.State.Terminated == nil ||
			w.pod.Spec.RestartPolicy != api.RestartPolicyNever
	}
	// 容器启动时间太短，没有超过配置的初始化等待时间 InitialDelaySeconds
	if int32(time.Since(c.State.Running.StartedAt.Time).Seconds()) < w.spec.InitialDelaySeconds {
		return true
	}

	// 调用 prober 进行检测容器的状态，执行探针程序
	result, err := w.probeManager.prober.probe(w.probeType, w.pod, status, w.container, w.containerID)
	if err != nil {
		// Prober error, throw away the result.
		return true
	}

	if w.lastResult == result {
		w.resultRun++
	} else {
		w.lastResult = result
		w.resultRun = 1
	}
	// 如果容器退出，并且没有超过最大的失败次数，则继续检测
	if (result == results.Failure && w.resultRun < int(w.spec.FailureThreshold)) ||
		(result == results.Success && w.resultRun < int(w.spec.SuccessThreshold)) {
		// Success or failure is below threshold - leave the probe state unchanged.
		return true
	}
	// 保存最新的检测结果
	// Set将结果写进worker的成员resultsManager的updates中也就是m.updates管道，对于liveness来说，管道的消费者就是kubelet
	// 也就是syncLoopInteration中的代码
	// kubelet从管道中得到状态变化就进行处理逻辑
	// readiness则不同，即使失败也不会重新创建pod
	// 它使用updateReadiness，定时读取readinessManager管道中的数据,
	// 并且根据数据调用statusManager更新apiserver中的pod状态信息。
	w.resultsManager.Set(w.containerID, result, w.pod)

	if w.probeType == liveness && result == results.Failure {
		// The container fails a liveness check, it will need to be restared.
		// Stop probing until we see a new container ID. This is to reduce the
		// chance of hitting #21751, where running `docker exec` when a
		// container is being stopped may lead to corrupted container state.
		// 容器 liveness 检测失败，需要删除容器并重新创建，在新容器成功创建出来之前，暂停检测
		w.onHold = true
	}

	return true
}
