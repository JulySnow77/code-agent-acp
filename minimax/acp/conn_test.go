package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newConnPair(t *testing.T, optsA, optsB ConnOptions) (*Conn, *Conn, Transport, Transport) {
	t.Helper()
	ta, tb := NewBufferedPair()
	a, b := NewConn(ta, optsA), NewConn(tb, optsB)
	go func() { _ = a.Run(context.Background()) }()
	go func() { _ = b.Run(context.Background()) }()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b, ta, tb
}

func TestConnBidirectionalCalls(t *testing.T) {
	a, b, _, _ := newConnPair(t, ConnOptions{}, ConnOptions{})
	a.OnRequest("client/side", func(_ context.Context, _ json.RawMessage) (any, *RPCError) {
		return map[string]any{"side": "client"}, nil
	})
	b.OnRequest("agent/side", func(_ context.Context, _ json.RawMessage) (any, *RPCError) {
		return map[string]any{"side": "agent"}, nil
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		var out map[string]any
		if err := a.Call(context.Background(), "agent/side", nil, &out); err != nil || out["side"] != "agent" {
			errs <- fmt.Errorf("a->b: out=%v err=%v", out, err)
		}
	}()
	go func() {
		defer wg.Done()
		var out map[string]any
		if err := b.Call(context.Background(), "client/side", nil, &out); err != nil || out["side"] != "client" {
			errs <- fmt.Errorf("b->a: out=%v err=%v", out, err)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A handler that issues a request back to the caller while the original
// request is still pending is the classic JSON-RPC deadlock. ACP full duplex
// requires it to work.
func TestConnReverseCallDuringPendingRequest(t *testing.T) {
	a, b, _, _ := newConnPair(t, ConnOptions{}, ConnOptions{})
	a.OnRequest("session/request_permission", func(_ context.Context, raw json.RawMessage) (any, *RPCError) {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	})
	b.OnRequest("session/prompt", func(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
		var out map[string]any
		if err := b.Call(ctx, "session/request_permission", map[string]any{"sessionId": "s"}, &out); err != nil {
			return nil, Errorf(CodeInternalError, "reverse call: %v", err)
		}
		return map[string]any{"stopReason": StopEndTurn, "permission": out}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var result struct {
		StopReason string         `json:"stopReason"`
		Permission map[string]any `json:"permission"`
	}
	if err := a.Call(ctx, "session/prompt", map[string]any{"sessionId": "s"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopEndTurn || result.Permission["outcome"] == nil {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestConnMethodNotFound(t *testing.T) {
	a, _, _, _ := newConnPair(t, ConnOptions{}, ConnOptions{})
	err := a.Call(context.Background(), "does/not/exist", nil, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("expected methodNotFound, got %v", err)
	}
	if data, ok := rpcErr.Data.(map[string]any); !ok || data["method"] != "does/not/exist" {
		t.Fatalf("missing method in error data: %#v", rpcErr.Data)
	}
}

func TestConnNotificationOrdering(t *testing.T) {
	a, b, _, _ := newConnPair(t, ConnOptions{}, ConnOptions{})
	const n = 40
	got := make(chan int, n)
	a.OnNotification("tick", func(_ context.Context, raw json.RawMessage) {
		var p struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(raw, &p)
		got <- p.N
	})
	for i := 0; i < n; i++ {
		if err := b.Notify(context.Background(), "tick", map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		select {
		case v := <-got:
			if v != i {
				t.Fatalf("notification %d arrived out of order: got %d", i, v)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for notification %d", i)
		}
	}
}

func TestConnParseErrorResponseAndRecovery(t *testing.T) {
	inbound := make(chan []byte, 8)
	a, b, ta, _ := newConnPair(t, ConnOptions{Observer: func(dir FrameDirection, frame []byte) {
		if dir == FrameInbound {
			inbound <- append([]byte(nil), frame...)
		}
	}}, ConnOptions{})
	if err := ta.Write([]byte("this is not json")); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-inbound:
		var m map[string]any
		if err := json.Unmarshal(frame, &m); err != nil {
			t.Fatal(err)
		}
		if m["id"] != nil {
			t.Fatalf("parse error must use id=null: %s", frame)
		}
		errObj, ok := m["error"].(map[string]any)
		if !ok || errObj["code"] != float64(CodeParseError) {
			t.Fatalf("unexpected error frame: %s", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no parse error response")
	}
	// The connection must still work after a malformed frame.
	b.OnRequest("still/alive", func(_ context.Context, _ json.RawMessage) (any, *RPCError) {
		return map[string]any{"ok": true}, nil
	})
	var out map[string]any
	if err := a.Call(context.Background(), "still/alive", nil, &out); err != nil || out["ok"] != true {
		t.Fatalf("connection did not recover: out=%v err=%v", out, err)
	}
}

func TestConnContextCancellationSendsCancelRequest(t *testing.T) {
	cancelSeen := make(chan string, 1)
	a, b, _, _ := newConnPair(t, ConnOptions{SendCancelRequest: true}, ConnOptions{})
	b.OnNotification(MethodCancelRequest, func(_ context.Context, raw json.RawMessage) {
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(raw, &p)
		cancelSeen <- string(p.RequestID)
	})
	b.OnRequest("slow", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		<-ctx.Done()
		return "late", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, err := a.CallRaw(ctx, "slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	select {
	case id := <-cancelSeen:
		if id == "" {
			t.Fatal("empty requestId in $/cancel_request")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not receive $/cancel_request")
	}
}

func TestConnInboundCancelRequest(t *testing.T) {
	a, b, _, _ := newConnPair(t, ConnOptions{}, ConnOptions{})
	started := make(chan struct{})
	b.OnRequest("slow", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		close(started)
		<-ctx.Done()
		return nil, nil
	})
	id := a.AllocID()
	type callErr struct{ err error }
	ch := make(chan callErr, 1)
	go func() {
		_, err := a.CallRawWithID(context.Background(), id, "slow", nil)
		ch <- callErr{err: err}
	}()
	<-started
	if err := a.Notify(context.Background(), MethodCancelRequest, map[string]any{"requestId": id}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		var rpcErr *RPCError
		if !errors.As(r.err, &rpcErr) || rpcErr.Code != CodeRequestCancelled {
			t.Fatalf("expected -32800, got %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled request never returned")
	}
}

func TestConnCloseUnblocksPending(t *testing.T) {
	a, b, _, _ := newConnPair(t, ConnOptions{}, ConnOptions{})
	b.OnRequest("block", func(ctx context.Context, _ json.RawMessage) (any, *RPCError) {
		<-ctx.Done()
		return nil, nil
	})
	ch := make(chan error, 1)
	go func() {
		_, err := a.CallRaw(context.Background(), "block", nil)
		ch <- err
	}()
	time.Sleep(50 * time.Millisecond)
	a.Close()
	select {
	case err := <-ch:
		if !errors.Is(err, ErrConnClosed) {
			t.Fatalf("expected ErrConnClosed, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call did not unblock on Close")
	}
}

func TestConnConcurrentCalls(t *testing.T) {
	a, b, _, _ := newConnPair(t, ConnOptions{}, ConnOptions{})
	b.OnRequest("square", func(_ context.Context, raw json.RawMessage) (any, *RPCError) {
		var p struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(raw, &p)
		return map[string]any{"v": p.N * p.N}, nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			var out struct {
				V int `json:"v"`
			}
			if err := a.Call(context.Background(), "square", map[string]any{"n": n}, &out); err != nil || out.V != n*n {
				errs <- fmt.Errorf("n=%d out=%d err=%v", n, out.V, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
