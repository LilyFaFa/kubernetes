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

package record

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/groupcache/lru"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/util/clock"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/strategicpatch"
)

const (
	maxLruCacheEntries = 4096

	// if we see the same event that varies only by message
	// more than 10 times in a 10 minute period, aggregate the event
	defaultAggregateMaxEvents         = 10
	defaultAggregateIntervalInSeconds = 600
)

// getEventKey builds unique event key based on source, involvedObject, reason, message
func getEventKey(event *api.Event) string {
	return strings.Join([]string{
		event.Source.Component,
		event.Source.Host,
		event.InvolvedObject.Kind,
		event.InvolvedObject.Namespace,
		event.InvolvedObject.Name,
		string(event.InvolvedObject.UID),
		event.InvolvedObject.APIVersion,
		event.Type,
		event.Reason,
		event.Message,
	},
		"")
}

// EventFilterFunc is a function that returns true if the event should be skipped
type EventFilterFunc func(event *api.Event) bool

// DefaultEventFilterFunc returns false for all incoming events
func DefaultEventFilterFunc(event *api.Event) bool {
	return false
}

// EventAggregatorKeyFunc is responsible for grouping events for aggregation
// It returns a tuple of the following:
// aggregateKey - key the identifies the aggregate group to bucket this event
// localKey - key that makes this event in the local group
type EventAggregatorKeyFunc func(event *api.Event) (aggregateKey string, localKey string)

// EventAggregatorByReasonFunc aggregates events by exact match on event.Source, event.InvolvedObject, event.Type and event.Reason
func EventAggregatorByReasonFunc(event *api.Event) (string, string) {
	return strings.Join([]string{
		event.Source.Component,
		event.Source.Host,
		event.InvolvedObject.Kind,
		event.InvolvedObject.Namespace,
		event.InvolvedObject.Name,
		string(event.InvolvedObject.UID),
		event.InvolvedObject.APIVersion,
		event.Type,
		event.Reason,
	},
		""), event.Message
}

// EventAggregatorMessageFunc is responsible for producing an aggregation message
type EventAggregatorMessageFunc func(event *api.Event) string

// EventAggregratorByReasonMessageFunc returns an aggregate message by prefixing the incoming message
func EventAggregatorByReasonMessageFunc(event *api.Event) string {
	return "(events with common reason combined)"
}

// EventAggregator identifies similar events and aggregates them into a single event
type EventAggregator struct {
	sync.RWMutex

	// The cache that manages aggregation state
	cache *lru.Cache

	// The function that groups events for aggregation
	keyFunc EventAggregatorKeyFunc

	// The function that generates a message for an aggregate event
	messageFunc EventAggregatorMessageFunc

	// The maximum number of events in the specified interval before aggregation occurs
	maxEvents int

	// The amount of time in seconds that must transpire since the last occurrence of a similar event before it's considered new
	maxIntervalInSeconds int

	// clock is used to allow for testing over a time interval
	clock clock.Clock
}

// NewEventAggregator returns a new instance of an EventAggregator
func NewEventAggregator(lruCacheSize int, keyFunc EventAggregatorKeyFunc, messageFunc EventAggregatorMessageFunc,
	maxEvents int, maxIntervalInSeconds int, clock clock.Clock) *EventAggregator {
	return &EventAggregator{
		cache:                lru.New(lruCacheSize),
		keyFunc:              keyFunc,
		messageFunc:          messageFunc,
		maxEvents:            maxEvents,
		maxIntervalInSeconds: maxIntervalInSeconds,
		clock:                clock,
	}
}

// aggregateRecord holds data used to perform aggregation decisions
type aggregateRecord struct {
	// we track the number of unique local keys we have seen in the aggregate set to know when to actually aggregate
	// if the size of this set exceeds the max, we know we need to aggregate
	localKeys sets.String
	// The last time at which the aggregate was recorded
	lastTimestamp unversioned.Time
}

// EventAggregate identifies similar events and groups into a common event if required
func (e *EventAggregator) EventAggregate(newEvent *api.Event) (*api.Event, error) {
	aggregateKey, localKey := e.keyFunc(newEvent)
	now := unversioned.NewTime(e.clock.Now())
	record := aggregateRecord{localKeys: sets.NewString(), lastTimestamp: now}
	e.Lock()
	defer e.Unlock()
	value, found := e.cache.Get(aggregateKey)
	if found {
		record = value.(aggregateRecord)
	}

	// if the last event was far enough in the past, it is not aggregated, and we must reset state
	maxInterval := time.Duration(e.maxIntervalInSeconds) * time.Second
	interval := now.Time.Sub(record.lastTimestamp.Time)
	if interval > maxInterval {
		record = aggregateRecord{localKeys: sets.NewString()}
	}
	record.localKeys.Insert(localKey)
	record.lastTimestamp = now
	e.cache.Add(aggregateKey, record)

	if record.localKeys.Len() < e.maxEvents {
		return newEvent, nil
	}

	// do not grow our local key set any larger than max
	record.localKeys.PopAny()

	// create a new aggregate event
	eventCopy := &api.Event{
		ObjectMeta: api.ObjectMeta{
			Name:      fmt.Sprintf("%v.%x", newEvent.InvolvedObject.Name, now.UnixNano()),
			Namespace: newEvent.Namespace,
		},
		Count:          1,
		FirstTimestamp: now,
		InvolvedObject: newEvent.InvolvedObject,
		LastTimestamp:  now,
		Message:        e.messageFunc(newEvent),
		Type:           newEvent.Type,
		Reason:         newEvent.Reason,
		Source:         newEvent.Source,
	}
	return eventCopy, nil
}

// eventLog records data about when an event was observed
type eventLog struct {
	// The number of times the event has occurred since first occurrence.
	count int

	// The time at which the event was first recorded.
	firstTimestamp unversioned.Time

	// The unique name of the first occurrence of this event
	name string

	// Resource version returned from previous interaction with server
	resourceVersion string
}

// eventLogger logs occurrences of an event
type eventLogger struct {
	sync.RWMutex
	cache *lru.Cache
	clock clock.Clock
}

// newEventLogger observes events and counts their frequencies
func newEventLogger(lruCacheEntries int, clock clock.Clock) *eventLogger {
	return &eventLogger{cache: lru.New(lruCacheEntries), clock: clock}
}

// eventObserve records the event, and determines if its frequency should update
func (e *eventLogger) eventObserve(newEvent *api.Event) (*api.Event, []byte, error) {
	var (
		patch []byte
		err   error
	)
	key := getEventKey(newEvent)
	eventCopy := *newEvent
	event := &eventCopy

	e.Lock()
	defer e.Unlock()

	lastObservation := e.lastEventObservationFromCache(key)

	// we have seen this event before, so we must prepare a patch
	if lastObservation.count > 0 {
		// update the event based on the last observation so patch will work as desired
		event.Name = lastObservation.name
		event.ResourceVersion = lastObservation.resourceVersion
		event.FirstTimestamp = lastObservation.firstTimestamp
		event.Count = int32(lastObservation.count) + 1

		eventCopy2 := *event
		eventCopy2.Count = 0
		eventCopy2.LastTimestamp = unversioned.NewTime(time.Unix(0, 0))

		newData, _ := json.Marshal(event)
		oldData, _ := json.Marshal(eventCopy2)
		patch, err = strategicpatch.CreateStrategicMergePatch(oldData, newData, event)
	}

	// record our new observation
	e.cache.Add(
		key,
		eventLog{
			count:           int(event.Count),
			firstTimestamp:  event.FirstTimestamp,
			name:            event.Name,
			resourceVersion: event.ResourceVersion,
		},
	)
	return event, patch, err
}

// updateState updates its internal tracking information based on latest server state
func (e *eventLogger) updateState(event *api.Event) {
	key := getEventKey(event)
	e.Lock()
	defer e.Unlock()
	// record our new observation
	e.cache.Add(
		key,
		eventLog{
			count:           int(event.Count),
			firstTimestamp:  event.FirstTimestamp,
			name:            event.Name,
			resourceVersion: event.ResourceVersion,
		},
	)
}

// lastEventObservationFromCache returns the event from the cache, reads must be protected via external lock
func (e *eventLogger) lastEventObservationFromCache(key string) eventLog {
	value, ok := e.cache.Get(key)
	if ok {
		observationValue, ok := value.(eventLog)
		if ok {
			return observationValue
		}
	}
	return eventLog{}
}

// EventCorrelator processes all incoming events and performs analysis to avoid overwhelming the system.  It can filter all
// incoming events to see if the event should be filtered from further processing.  It can aggregate similar events that occur
// frequently to protect the system from spamming events that are difficult for users to distinguish.  It performs de-duplication
// to ensure events that are observed multiple times are compacted into a single event with increasing counts.
type EventCorrelator struct {
	// the function to filter the event
	filterFunc EventFilterFunc
	// the object that performs event aggregation
	aggregator *EventAggregator
	// the object that observes events as they come through
	logger *eventLogger
}

// EventCorrelateResult is the result of a Correlate
type EventCorrelateResult struct {
	// the event after correlation
	Event *api.Event
	// if provided, perform a strategic patch when updating the record on the server
	Patch []byte
	// if true, do no further processing of the event
	Skip bool
}

// NewEventCorrelator returns an EventCorrelator configured with default values.
//
// The EventCorrelator is responsible for event filtering, aggregating, and counting
// prior to interacting with the API server to record the event.
//
// The default behavior is as follows:
//   * No events are filtered from being recorded
//   * Aggregation is performed if a similar event is recorded 10 times in a
//     in a 10 minute rolling interval.  A similar event is an event that varies only by
//     the Event.Message field.  Rather than recording the precise event, aggregation
//     will create a new event whose message reports that it has combined events with
//     the same reason.
//   * Events are incrementally counted if the exact same event is encountered multiple
//     times.
//对事件做缓存
func NewEventCorrelator(clock clock.Clock) *EventCorrelator {
	cacheSize := maxLruCacheEntries
	return &EventCorrelator{
		//filterFunc过滤事件
		filterFunc: DefaultEventFilterFunc,
		// 把相似的事件汇聚在一起
		// 如果在最近 10 分钟出现过 10 个相似的事件
		//（除了 message 和时间戳之外其他关键字段都相同的事件），
		// aggregator 会把它们的 message 设置为
		// events with common reason combined，这样它们就完全一样了
		aggregator: NewEventAggregator(
			cacheSize,
			// 通过相同的Event域来进行分组
			EventAggregatorByReasonFunc,
			// 生成"根据同样的原因进行分组"消息
			EventAggregatorByReasonMessageFunc,
			// 每个时间间隔里最多统计10个Events
			defaultAggregateMaxEvents,
			// 最大时间间隔为10mins
			defaultAggregateIntervalInSeconds,
			clock),
		//把相同的事件记录到一起
		//这个变量的名字有点奇怪，其实它会把相同的事件
		//（除了时间戳之外其他字段都相同）变成同一个事件，
		//通过增加事件的 Count 字段来记录该事件发生了多少次。
		//经过 aggregator 的事件会在这里变成同一个事件
		//在一定事件内多次尝试的事件就会被压缩
		logger: newEventLogger(cacheSize, clock),
		//aggregator 和 logger 都会在内部维护一个缓存（默认长度是 4096），
		//事件的相似性和相同性比较是和缓存中的事件进行的，
		//也就是说它并在乎 kubelet 启动之前的事件，
		//而且如果事件超过 4096 的长度，最近没有被访问的事件也会被从缓存中移除。
		//这也是这个文件中带有 cache 的原因。
		//它们的内部实现并不复杂，有兴趣的读者请自行阅读相关源码。
	}
}

// EventCorrelate filters, aggregates, counts, and de-duplicates all incoming events
func (c *EventCorrelator) EventCorrelate(newEvent *api.Event) (*EventCorrelateResult, error) {
	if c.filterFunc(newEvent) {
		return &EventCorrelateResult{Skip: true}, nil
	}
	aggregateEvent, err := c.aggregator.EventAggregate(newEvent)
	if err != nil {
		return &EventCorrelateResult{}, err
	}
	observedEvent, patch, err := c.logger.eventObserve(aggregateEvent)
	return &EventCorrelateResult{Event: observedEvent, Patch: patch}, err
}

// UpdateState based on the latest observed state from server
func (c *EventCorrelator) UpdateState(event *api.Event) {
	c.logger.updateState(event)
}
