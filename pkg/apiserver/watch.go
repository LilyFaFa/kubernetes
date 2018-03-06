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

package apiserver

import (
	"bytes"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/httplog"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/runtime/serializer/streaming"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/wsstream"
	"k8s.io/kubernetes/pkg/watch"
	"k8s.io/kubernetes/pkg/watch/versioned"

	"github.com/emicklei/go-restful"
	"golang.org/x/net/websocket"
)

// nothing will ever be sent down this channel
var neverExitWatch <-chan time.Time = make(chan time.Time)

// timeoutFactory abstracts watch timeout logic for testing
type timeoutFactory interface {
	TimeoutCh() (<-chan time.Time, func() bool)
}

// realTimeoutFactory implements timeoutFactory
type realTimeoutFactory struct {
	timeout time.Duration
}

// TimeoutChan returns a channel which will receive something when the watch times out,
// and a cleanup function to call when this happens.
func (w *realTimeoutFactory) TimeoutCh() (<-chan time.Time, func() bool) {
	if w.timeout == 0 {
		return neverExitWatch, func() bool { return false }
	}
	t := time.NewTimer(w.timeout)
	return t.C, t.Stop
}

// serveWatch handles serving requests to the server
// TODO: the functionality in this method and in WatchServer.Serve is not cleanly decoupled.
func serveWatch(watcher watch.Interface, scope RequestScope, req *restful.Request, res *restful.Response, timeout time.Duration) {
	// negotiate for the stream serializer
	serializer, err := negotiateOutputStreamSerializer(req.Request, scope.Serializer)
	if err != nil {
		scope.err(err, res.ResponseWriter, req.Request)
		return
	}
	framer := serializer.StreamSerializer.Framer
	streamSerializer := serializer.StreamSerializer.Serializer
	embedded := serializer.Serializer
	if framer == nil {
		scope.err(fmt.Errorf("no framer defined for %q available for embedded encoding", serializer.MediaType), res.ResponseWriter, req.Request)
		return
	}
	encoder := scope.Serializer.EncoderForVersion(streamSerializer, scope.Kind.GroupVersion())

	useTextFraming := serializer.EncodesAsText

	// find the embedded serializer matching the media type
	embeddedEncoder := scope.Serializer.EncoderForVersion(embedded, scope.Kind.GroupVersion())

	// TODO: next step, get back mediaTypeOptions from negotiate and return the exact value here
	mediaType := serializer.MediaType
	if mediaType != runtime.ContentTypeJSON {
		mediaType += ";stream=watch"
	}

	server := &WatchServer{
		watching: watcher,
		scope:    scope,

		useTextFraming:  useTextFraming,
		mediaType:       mediaType,
		framer:          framer,
		encoder:         encoder,
		embeddedEncoder: embeddedEncoder,
		fixup: func(obj runtime.Object) {
			if err := setSelfLink(obj, req, scope.Namer); err != nil {
				utilruntime.HandleError(fmt.Errorf("failed to set link for object %v: %v", reflect.TypeOf(obj), err))
			}
		},

		t: &realTimeoutFactory{timeout},
	}
	//watch这个api涉及到的http
	server.ServeHTTP(res.ResponseWriter, req.Request)
}

// WatchServer serves a watch.Interface over a websocket or vanilla HTTP.
type WatchServer struct {
	watching watch.Interface
	scope    RequestScope

	// true if websocket messages should use text framing (as opposed to binary framing)
	useTextFraming bool
	// the media type this watch is being served with
	mediaType string
	// used to frame the watch stream
	framer runtime.Framer
	// used to encode the watch stream event itself
	encoder runtime.Encoder
	// used to encode the nested object in the watch stream
	embeddedEncoder runtime.Encoder
	fixup           func(runtime.Object)

	t timeoutFactory
}

// ServeHTTP serves a series of encoded events via HTTP with Transfer-Encoding: chunked
// or over a websocket connection.
func (s *WatchServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w = httplog.Unlogged(w)
	// 首先会查看发送来的请求是不是要求使用websocket，即wsstream.IsWebSocketRequest(req)，
	// 假如是的话就通过websocket向client发送watch event，
	// 也就是说kube-apiserver是支持通过websocket向客户端发送watch event的。
	// 如果不是就使用后面的代码
	if wsstream.IsWebSocketRequest(req) {
		w.Header().Set("Content-Type", s.mediaType)
		websocket.Handler(s.HandleWS).ServeHTTP(w, req)
		return
	}

	cn, ok := w.(http.CloseNotifier)
	if !ok {
		err := fmt.Errorf("unable to start watch - can't get http.CloseNotifier: %#v", w)
		utilruntime.HandleError(err)
		s.scope.err(errors.NewInternalError(err), w, req)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		err := fmt.Errorf("unable to start watch - can't get http.Flusher: %#v", w)
		utilruntime.HandleError(err)
		s.scope.err(errors.NewInternalError(err), w, req)
		return
	}

	framer := s.framer.NewFrameWriter(w)
	if framer == nil {
		// programmer error
		err := fmt.Errorf("no stream framing support is available for media type %q", s.mediaType)
		utilruntime.HandleError(err)
		s.scope.err(errors.NewBadRequest(err.Error()), w, req)
		return
	}
	e := streaming.NewEncoder(framer, s.encoder)

	// ensure the connection times out
	timeoutCh, cleanup := s.t.TimeoutCh()
	defer cleanup()
	defer s.watching.Stop()

	// begin the stream
	// 假如不是webhook的话，则首先设置http返回头，Content-Type设置为s.MediaType，一般为json，
	// 同时设置Transfer-Encoding为chunked，设置返回码为200(StatusOK)，
	// 和我们从API分析那一节获取的信息一样，首先会返回一个200的状态吗。
	w.Header().Set("Content-Type", s.mediaType)
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var unknown runtime.Unknown
	internalEvent := &versioned.InternalEvent{}
	buf := &bytes.Buffer{}
	ch := s.watching.ResultChan()
	for {
		select {
		case <-cn.CloseNotify():
			return
		case <-timeoutCh:
			return
		//从channel中获取event数据，这个channel中的数据怎么来的需要看一下
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}
			// 序列化事件
			obj := event.Object
			s.fixup(obj)
			if err := s.embeddedEncoder.Encode(obj, buf); err != nil {
				// unexpected error
				utilruntime.HandleError(fmt.Errorf("unable to encode watch object: %v", err))
				return
			}

			// ContentType is not required here because we are defaulting to the serializer
			// type
			unknown.Raw = buf.Bytes()
			event.Object = &unknown

			// the internal event will be versioned by the encoder
			// 发送数据
			*internalEvent = versioned.InternalEvent(event)
			if err := e.Encode(internalEvent); err != nil {
				utilruntime.HandleError(fmt.Errorf("unable to encode watch object: %v (%#v)", err, e))
				// client disconnect.
				return
			}
			if len(ch) == 0 {
				flusher.Flush()
			}

			buf.Reset()
		}
	}
}

// HandleWS implements a websocket handler.
// websocket处理
func (s *WatchServer) HandleWS(ws *websocket.Conn) {
	defer ws.Close()
	done := make(chan struct{})

	go func() {
		defer utilruntime.HandleCrash()
		// This blocks until the connection is closed.
		// Client should not send anything.
		wsstream.IgnoreReceives(ws, 0)
		// Once the client closes, we should also close
		close(done)
	}()

	var unknown runtime.Unknown
	internalEvent := &versioned.InternalEvent{}
	buf := &bytes.Buffer{}
	streamBuf := &bytes.Buffer{}
	ch := s.watching.ResultChan()
	for {
		select {
		case <-done:
			s.watching.Stop()
			return
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}
			obj := event.Object
			s.fixup(obj)
			if err := s.embeddedEncoder.Encode(obj, buf); err != nil {
				// unexpected error
				utilruntime.HandleError(fmt.Errorf("unable to encode watch object: %v", err))
				return
			}

			// ContentType is not required here because we are defaulting to the serializer
			// type
			unknown.Raw = buf.Bytes()
			event.Object = &unknown

			// the internal event will be versioned by the encoder
			*internalEvent = versioned.InternalEvent(event)
			if err := s.encoder.Encode(internalEvent, streamBuf); err != nil {
				// encoding error
				utilruntime.HandleError(fmt.Errorf("unable to encode event: %v", err))
				s.watching.Stop()
				return
			}
			if s.useTextFraming {
				if err := websocket.Message.Send(ws, streamBuf.String()); err != nil {
					// Client disconnect.
					s.watching.Stop()
					return
				}
			} else {
				if err := websocket.Message.Send(ws, streamBuf.Bytes()); err != nil {
					// Client disconnect.
					s.watching.Stop()
					return
				}
			}
			buf.Reset()
			streamBuf.Reset()
		}
	}
}
