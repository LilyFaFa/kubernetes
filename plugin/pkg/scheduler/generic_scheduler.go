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

package scheduler

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/workqueue"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
	schedulerapi "k8s.io/kubernetes/plugin/pkg/scheduler/api"
	"k8s.io/kubernetes/plugin/pkg/scheduler/schedulercache"
)

type FailedPredicateMap map[string][]algorithm.PredicateFailureReason

type FitError struct {
	Pod              *api.Pod
	FailedPredicates FailedPredicateMap
}

var ErrNoNodesAvailable = fmt.Errorf("no nodes available to schedule pods")

// Error returns detailed information of why the pod failed to fit on each node
func (f *FitError) Error() string {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("pod (%s) failed to fit in any node\n", f.Pod.Name))
	reasons := make(map[string]int)
	for _, predicates := range f.FailedPredicates {
		for _, pred := range predicates {
			reasons[pred.GetReason()] += 1
		}
	}

	sortReasonsHistogram := func() []string {
		reasonStrings := []string{}
		for k, v := range reasons {
			reasonStrings = append(reasonStrings, fmt.Sprintf("%v (%v)", k, v))
		}
		sort.Strings(reasonStrings)
		return reasonStrings
	}

	reasonMsg := fmt.Sprintf("fit failure summary on nodes : %v", strings.Join(sortReasonsHistogram(), ", "))
	buf.WriteString(reasonMsg)
	return buf.String()
}

type genericScheduler struct {
	cache                 schedulercache.Cache
	predicates            map[string]algorithm.FitPredicate
	priorityMetaProducer  algorithm.MetadataProducer
	predicateMetaProducer algorithm.MetadataProducer
	prioritizers          []algorithm.PriorityConfig
	extenders             []algorithm.SchedulerExtender
	pods                  algorithm.PodLister
	lastNodeIndexLock     sync.Mutex
	lastNodeIndex         uint64

	cachedNodeInfoMap map[string]*schedulercache.NodeInfo

	equivalenceCache *EquivalenceCache
}

// Schedule tries to schedule the given pod to one of node in the node list.
// If it succeeds, it will return the name of the node.
// If it fails, it will return a Fiterror error with reasons.
// 默认的schduler
func (g *genericScheduler) Schedule(pod *api.Pod, nodeLister algorithm.NodeLister) (string, error) {
	var trace *util.Trace
	if pod != nil {
		trace = util.NewTrace(fmt.Sprintf("Scheduling %s/%s", pod.Namespace, pod.Name))
	} else {
		trace = util.NewTrace("Scheduling <nil> pod")
	}
	defer trace.LogIfLong(100 * time.Millisecond)

	//获取可以被调度的node列表
	nodes, err := nodeLister.List()
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		return "", ErrNoNodesAvailable
	}

	// Used for all fit and priority funcs.
	err = g.cache.UpdateNodeNameToInfoMap(g.cachedNodeInfoMap)
	if err != nil {
		return "", err
	}

	// TODO(harryz) Check if equivalenceCache is enabled and call scheduleWithEquivalenceClass here

	//开始预选
	//看一下预选过程
	trace.Step("Computing predicates")
	filteredNodes, failedPredicateMap, err := findNodesThatFit(pod, g.cachedNodeInfoMap, nodes, g.predicates, g.extenders, g.predicateMetaProducer)
	if err != nil {
		return "", err
	}
	//如果没有通过预选的节点，就直接返回说没有节点可以调度这个pod
	if len(filteredNodes) == 0 {
		return "", &FitError{
			Pod:              pod,
			FailedPredicates: failedPredicateMap,
		}
	}
	//开始优选打分
	trace.Step("Prioritizing")
	metaPrioritiesInterface := g.priorityMetaProducer(pod, g.cachedNodeInfoMap)
	//返回优选打分后的列表
	//看一下这个过程
	priorityList, err := PrioritizeNodes(pod, g.cachedNodeInfoMap, metaPrioritiesInterface, g.prioritizers, filteredNodes, g.extenders)
	if err != nil {
		return "", err
	}
	//从选出的pod中随机一个pod
	trace.Step("Selecting host")
	return g.selectHost(priorityList)
}

// selectHost takes a prioritized list of nodes and then picks one
// in a round-robin manner from the nodes that had the highest score.
func (g *genericScheduler) selectHost(priorityList schedulerapi.HostPriorityList) (string, error) {
	if len(priorityList) == 0 {
		return "", fmt.Errorf("empty priorityList")
	}

	sort.Sort(sort.Reverse(priorityList))
	maxScore := priorityList[0].Score
	firstAfterMaxScore := sort.Search(len(priorityList), func(i int) bool { return priorityList[i].Score < maxScore })

	g.lastNodeIndexLock.Lock()
	ix := int(g.lastNodeIndex % uint64(firstAfterMaxScore))
	g.lastNodeIndex++
	g.lastNodeIndexLock.Unlock()

	return priorityList[ix].Host, nil
}

// Filters the nodes to find the ones that fit based on the given predicate functions
// Each node is passed through the predicate functions to determine if it is a fit
func findNodesThatFit(
	pod *api.Pod,
	nodeNameToInfo map[string]*schedulercache.NodeInfo,
	nodes []*api.Node,
	predicateFuncs map[string]algorithm.FitPredicate,
	extenders []algorithm.SchedulerExtender,
	metadataProducer algorithm.MetadataProducer,
) ([]*api.Node, FailedPredicateMap, error) {
	var filtered []*api.Node
	failedPredicateMap := FailedPredicateMap{}

	if len(predicateFuncs) == 0 {
		filtered = nodes
	} else {
		// Create filtered list with enough space to avoid growing it
		// and allow assigning.
		filtered = make([]*api.Node, len(nodes))
		errs := []error{}
		var predicateResultLock sync.Mutex
		var filteredLen int32

		// We can use the same metadata producer for all nodes.
		meta := metadataProducer(pod, nodeNameToInfo)
		// checkNode会调用podFitsOnNode完成配置的所有Predicates Policies对该Node的检查。
		checkNode := func(i int) {
			nodeName := nodes[i].Name
			//对该pod运行所有的默认筛选函数
			//不符合的原因在failedPredicates
			//fits是true的时候说明该节点都可以调度这个pod
			fits, failedPredicates, err := podFitsOnNode(pod, meta, nodeNameToInfo[nodeName], predicateFuncs)
			if err != nil {
				predicateResultLock.Lock()
				errs = append(errs, err)
				predicateResultLock.Unlock()
				return
			}
			//如果经过了所有的筛选函数，那么该节点可以被调度
			if fits {
				filtered[atomic.AddInt32(&filteredLen, 1)-1] = nodes[i]
			} else {
				predicateResultLock.Lock()
				failedPredicateMap[nodeName] = failedPredicates
				predicateResultLock.Unlock()
			}
		}
		// 根据nodes数量，启动最多16个goroutine worker执行checkNode方法
		workqueue.Parallelize(16, len(nodes), checkNode)
		filtered = filtered[:filteredLen]
		if len(errs) > 0 {
			return []*api.Node{}, FailedPredicateMap{}, errors.NewAggregate(errs)
		}
	}
	// 如果配置了Extender，就是有自己的配置的算法
	// 则执行Extender的Filter逻辑继续甩选。
	if len(filtered) > 0 && len(extenders) != 0 {
		for _, extender := range extenders {
			filteredList, failedMap, err := extender.Filter(pod, filtered)
			if err != nil {
				return []*api.Node{}, FailedPredicateMap{}, err
			}

			for failedNodeName, failedMsg := range failedMap {
				if _, found := failedPredicateMap[failedNodeName]; !found {
					failedPredicateMap[failedNodeName] = []algorithm.PredicateFailureReason{}
				}
				failedPredicateMap[failedNodeName] = append(failedPredicateMap[failedNodeName], predicates.NewFailureReason(failedMsg))
			}
			filtered = filteredList
			if len(filtered) == 0 {
				break
			}
		}
	}
	//返回符合筛选条件的node以及不符合筛选条件的node被淘汰的原因
	return filtered, failedPredicateMap, nil
}

// Checks whether node with a given name and NodeInfo satisfies all predicateFuncs.
// 循环执行所有配置的Predicates Polic对应的predicateFunc。
func podFitsOnNode(pod *api.Pod, meta interface{}, info *schedulercache.NodeInfo, predicateFuncs map[string]algorithm.FitPredicate) (bool, []algorithm.PredicateFailureReason, error) {
	var failedPredicates []algorithm.PredicateFailureReason
	//for循环遍历predicate函数
	//使用predict作为过滤器，逐一进行筛选，一气呵成，
	//这里将算法传递给数据的思想，将Golang的思维展示得淋漓尽致！
	for _, predicate := range predicateFuncs {
		fit, reasons, err := predicate(pod, meta, info)
		if err != nil {
			err := fmt.Errorf("SchedulerPredicates failed due to %v, which is unexpected.", err)
			return false, []algorithm.PredicateFailureReason{}, err
		}
		if !fit {
			failedPredicates = append(failedPredicates, reasons...)
		}
	}
	return len(failedPredicates) == 0, failedPredicates, nil
}

// Prioritizes the nodes by running the individual priority functions in parallel.
// Each priority function is expected to set a score of 0-10
// 0 is the lowest priority score (least preferred node) and 10 is the highest
// Each priority function can also have its own weight
// The node scores returned by the priority function are multiplied by the weights to get weighted scores
// All scores are finally combined (added) to get the total weighted scores of all nodes
// 根据所有配置到Priorities Policies对所有预选后的Nodes进行优选打分
// 每个Priorities policy对每个node打分范围为0-10分，分越高表示越合适
func PrioritizeNodes(
	pod *api.Pod,
	nodeNameToInfo map[string]*schedulercache.NodeInfo,
	meta interface{},
	priorityConfigs []algorithm.PriorityConfig,
	nodes []*api.Node,
	extenders []algorithm.SchedulerExtender,
) (schedulerapi.HostPriorityList, error) {
	// If no priority configs are provided, then the EqualPriority function is applied
	// This is required to generate the priority list in the required format
	if len(priorityConfigs) == 0 && len(extenders) == 0 {
		result := make(schedulerapi.HostPriorityList, 0, len(nodes))
		for i := range nodes {
			hostPriority, err := EqualPriorityMap(pod, meta, nodeNameToInfo[nodes[i].Name])
			if err != nil {
				return nil, err
			}
			result = append(result, hostPriority)
		}
		return result, nil
	}

	var (
		mu   = sync.Mutex{}
		wg   = sync.WaitGroup{}
		errs []error
	)
	appendError := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}

	results := make([]schedulerapi.HostPriorityList, 0, len(priorityConfigs))
	for range priorityConfigs {
		results = append(results, nil)
	}
	for i, priorityConfig := range priorityConfigs {
		if priorityConfig.Function != nil {
			// DEPRECATED
			wg.Add(1)
			go func(index int, config algorithm.PriorityConfig) {
				defer wg.Done()
				var err error
				results[index], err = config.Function(pod, nodeNameToInfo, nodes)
				if err != nil {
					appendError(err)
				}
			}(i, priorityConfig)
		} else {
			results[i] = make(schedulerapi.HostPriorityList, len(nodes))
		}
	}
	// 对单个node遍历所有的Priorities Policies，得到每个node每个policy打分的二维数据数据
	processNode := func(index int) {
		nodeInfo := nodeNameToInfo[nodes[index].Name]
		var err error
		for i := range priorityConfigs {
			if priorityConfigs[i].Function != nil {
				continue
			}
			results[i][index], err = priorityConfigs[i].Map(pod, meta, nodeInfo)
			if err != nil {
				appendError(err)
				return
			}
		}
	}
	// 根据nodes数量，启动最多16个goroutine worker执行processNode方法
	workqueue.Parallelize(16, len(nodes), processNode)
	// 遍历所有配置的Priorities policies，
	// 如果某个policy配置了Reduce，则执行对应的Reduce，更新result[node][policy]得分
	for i, priorityConfig := range priorityConfigs {
		if priorityConfig.Reduce == nil {
			continue
		}
		wg.Add(1)
		go func(index int, config algorithm.PriorityConfig) {
			defer wg.Done()
			if err := config.Reduce(pod, meta, nodeNameToInfo, results[index]); err != nil {
				appendError(err)
			}
		}(i, priorityConfig)
	}
	// Wait for all computations to be finished.
	wg.Wait()
	if len(errs) != 0 {
		return schedulerapi.HostPriorityList{}, errors.NewAggregate(errs)
	}

	// Summarize all scores.
	// 对得分进行加权求和得到最终分数
	result := make(schedulerapi.HostPriorityList, 0, len(nodes))
	// TODO: Consider parallelizing it.
	for i := range nodes {
		result = append(result, schedulerapi.HostPriority{Host: nodes[i].Name, Score: 0})
		for j := range priorityConfigs {
			result[i].Score += results[j][i].Score * priorityConfigs[j].Weight
		}
	}

	// 如果配置了Extender，则再执行Extender的优选打分方法Extender.Prioritize
	if len(extenders) != 0 && nodes != nil {
		combinedScores := make(map[string]int, len(nodeNameToInfo))
		for _, extender := range extenders {
			wg.Add(1)
			go func(ext algorithm.SchedulerExtender) {
				defer wg.Done()
				prioritizedList, weight, err := ext.Prioritize(pod, nodes)
				if err != nil {
					// Prioritization errors from extender can be ignored, let k8s/other extenders determine the priorities
					return
				}
				mu.Lock()
				for i := range *prioritizedList {
					host, score := (*prioritizedList)[i].Host, (*prioritizedList)[i].Score
					combinedScores[host] += score * weight
				}
				mu.Unlock()
			}(extender)
		}
		// wait for all go routines to finish
		wg.Wait()
		for i := range result {
			result[i].Score += combinedScores[result[i].Host]
		}
	}

	if glog.V(10) {
		for i := range result {
			glog.V(10).Infof("Host %s => Score %d", result[i].Host, result[i].Score)
		}
	}
	return result, nil
}

// EqualPriority is a prioritizer function that gives an equal weight of one to all nodes
func EqualPriorityMap(_ *api.Pod, _ interface{}, nodeInfo *schedulercache.NodeInfo) (schedulerapi.HostPriority, error) {
	node := nodeInfo.Node()
	if node == nil {
		return schedulerapi.HostPriority{}, fmt.Errorf("node not found")
	}
	return schedulerapi.HostPriority{
		Host:  node.Name,
		Score: 1,
	}, nil
}

func NewGenericScheduler(
	cache schedulercache.Cache,
	predicates map[string]algorithm.FitPredicate,
	predicateMetaProducer algorithm.MetadataProducer,
	prioritizers []algorithm.PriorityConfig,
	priorityMetaProducer algorithm.MetadataProducer,
	extenders []algorithm.SchedulerExtender) algorithm.ScheduleAlgorithm {
	return &genericScheduler{
		cache:                 cache,
		predicates:            predicates,
		predicateMetaProducer: predicateMetaProducer,
		prioritizers:          prioritizers,
		priorityMetaProducer:  priorityMetaProducer,
		extenders:             extenders,
		cachedNodeInfoMap:     make(map[string]*schedulercache.NodeInfo),
	}
}
