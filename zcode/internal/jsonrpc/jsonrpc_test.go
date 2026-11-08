package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// testConn 把连接的读写两端都拿到手: 写端模拟 ACP client 的 stdin, buf 收桥的 stdout。
type testConn struct {
	*Conn
	w   *io.PipeWriter
	mu  sync.Mutex
	buf bytes.Buffer
}

func newTestConn(t *testing.T) *testConn {
	t.Helper()
	pr, pw := io.Pipe()
	tc := &testConn{w: pw}
	tc.Conn = New(bufWriter{&tc.mu, &tc.buf}, nil, false)
	go func() { _ = tc.Conn.Run(context.Background(), pr) }()
	t.Cleanup(func() { _ = pw.Close() })
	return tc
}

// bufWriter 是带锁的 bytes.Writer (读循环与测试断言并发访问)。
type bufWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (b bufWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (tc *testConn) send(t *testing.T, v any) {
	t.Helper()
	bts, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.w.Write(append(bts, '\n')); err != nil {
		t.Fatal(err)
	}
}

// readFrame 读下一帧 (带超时保护)。
func (tc *testConn) readFrame(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tc.mu.Lock()
		line, err := tc.buf.ReadString('\n')
		tc.mu.Unlock()
		if err == nil && line != "\n" {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) != nil {
				t.Fatalf("坏帧: %q", line)
			}
			return m
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待输出帧超时")
	return nil
}

func TestRequestResponse(t *testing.T) {
	tc := newTestConn(t)
	tc.Handle("ping", func(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *Error) {
		return map[string]any{"pong": true}, nil
	})

	// 数字 id: 必须原样回 (zed 等客户端校验类型)。
	tc.send(t, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "ping", "params": map[string]any{}})
	resp := tc.readFrame(t)
	if raw, _ := json.Marshal(resp["id"]); string(raw) != "7" {
		t.Errorf("id 未原样回: %v", resp["id"])
	}
	if m := resp["result"].(map[string]any); m["pong"] != true {
		t.Errorf("result 异常: %v", resp)
	}

	// 字符串 id。
	tc.send(t, map[string]any{"jsonrpc": "2.0", "id": "abc", "method": "ping"})
	resp = tc.readFrame(t)
	if resp["id"] != "abc" {
		t.Errorf("字符串 id 未原样回: %v", resp["id"])
	}
}

func TestHandlerError(t *testing.T) {
	tc := newTestConn(t)
	tc.Handle("boom", func(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *Error) {
		return nil, Errf(-32000, "kaboom")
	})
	tc.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "boom"})
	resp := tc.readFrame(t)
	e := resp["error"].(map[string]any)
	if e["code"] != float64(-32000) || e["message"] != "kaboom" {
		t.Errorf("错误帧异常: %v", resp)
	}
}

func TestUnknownMethod(t *testing.T) {
	tc := newTestConn(t)
	tc.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "nope"})
	resp := tc.readFrame(t)
	e := resp["error"].(map[string]any)
	if e["code"] != float64(-32601) {
		t.Errorf("未注册方法应 -32601: %v", resp)
	}
}

func TestNotificationNoResponse(t *testing.T) {
	tc := newTestConn(t)
	called := make(chan struct{}, 1)
	tc.HandleNotification("session/cancel", func(ctx context.Context, params json.RawMessage) {
		called <- struct{}{}
	})
	tc.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": "s"}})
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("notification handler 未被调用")
	}
	// 无 id 的通知不产生任何输出帧。
	time.Sleep(100 * time.Millisecond)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.buf.Len() != 0 {
		t.Errorf("notification 不应有响应输出: %q", tc.buf.String())
	}
}

// TestBadLineSkipped 验证单行坏帧不杀死连接。
func TestBadLineSkipped(t *testing.T) {
	tc := newTestConn(t)
	tc.Handle("ping", func(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *Error) {
		return "ok", nil
	})
	if _, err := tc.w.Write([]byte("this is not json\n")); err != nil {
		t.Fatal(err)
	}
	tc.send(t, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "ping"})
	if resp := tc.readFrame(t); resp["result"] != "ok" {
		t.Errorf("坏帧后连接应存活: %v", resp)
	}
}

// TestAgentToClientRequest 验证 agent→client 请求与响应路由。
func TestAgentToClientRequest(t *testing.T) {
	tc := newTestConn(t)
	type answer struct {
		Update map[string]any `json:"update"`
	}
	tc.Handle("ask", func(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *Error) {
		var ans answer
		if rerr := tc.Conn.Request(ctx, "session/request_permission", map[string]any{
			"sessionId": "s", "options": []any{},
		}, &ans); rerr != nil {
			return nil, rerr
		}
		return ans.Update, nil
	})
	tc.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "ask"})

	// 收到 agent→client 请求, 回响应。
	req := tc.readFrame(t)
	reqID, _ := req["id"].(string)
	if req["method"] != "session/request_permission" || reqID == "" {
		t.Fatalf("agent→client 请求帧异常: %v", req)
	}
	tc.send(t, map[string]any{"jsonrpc": "2.0", "id": reqID,
		"result": map[string]any{"update": map[string]any{"outcome": "selected"}}})

	resp := tc.readFrame(t)
	result := resp["result"].(map[string]any)
	if result["outcome"] != "selected" {
		t.Errorf("agent→client 响应未路由回 handler: %v", resp)
	}
}

// TestNotifyFrame 验证通知帧的形状。
func TestNotifyFrame(t *testing.T) {
	tc := newTestConn(t)
	if err := tc.Conn.Notify("session/update", map[string]any{"sessionId": "s"}); err != nil {
		t.Fatal(err)
	}
	frame := tc.readFrame(t)
	if frame["method"] != "session/update" || frame["id"] != nil {
		t.Errorf("通知帧不应带 id: %v", frame)
	}
	if frame["params"].(map[string]any)["sessionId"] != "s" {
		t.Errorf("params 异常: %v", frame)
	}
}
