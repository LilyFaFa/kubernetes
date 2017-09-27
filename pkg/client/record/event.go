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

package record

import (
	"fmt"
	"math/rand"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/clock"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/watch"

	"net/http"

	"github.com/golang/glog"
)

const maxTriesPerEvent = 12

var defaultSleepDuration = 10 * time.Second

const maxQueuedEvents = 1000

// EventSink knows how to store events (client.Client implements it.)
// EventSink must respect the namespace that will be embedded in 'event'.
// It is assumed that EventSink will return the same sorts of errors as
// pkg/client's REST client.
// EventSink知道如何存储消息
type EventSink interface {
	Create(event *api.Event) (*api.Event, error)
	Update(event *api.Event) (*api.Event, error)
	Patch(oldEvent *api.Event, data []byte) (*api.Event, error)
}

// EventRecorder knows how to record events on behalf of an EventSource.
// 事件机制，三个方法的interface，这三个方法全都是构造事件用的
// 三个方法均是用于生成事件，并且发送给出去，只是可以接收的传入的类型不一致
// 生成的事件写进watcher的channel之后EventBroadcaster读channel并且调用相应的处理函数
// EventsRecorder主要被K8s的重要组件ControllerManager和Kubelet使用。
// 比如，负责管理注册、注销等的NodeController，
// 会将Node的状态变化信息记录为Events。DeploymentController会记录回滚、扩容等的Events。
// 他们都在ControllerManager启动时被初始化并运行。
// 与此同时Kubelet除了会记录它本身运行时的Events，
// 比如：无法为Pod挂载卷、无法带宽整型等，还包含了一系列像docker_manager这样的小单元，
//它们各司其职，并记录相关Events。

type EventRecorder interface {
	// Event constructs an event from the given information and puts it in the queue for sending.
	// 'object' is the object this event is about. Event will make a reference-- or you may also
	// pass a reference to the object directly.
	// 'type' of this event, and can be one of Normal, Warning. New types could be added in future
	// 'reason' is the reason this event is generated. 'reason' should be short and unique; it
	// should be in UpperCamelCase format (starting with a capital letter). "reason" will be used
	// to automate handling of events, so imagine people writing switch statements to handle them.
	// You want to make that easy.
	// 'message' is intended to be human readable.
	//
	// The resulting event will be created in the same namespace as the reference object.
	//Event记录了事件并且放进队列中等待发送
	Event(object runtime.Object, eventtype, reason, message string)

	// Eventf is just like Event, but with Sprintf for the message field.
	// Eventf 类似于Event，但是封装了类似 Printf 的信息打印机制，内部也会调用 Event
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})

	// PastEventf is just like Eventf, but with an option to specify the event's 'timestamp' field.
	// PastEventf 类似于Eventf，允许用户传进来自定义的时间戳，因此可以设置事件产生的时间
	PastEventf(object runtime.Object, timestamp unversioned.Time, eventtype, reason, messageFmt string, args ...interface{})
}

// EventBroadcaster knows how to receive events and send them to any EventSink, watcher, or log.
// 事件广播
// EventBroadcaster 定义了三个 Start 开头的方法，它们用来添加事件处理 handler
// EventBroadcaster 的工作原理就比较清晰了：它通过 EventRecorder 提供接口供用户写事件，
// recoder接收到的事件发送给处理函数。
// 处理函数是可以扩展的，
// 用户可以通过 StartEventWatcher 来编写自己的事件处理逻辑，
// kubelet 默认会使用 StartRecordingToSink 和 StartLogging，
// 也就是说任何一个事件会同时发送给 apiserver，并打印到日志中。
// 问题：
// EventRecorder 是怎么把事件发送给 EventBroadcaster 的？
// EventBroadcaster 是怎么实现事件广播的？
// StartRecodingToSink 内部是如何把事件发送到 apiserver 的？
type EventBroadcaster interface {
	// StartEventWatcher starts sending events received from this EventBroadcaster to the given
	// event handler function. The return value can be ignored or used to stop recording, if
	// desired.
	//核心方法是 StartEventWatcher，它会在后台启动一个 goroutine，
	//不断从 EventBroadcaster 提供的管道中接收事件，
	//然后调用 eventHandler 处理函数对事件进行处理。
	StartEventWatcher(eventHandler func(*api.Event)) watch.Interface

	// StartRecordingToSink starts sending events received from this EventBroadcaster to the given
	// sink. The return value can be ignored or used to stop recording, if desired.

	// StartRecordingToSink 和 StartLogging 是对 StartEventWatcher 的封装，
	// 分别实现了不同的处理函数，然后将这个处理函数作为参数调用StartEventWatcher
	// 发送给 apiserver
	// 从sventBroadcaster接收信息并且发给apiserver
	StartRecordingToSink(sink EventSink) watch.Interface

	// StartLogging starts sending events received from this EventBroadcaster to the given logging
	// function. The return value can be ignored or used to stop recording, if desired.
	// StartRecordingToSink 和 StartLogging 是对 StartEventWatcher 的封装，
	// 分别实现了不同的处理函数，然后将这个处理函数作为参数嗲用StartEventWatcher
	// 从eventBroadCaster接收信息并且给给定的logging
	// 写日志
	StartLogging(logf func(format string, args ...interface{})) watch.Interface

	// NewRecorder returns an EventRecorder that can be used to send events to this EventBroadcaster
	// with the event source set to the given event source.
	// 创建一个事件机制，类似于一个事件记录仪，
	// 用户可以通过它记录事件，它在内部会把事件发送给 EventBroadcaster watch channel
	// EventBroadcaster从channel中读取事件并且调用处理函数处理事件
	NewRecorder(source api.EventSource) EventRecorder
}

// Creates a new event broadcaster.
func NewBroadcaster() EventBroadcaster {
	return &eventBroadcasterImpl{watch.NewBroadcaster(maxQueuedEvents, watch.DropIfChannelFull), defaultSleepDuration}
}

func NewBroadcasterForTests(sleepDuration time.Duration) EventBroadcaster {
	return &eventBroadcasterImpl{watch.NewBroadcaster(maxQueuedEvents, watch.DropIfChannelFull), sleepDuration}
}

// 实现接口EventBroadcaster
// 它的核心组件是 watch.Broadcaster，Broadcaster 就是广播的意思，
// 主要功能就是把发给它的消息，广播给所有的监听者（watcher）
// 它的实现代码在 pkg/watch/mux.go，我们不再深入剖析，
// 不过这部分代码如何使用 golang channel 是值得所有读者学习的。
type eventBroadcasterImpl struct {
	//watch.Broadcaster 是一个分发器，内部保存了一个消息队列，
	//可以通过 Watch 创建监听它内部的 worker。
	//当有消息发送到队列中，watch.Broadcaster 后台运行的 goroutine
	//会接收消息并发送给所有的 watcher。
	//而每个 watcher 都有一个接收消息的 channel，
	//用户可以通过它的 ResultChan() 获取这个 channel 从中读取数据进行处理。
	*watch.Broadcaster
	sleepDuration time.Duration
}

// StartRecordingToSink starts sending events received from the specified eventBroadcaster to the given sink.
// The return value can be ignored or used to stop recording, if desired.
// TODO: make me an object with parameterizable queue length and retry interval
// StartRecordingToSink 就是对 StartEventWatcher 的封装，
// 将处理函数设置为 recordToSink
// 也就是发送给apiserver
func (eventBroadcaster *eventBroadcasterImpl) StartRecordingToSink(sink EventSink) watch.Interface {
	// The default math/rand package functions aren't thread safe, so create a
	// new Rand object for each StartRecording call.
	randGen := rand.New(rand.NewSource(time.Now().UnixNano()))
	eventCorrelator := NewEventCorrelator(clock.RealClock{})
	return eventBroadcaster.StartEventWatcher(
		func(event *api.Event) {
			recordToSink(sink, event, eventCorrelator, randGen, eventBroadcaster.sleepDuration)
		})
}

//recordToSink 负责把事件发送到 apiserver，
//这里的 sink 其实就是和 apiserver 交互的 restclient，
//event 是要发送的事件，eventCorrelator 在发送事件之前先对事件进行预处理。
func recordToSink(sink EventSink, event *api.Event, eventCorrelator *EventCorrelator, randGen *rand.Rand, sleepDuration time.Duration) {
	// Make a copy before modification, because there could be multiple listeners.
	// Events are safe to copy like this.
	//因为事件有很多watcher，所以对事件进行处理之前进行copy一份
	eventCopy := *event
	event = &eventCopy
	//eventCorrelator.EventCorrelate 会对事件做预处理，
	//主要是聚合相同的事件（避免产生的事件过多，增加 etcd 和 apiserver 的压力，
	//也会导致查看 pod 事件很不清晰
	//看一下处理逻辑
	result, err := eventCorrelator.EventCorrelatea(event)
	if err != nil {
		utilruntime.HandleError(err)
	}
	if result.Skip {
		return
	}
	tries := 0
	for {
		//处理事件
		//负责最终把事件发送到 apiserver，它会重试很多次（默认是 12 次），
		//并且每次重试都有一定时间间隔（默认是 10 秒钟）
		if recordEvent(sink, result.Event, result.Patch, result.Event.Count > 1, eventCorrelator) {
			break
		}
		tries++
		if tries >= maxTriesPerEvent {
			glog.Errorf("Unable to write event '%#v' (retry limit exceeded!)", event)
			break
		}
		// Randomize the first sleep so that various clients won't all be
		// synced up if the master goes down.
		if tries == 1 {
			time.Sleep(time.Duration(float64(sleepDuration) * randGen.Float64()))
		} else {
			time.Sleep(sleepDuration)
		}
	}
}

func isKeyNotFoundError(err error) bool {
	statusErr, _ := err.(*errors.StatusError)

	if statusErr != nil && statusErr.Status().Code == http.StatusNotFound {
		return true
	}

	return false
}

// recordEvent attempts to write event to a sink. It returns true if the event
// was successfully recorded or discarded, false if it should be retried.
// If updateExistingEvent is false, it creates a new event, otherwise it updates
// existing event.
func recordEvent(sink EventSink, event *api.Event, patch []byte, updateExistingEvent bool, eventCorrelator *EventCorrelator) bool {
	var newEvent *api.Event
	var err error
	// 更新已经存在的事件
	if updateExistingEvent {
		//sink.Patch 是自动生成的 apiserver 的 client
		newEvent, err = sink.Patch(event, patch)
	}
	// Update can fail because the event may have been removed and it no longer exists.
	// 创建一个新的事件
	if !updateExistingEvent || (updateExistingEvent && isKeyNotFoundError(err)) {
		// Making sure that ResourceVersion is empty on creation
		event.ResourceVersion = ""
		//sink.Create 和 sink.Patch 是自动生成的 apiserver 的 client
		newEvent, err = sink.Create(event)
	}
	if err == nil {
		// we need to update our event correlator with the server returned state to handle name/resourceversion
		eventCorrelator.UpdateState(newEvent)
		return true
	}

	// If we can't contact the server, then hold everything while we keep trying.
	// Otherwise, something about the event is malformed and we should abandon it.
	// 如果是已知错误，就不要再重试了；否则，返回 false，让上层进行重试
	switch err.(type) {
	case *restclient.RequestConstructionError:
		// We will construct the request the same next time, so don't keep trying.
		glog.Errorf("Unable to construct event '%#v': '%v' (will not retry!)", event, err)
		return true
	case *errors.StatusError:
		if errors.IsAlreadyExists(err) {
			glog.V(5).Infof("Server rejected event '%#v': '%v' (will not retry!)", event, err)
		} else {
			glog.Errorf("Server rejected event '%#v': '%v' (will not retry!)", event, err)
		}
		return true
	case *errors.UnexpectedObjectError:
		// We don't expect this; it implies the server's response didn't match a
		// known pattern. Go ahead and retry.
	default:
		// This case includes actual http transport errors. Go ahead and retry.
	}
	glog.Errorf("Unable to write event: '%v' (may retry after sleeping)", err)
	return false
}

// StartLogging starts sending events received from this EventBroadcaster to the given logging function.
// The return value can be ignored or used to stop recording, if desired.
func (eventBroadcaster *eventBroadcasterImpl) StartLogging(logf func(format string, args ...interface{})) watch.Interface {
	return eventBroadcaster.StartEventWatcher(
		func(e *api.Event) {
			logf("Event(%#v): type: '%v' reason: '%v' %v", e.InvolvedObject, e.Type, e.Reason, e.Message)
		})
}

// StartEventWatcher starts sending events received from this EventBroadcaster to the given event handler function.
// The return value can be ignored or used to stop recording, if desired.
// 它启动一个 goroutine，不断从 watcher.ResultChan() 中读取消息，
// 然后调用 eventHandler(event) 对事件进行处理。
func (eventBroadcaster *eventBroadcasterImpl) StartEventWatcher(eventHandler func(*api.Event)) watch.Interface {
	//实例化一个watcher，开始接收信息
	watcher := eventBroadcaster.Watch()
	//定义一个go routine，用来监视Broadcaster发来的Events
	go func() {
		defer utilruntime.HandleCrash()
		for {
			//如果watcher中监听到消息
			watchEvent, open := <-watcher.ResultChan()
			if !open {
				return
			}
			event, ok := watchEvent.Object.(*api.Event)
			if !ok {
				// This is all local, so there's no reason this should
				// ever happen.
				continue
			}
			//使用指定的处理函数对消息进行处理，写日志或者是发送给apiserver
			eventHandler(event)
		}
	}()
	return watcher
}

// NewRecorder returns an EventRecorder that records events with the given event source.
func (eventBroadcaster *eventBroadcasterImpl) NewRecorder(source api.EventSource) EventRecorder {
	return &recorderImpl{source, eventBroadcaster.Broadcaster, clock.RealClock{}}
}

//实现EventRecoder接口的结构体
type recorderImpl struct {
	//EventSource 指明了哪个节点的哪个组件
	//eventBroadcaster.NewRecorder 会创建一个指定 EventSource 的 EventRecorder
	source api.EventSource
	*watch.Broadcaster
	clock clock.Clock
}

//generateEvent 就是根据传入的参数，生成一个 api.Event 对象，并发送出去
//参数分析
//object：哪个组件/对象发出的事件
//timestamp：事件产生的时间
//eventtype：事件类型，目前有两种：Normal 和 Warning，分别代表正常的事件和可能有问题的事件，定义在 pkg/api/types.go 文件中，未来可能有其他类型的事件扩展
//reason：  事件产生的原因，可以在 pkg/kubelet/events/event.go 看到 kubelet 定义的所有事件类型
//message：事件的具体内容，用户可以理解的语句
func (recorder *recorderImpl) generateEvent(object runtime.Object, timestamp unversioned.Time, eventtype, reason, message string) {
	ref, err := api.GetReference(object)
	if err != nil {
		glog.Errorf("Could not construct reference to: '%#v' due to: '%v'. Will not report event: '%v' '%v' '%v'", object, err, eventtype, reason, message)
		return
	}
	//支持normal和warning
	if !validateEventType(eventtype) {
		glog.Errorf("Unsupported event type: '%v'", eventtype)
		return
	}
	//创建事件
	event := recorder.makeEvent(ref, eventtype, reason, message)
	event.Source = recorder.source

	go func() {
		// NOTE: events should be a non-blocking operation
		defer utilruntime.HandleCrash()
		//发送事件
		//把对象封装一下，发送到 m.incoming 管道
		recorder.Action(watch.Added, event)
	}()
}

func validateEventType(eventtype string) bool {
	switch eventtype {
  //支持两类events
	case api.EventTypeNormal, api.EventTypeWarning:
		return true
	}
	return false
}

func (recorder *recorderImpl) Event(object runtime.Object, eventtype, reason, message string) {
	recorder.generateEvent(object, unversioned.Now(), eventtype, reason, message)
}

func (recorder *recorderImpl) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (recorder *recorderImpl) PastEventf(object runtime.Object, timestamp unversioned.Time, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.generateEvent(object, timestamp, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

//这个格式和kubectl get events的json格式一致
func (recorder *recorderImpl) makeEvent(ref *api.ObjectReference, eventtype, reason, message string) *api.Event {
	t := unversioned.Time{Time: recorder.clock.Now()}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = api.NamespaceDefault
	}
	return &api.Event{
		ObjectMeta: api.ObjectMeta{
			//事件关联对象的名字和当前的时间，中间用点隔开。
			Name:      fmt.Sprintf("%v.%x", ref.Name, t.UnixNano()),
			Namespace: namespace,
		},
		InvolvedObject: *ref,
		Reason:         reason,
		Message:        message,
		FirstTimestamp: t,
		LastTimestamp:  t,
		Count:          1,
		Type:           eventtype,
	}
}s
