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

package images

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/container"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/wait"
)

// Manages lifecycle of all images.
//
// Implementation is thread-safe.
type ImageGCManager interface {
	// Applies the garbage collection policy. Errors include being unable to free
	// enough space as per the garbage collection policy.
	GarbageCollect() error

	// Start async garbage collection of images.
	Start() error

	GetImageList() ([]kubecontainer.Image, error)

	// Delete all unused images and returns the number of bytes freed. The number of bytes freed is always returned.
	DeleteUnusedImages() (int64, error)
}

// A policy for garbage collecting images. Policy defines an allowed band in
// which garbage collection will be run.
type ImageGCPolicy struct {
	// Any usage above this threshold will always trigger garbage collection.
	// This is the highest usage we will allow.
	HighThresholdPercent int

	// Any usage below this threshold will never trigger garbage collection.
	// This is the lowest threshold we will try to garbage collect to.
	LowThresholdPercent int

	// Minimum age at which an image can be garbage collected.
	MinAge time.Duration
}

//用于缓存当前节点使用的镜像信息，
//并在 Start() 方法中启动两个 goroutine 周期性地去更新缓存的内容。
type realImageGCManager struct {
	// Container runtime
	runtime container.Runtime

	// Records of images and their use.
	imageRecords     map[string]*imageRecord
	imageRecordsLock sync.Mutex

	// The image garbage collection policy in use.
	policy ImageGCPolicy

	// cAdvisor instance.
	cadvisor cadvisor.Interface

	// Recorder for Kubernetes events.
	recorder record.EventRecorder

	// Reference to this node.
	nodeRef *api.ObjectReference

	// Track initialization
	initialized bool

	// imageCache is the cache of latest image list.
	imageCache imageCache
}

// imageCache caches latest result of ListImages.
type imageCache struct {
	// sync.RWMutex is the mutex protects the image cache.
	sync.RWMutex
	// images is the image cache.
	images []kubecontainer.Image
}

// set updates image cache.
func (i *imageCache) set(images []kubecontainer.Image) {
	i.Lock()
	defer i.Unlock()
	i.images = images
}

// get gets image list from image cache.
func (i *imageCache) get() []kubecontainer.Image {
	i.RLock()
	defer i.RUnlock()
	return i.images
}

// Information about the images we track.
type imageRecord struct {
	// Time when this image was first detected.
	firstDetected time.Time

	// Time when we last saw this image being used.
	lastUsed time.Time

	// Size of the image in bytes.
	size int64
}

func NewImageGCManager(runtime container.Runtime, cadvisorInterface cadvisor.Interface, recorder record.EventRecorder, nodeRef *api.ObjectReference, policy ImageGCPolicy) (ImageGCManager, error) {
	// Validate policy.
	if policy.HighThresholdPercent < 0 || policy.HighThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid HighThresholdPercent %d, must be in range [0-100]", policy.HighThresholdPercent)
	}
	if policy.LowThresholdPercent < 0 || policy.LowThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid LowThresholdPercent %d, must be in range [0-100]", policy.LowThresholdPercent)
	}
	if policy.LowThresholdPercent > policy.HighThresholdPercent {
		return nil, fmt.Errorf("LowThresholdPercent %d can not be higher than HighThresholdPercent %d", policy.LowThresholdPercent, policy.HighThresholdPercent)
	}
	im := &realImageGCManager{
		runtime:      runtime,
		policy:       policy,
		imageRecords: make(map[string]*imageRecord),
		cadvisor:     cadvisorInterface,
		recorder:     recorder,
		nodeRef:      nodeRef,
		initialized:  false,
	}

	return im, nil
}

func (im *realImageGCManager) Start() error {
	go wait.Until(func() {
		// Initial detection make detected time "unknown" in the past.
		var ts time.Time
		if im.initialized {
			ts = time.Now()
		}
		err := im.detectImages(ts)
		if err != nil {
			glog.Warningf("[imageGCManager] Failed to monitor images: %v", err)
		} else {
			im.initialized = true
		}
	}, 5*time.Minute, wait.NeverStop)

	// Start a goroutine periodically updates image cache.
	// TODO(random-liu): Merge this with the previous loop.
	go wait.Until(func() {
		images, err := im.runtime.ListImages()
		if err != nil {
			glog.Warningf("[imageGCManager] Failed to update image list: %v", err)
		} else {
			im.imageCache.set(images)
		}
	}, 30*time.Second, wait.NeverStop)

	return nil
}

// Get a list of images on this node
func (im *realImageGCManager) GetImageList() ([]kubecontainer.Image, error) {
	return im.imageCache.get(), nil
}

func (im *realImageGCManager) detectImages(detectTime time.Time) error {
	images, err := im.runtime.ListImages()
	if err != nil {
		return err
	}
	pods, err := im.runtime.GetPods(true)
	if err != nil {
		return err
	}

	// Make a set of images in use by containers.
	imagesInUse := sets.NewString()
	for _, pod := range pods {
		for _, container := range pod.Containers {
			glog.V(5).Infof("Pod %s/%s, container %s uses image %s(%s)", pod.Namespace, pod.Name, container.Name, container.Image, container.ImageID)
			imagesInUse.Insert(container.ImageID)
		}
	}

	// Add new images and record those being used.
	now := time.Now()
	currentImages := sets.NewString()
	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()
	for _, image := range images {
		glog.V(5).Infof("Adding image ID %s to currentImages", image.ID)
		currentImages.Insert(image.ID)

		// New image, set it as detected now.
		if _, ok := im.imageRecords[image.ID]; !ok {
			glog.V(5).Infof("Image ID %s is new", image.ID)
			im.imageRecords[image.ID] = &imageRecord{
				firstDetected: detectTime,
			}
		}

		// Set last used time to now if the image is being used.
		if isImageUsed(image, imagesInUse) {
			glog.V(5).Infof("Setting Image ID %s lastUsed to %v", image.ID, now)
			im.imageRecords[image.ID].lastUsed = now
		}

		glog.V(5).Infof("Image ID %s has size %d", image.ID, image.Size)
		im.imageRecords[image.ID].size = image.Size
	}

	// Remove old images from our records.
	for image := range im.imageRecords {
		if !currentImages.Has(image) {
			glog.V(5).Infof("Image ID %s is no longer present; removing from imageRecords", image)
			delete(im.imageRecords, image)
		}
	}

	return nil
}

//逻辑：
//调用 cAdvisor 接口获取镜像所在磁盘的文件系统信息，根据当前的使用量和配置的 GC 策略确定是否需要进行清理
//如果需要清理，计算需要清理的总大小，调用 freeSpace 进行镜像清理工作
//把所有可以清理的镜像根据使用时间进行排序，进行逐个清理，知道清理的镜像总大小满足需求才停止
func (im *realImageGCManager) GarbageCollect() error {
	// Get disk usage on disk holding images.
	// 从 cadvisor 中获取镜像所在文件系统的信息，包括磁盘的容量和当前的使用量
	fsInfo, err := im.cadvisor.ImagesFsInfo()
	if err != nil {
		return err
	}
	capacity := int64(fsInfo.Capacity)
	available := int64(fsInfo.Available)
	if available > capacity {
		glog.Warningf("available %d is larger than capacity %d", available, capacity)
		available = capacity
	}

	// Check valid capacity.
	if capacity == 0 {
		err := fmt.Errorf("invalid capacity %d on device %q at mount point %q", capacity, fsInfo.Device, fsInfo.Mountpoint)
		im.recorder.Eventf(im.nodeRef, api.EventTypeWarning, events.InvalidDiskCapacity, err.Error())
		return err
	}

	// If over the max threshold, free enough to place us at the lower threshold.
	// 如果镜像的磁盘使用率达到了设定的最高阈值，就进行清理工作，直到使用率
	usagePercent := 100 - int(available*100/capacity)
	if usagePercent >= im.policy.HighThresholdPercent {
		amountToFree := capacity*int64(100-im.policy.LowThresholdPercent)/100 - available
		glog.Infof("[imageGCManager]: Disk usage on %q (%s) is at %d%% which is over the high threshold (%d%%). Trying to free %d bytes", fsInfo.Device, fsInfo.Mountpoint, usagePercent, im.policy.HighThresholdPercent, amountToFree)
		//进行清除
		freed, err := im.freeSpace(amountToFree, time.Now())
		if err != nil {
			return err
		}

		if freed < amountToFree {
			err := fmt.Errorf("failed to garbage collect required amount of images. Wanted to free %d, but freed %d", amountToFree, freed)
			//通知apiserver失败的原因
			im.recorder.Eventf(im.nodeRef, api.EventTypeWarning, events.FreeDiskSpaceFailed, err.Error())
			return err
		}
	}

	return nil
}

func (im *realImageGCManager) DeleteUnusedImages() (int64, error) {
	return im.freeSpace(math.MaxInt64, time.Now())
}

// Tries to free bytesToFree worth of images on the disk.
//
// Returns the number of bytes free and an error if any occurred. The number of
// bytes freed is always returned.
// Note that error may be nil and the number of bytes free may be less
// than bytesToFree.
// 清除废旧镜像
func (im *realImageGCManager) freeSpace(bytesToFree int64, freeTime time.Time) (int64, error) {
	// 更新镜像记录列表中的数据，添加刚发现的镜像，移除已经不存在的镜像
	err := im.detectImages(freeTime)
	if err != nil {
		return 0, err
	}

	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()

	// Get all images in eviction order.
	images := make([]evictionInfo, 0, len(im.imageRecords))
	for image, record := range im.imageRecords {
		images = append(images, evictionInfo{
			id:          image,
			imageRecord: *record,
		})
	}
	// 根据镜像的最近使用时间和最近发现时间进行排序
	sort.Sort(byLastUsedAndDetected(images))

	// Delete unused images until we've freed up enough space.
	//删除不再使用的镜像直至腾出足够的存储空间
	var deletionErrors []error
	spaceFreed := int64(0)
	for _, image := range images {
		glog.V(5).Infof("Evaluating image ID %s for possible garbage collection", image.id)
		// Images that are currently in used were given a newer lastUsed.
		// 忽略正在被使用的镜像
		if image.lastUsed.Equal(freeTime) || image.lastUsed.After(freeTime) {
			glog.V(5).Infof("Image ID %s has lastUsed=%v which is >= freeTime=%v, not eligible for garbage collection", image.id, image.lastUsed, freeTime)
			break
		}

		// Avoid garbage collect the image if the image is not old enough.
		// In such a case, the image may have just been pulled down, and will be used by a container right away.
		// 略过最近使用时间距离现在小于设置的 MinAge 的镜像
		if freeTime.Sub(image.firstDetected) < im.policy.MinAge {
			glog.V(5).Infof("Image ID %s has age %v which is less than the policy's minAge of %v, not eligible for garbage collection", image.id, freeTime.Sub(image.firstDetected), im.policy.MinAge)
			continue
		}

		// Remove image. Continue despite errors.
		glog.Infof("[imageGCManager]: Removing image %q to free %d bytes", image.id, image.size)
		// 删除镜像并更新 imageRecords 对象中缓存的镜像信息，记录删除的镜像大小
		err := im.runtime.RemoveImage(container.ImageSpec{Image: image.id})
		if err != nil {
			deletionErrors = append(deletionErrors, err)
			continue
		}
		delete(im.imageRecords, image.id)
		spaceFreed += image.size
		// 如果删除的镜像大小满足需求，停止继续删除，达到了policy的标准
		if spaceFreed >= bytesToFree {
			break
		}
	}

	if len(deletionErrors) > 0 {
		return spaceFreed, fmt.Errorf("wanted to free %d, but freed %d space with errors in image deletion: %v", bytesToFree, spaceFreed, errors.NewAggregate(deletionErrors))
	}
	return spaceFreed, nil
}

type evictionInfo struct {
	id string
	imageRecord
}

type byLastUsedAndDetected []evictionInfo

func (ev byLastUsedAndDetected) Len() int      { return len(ev) }
func (ev byLastUsedAndDetected) Swap(i, j int) { ev[i], ev[j] = ev[j], ev[i] }
func (ev byLastUsedAndDetected) Less(i, j int) bool {
	// Sort by last used, break ties by detected.
	if ev[i].lastUsed.Equal(ev[j].lastUsed) {
		return ev[i].firstDetected.Before(ev[j].firstDetected)
	} else {
		return ev[i].lastUsed.Before(ev[j].lastUsed)
	}
}

func isImageUsed(image container.Image, imagesInUse sets.String) bool {
	// Check the image ID.
	if _, ok := imagesInUse[image.ID]; ok {
		return true
	}
	return false
}
