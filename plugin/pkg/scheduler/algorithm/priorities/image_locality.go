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

package priorities

import (
	"fmt"

	"k8s.io/kubernetes/pkg/api"
	schedulerapi "k8s.io/kubernetes/plugin/pkg/scheduler/api"
	"k8s.io/kubernetes/plugin/pkg/scheduler/schedulercache"
)

// ImageLocalityPriority is a priority function that favors nodes that already have requested pod container's images.
// It will detect whether the requested images are present on a node, and then calculate a score ranging from 0 to 10
// based on the total size of those images.
// - If none of the images are present, this node will be given the lowest priority.
// - If some of the images are present on a node, the larger their sizes' sum, the higher the node's priority.
// 这个算法是根据Image是否在Node上及Image的大小，来为每个node打分
func ImageLocalityPriorityMap(pod *api.Pod, meta interface{}, nodeInfo *schedulercache.NodeInfo) (schedulerapi.HostPriority, error) {
	node := nodeInfo.Node()
	if node == nil {
		return schedulerapi.HostPriority{}, fmt.Errorf("node not found")
	}

	var sumSize int64
	for i := range pod.Spec.Containers {
		//检测pod的container需要的镜像是否在节点上
		//如果在这个节点上，那么返回这个镜像的大小
		sumSize += checkContainerImageOnNode(node, &pod.Spec.Containers[i])
	}
	return schedulerapi.HostPriority{
		Host: node.Name,
		// sumSize是指pod需要的镜像已经存在在这个pod上的大小
		// 值是所有存在的镜像的大小的总和
		Score: calculateScoreFromSize(sumSize),
	}, nil
}

// calculateScoreFromSize calculates the priority of a node. sumSize is sum size of requested images on this node.
// 1. Split image size range into 10 buckets.
// 2. Decide the priority of a given sumSize based on which bucket it belongs to.
// 将镜像的大小范围分成10个buckets，看一下节点上已经存在的镜像在哪个范围
// minImgSize int64 = 23 * mb
// maxImgSize int64 = 1000 * mb
func calculateScoreFromSize(sumSize int64) int {
	var score int
	switch {
	case sumSize == 0 || sumSize < minImgSize:
		// score == 0 means none of the images required by this pod are present on this
		// node or the total size of the images present is too small to be taken into further consideration.
		score = 0
	// If existing images' total size is larger than max, just make it highest priority.
	case sumSize >= maxImgSize:
		score = 10
	default:
		score = int((10 * (sumSize - minImgSize) / (maxImgSize - minImgSize)) + 1)
	}
	// Return which bucket the given size belongs to
	return score
}

// checkContainerImageOnNode checks if a container image is present on a node and returns its size.
func checkContainerImageOnNode(node *api.Node, container *api.Container) int64 {
	for _, image := range node.Status.Images {
		for _, name := range image.Names {
			if container.Image == name {
				// Should return immediately.
				//如果镜像存在节点上，那么就返回镜像的大小
				return image.SizeBytes
			}
		}
	}
	return 0
}
