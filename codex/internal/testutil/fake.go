// Package testutil provides a deterministic, in-memory stand-in for the
// Codex app-server client and a small ACP client harness. No real Codex
// process, model call or network is involved in the tests.
package testutil

import (
	"context"
	"fmt"
	"sync"
	"time"

	"codex-acp-bridge/internal/protocol"
)

// RecordedRequest is one bridge -> app-server call.
type RecordedRequest struct {
	Method string
	Params protocol.Message
}

// FakeEvent is a notification emitted by the fake after turn/start.
type FakeEvent struct {
	Method string
	Params protocol.Message
	Delay  time.Duration
}

// FakeCodex implements bridge.Codex structurally (no import cycle).
type FakeCodex struct {
	mu       sync.Mutex
	note     func(method string, params protocol.Message)
	request  func(id any, method string, params protocol.Message)
	requests []RecordedRequest
	answers  []protocol.Message
	history  protocol.Message
	script   []FakeEvent
	threadN  int
	turnN    int
	done     chan struct{}
	closeOne sync.Once
	// FailTurnStart makes turn/start fail, for error-path tests.
	FailTurnStart error
	// FailThreadStart makes thread/start fail.
	FailThreadStart error
}

func NewFakeCodex() *FakeCodex {
	return &FakeCodex{
		done:    make(chan struct{}),
		history: protocol.Message{"id": "thr-history"},
	}
}

func (f *FakeCodex) InitializeResult() protocol.Message {
	return protocol.Message{
		"userAgent":      "fake-codex/0",
		"codexHome":      "/tmp/fake-codex-home",
		"platformFamily": "unix",
		"platformOs":     "test",
	}
}

func (f *FakeCodex) SetHandlers(
	onNotification func(method string, params protocol.Message),
	onRequest func(id any, method string, params protocol.Message),
) {
	f.mu.Lock()
	f.note, f.request = onNotification, onRequest
	f.mu.Unlock()
}

func (f *FakeCodex) record(method string, params protocol.Message) {
	f.mu.Lock()
	f.requests = append(f.requests, RecordedRequest{Method: method, Params: params})
	f.mu.Unlock()
}

// Requests returns a snapshot of the recorded calls.
func (f *FakeCodex) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecordedRequest(nil), f.requests...)
}

// FindRequest returns the params of the first call with the given method.
func (f *FakeCodex) FindRequest(method string) (protocol.Message, bool) {
	for _, request := range f.Requests() {
		if request.Method == method {
			return request.Params, true
		}
	}
	return nil, false
}

func (f *FakeCodex) ThreadStart(ctx context.Context, params protocol.Message) (protocol.Message, error) {
	f.record("thread/start", params)
	if f.FailThreadStart != nil {
		return nil, f.FailThreadStart
	}
	f.mu.Lock()
	f.threadN++
	id := fmt.Sprintf("thr-%d", f.threadN)
	f.mu.Unlock()
	return protocol.Message{"id": id, "cwd": protocol.Str(params, "cwd")}, nil
}

func (f *FakeCodex) ThreadResume(ctx context.Context, threadID string, params protocol.Message) (protocol.Message, error) {
	resume := make(protocol.Message, len(params)+1)
	for key, value := range params {
		resume[key] = value
	}
	resume["threadId"] = threadID
	f.record("thread/resume", resume)
	f.mu.Lock()
	thread := protocol.Message{}
	for key, value := range f.history {
		thread[key] = value
	}
	f.mu.Unlock()
	thread["id"] = threadID
	return thread, nil
}

func (f *FakeCodex) TurnStart(ctx context.Context, params protocol.Message) (protocol.Message, error) {
	f.record("turn/start", params)
	if f.FailTurnStart != nil {
		return nil, f.FailTurnStart
	}
	f.mu.Lock()
	f.turnN++
	id := fmt.Sprintf("turn-%d", f.turnN)
	script := append([]FakeEvent(nil), f.script...)
	f.mu.Unlock()

	threadID := protocol.Str(params, "threadId")
	go func() {
		for _, event := range script {
			if event.Delay > 0 {
				time.Sleep(event.Delay)
			}
			if event.Method == "__server_request__" {
				f.Ask(event.Params["id"], protocol.Str(event.Params, "method"),
					protocol.Sub(event.Params, "params"))
				continue
			}
			eventParams := protocol.Message{}
			for key, value := range event.Params {
				eventParams[key] = value
			}
			if eventParams["threadId"] == nil {
				eventParams["threadId"] = threadID
			}
			if eventParams["turnId"] == nil && event.Method != "thread/started" {
				eventParams["turnId"] = id
			}
			f.Emit(event.Method, eventParams)
		}
	}()
	return protocol.Message{"id": id, "status": "inProgress"}, nil
}

func (f *FakeCodex) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	f.record("turn/interrupt", protocol.Message{"threadId": threadID, "turnId": turnID})
	f.Emit("turn/completed", protocol.Message{
		"threadId": threadID,
		"turnId":   turnID,
		"turn":     protocol.Message{"id": turnID, "status": "interrupted"},
	})
	return nil
}

func (f *FakeCodex) Respond(id any, result protocol.Message, rpcErr *protocol.RPCError) error {
	f.mu.Lock()
	f.answers = append(f.answers, result)
	f.mu.Unlock()
	return nil
}

// Answers returns the decisions sent back to app-server.
func (f *FakeCodex) Answers() []protocol.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Message(nil), f.answers...)
}

func (f *FakeCodex) Done() <-chan struct{} { return f.done }

func (f *FakeCodex) Close() error {
	f.closeOne.Do(func() { close(f.done) })
	return nil
}

// ---- test control ----

func (f *FakeCodex) SetScript(events ...FakeEvent) {
	f.mu.Lock()
	f.script = append([]FakeEvent(nil), events...)
	f.mu.Unlock()
}

func (f *FakeCodex) SetHistory(thread protocol.Message) {
	f.mu.Lock()
	f.history = thread
	f.mu.Unlock()
}

func (f *FakeCodex) Emit(method string, params protocol.Message) {
	f.mu.Lock()
	handler := f.note
	f.mu.Unlock()
	if handler != nil {
		handler(method, params)
	}
}

// Ask injects a server-initiated request into the bridge (approval flow).
func (f *FakeCodex) Ask(id any, method string, params protocol.Message) {
	f.mu.Lock()
	handler := f.request
	f.mu.Unlock()
	if handler != nil {
		handler(id, method, params)
	}
}
