package codexapp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"codex-acp-bridge/internal/protocol"
)

type fakePeer struct {
	conn    net.Conn
	frames  chan protocol.Message
	writeMu sync.Mutex
}

func newFakePeer(b net.Conn) *fakePeer {
	peer := &fakePeer{conn: b, frames: make(chan protocol.Message, 64)}
	scanner := bufio.NewScanner(b)
	go func() {
		defer close(peer.frames)
		for scanner.Scan() {
			message, err := protocol.ParseFrame(scanner.Bytes(), false)
			if err == nil {
				peer.frames <- message
			}
		}
	}()
	return peer
}

func (p *fakePeer) send(frame protocol.Message) error {
	body, err := protocol.LiteFrame(frame)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.conn.Write(body)
	return err
}

func (p *fakePeer) expect(t *testing.T, timeout time.Duration) protocol.Message {
	t.Helper()
	select {
	case message := <-p.frames:
		return message
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a frame from the bridge")
		return nil
	}
}

type connRig struct {
	conn     *Conn
	peer     *fakePeer
	notes    chan string
	requests chan protocol.Message
	closers  []io.Closer
}

func newConnRig(t *testing.T) *connRig {
	t.Helper()
	a, b := net.Pipe()
	rig := &connRig{
		peer:     newFakePeer(b),
		notes:    make(chan string, 64),
		requests: make(chan protocol.Message, 8),
		closers:  []io.Closer{a, b},
	}
	var conn *Conn
	conn = NewConn(a, a, ConnOptions{
		Closers: []io.Closer{a},
		OnNotification: func(method string, params protocol.Message) {
			rig.notes <- method
		},
		OnRequest: func(id any, method string, params protocol.Message) {
			rig.requests <- params
			_ = conn.Respond(id, protocol.Message{"decision": "accept"}, nil)
		},
	})
	rig.conn = conn
	t.Cleanup(func() {
		conn.Close()
		for _, closer := range rig.closers {
			_ = closer.Close()
		}
	})
	return rig
}

func TestCallUsesLiteFramingAndCorrelatesReplies(t *testing.T) {
	rig := newConnRig(t)
	go func() {
		request := rig.peer.expect(t, 2*time.Second)
		if _, hasVersion := request["jsonrpc"]; hasVersion {
			t.Errorf("app-server frame must not carry jsonrpc: %v", request)
		}
		if request["method"] != "initialize" {
			t.Errorf("method = %v", request["method"])
		}
		_ = rig.peer.send(protocol.Message{
			"id": request["id"], "result": protocol.Message{"userAgent": "fake/1"},
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := rig.conn.Call(ctx, "initialize", protocol.Message{
		"clientInfo": protocol.Message{"name": "test", "version": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Str(result, "userAgent") != "fake/1" {
		t.Errorf("result = %v", result)
	}
}

func TestNotifyOmitsNilParams(t *testing.T) {
	rig := newConnRig(t)
	if err := rig.conn.Notify("initialized", nil); err != nil {
		t.Fatal(err)
	}
	frame := rig.peer.expect(t, 2*time.Second)
	if _, present := frame["params"]; present {
		t.Errorf("initialized notification must omit params: %v", frame)
	}
	if frame["method"] != "initialized" {
		t.Errorf("method = %v", frame["method"])
	}
}

func TestCallSurfacesRPCError(t *testing.T) {
	rig := newConnRig(t)
	go func() {
		request := rig.peer.expect(t, 2*time.Second)
		_ = rig.peer.send(protocol.RPCErrorResponse(request["id"], -32601, "nope", nil))
	}()
	_, err := rig.conn.Call(context.Background(), "thread/start", nil)
	var rpcErr *protocol.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("expected a JSON-RPC error, got %v", err)
	}
}

func TestNotificationsAreDeliveredInOrder(t *testing.T) {
	rig := newConnRig(t)
	for _, method := range []string{"turn/started", "item/started", "item/completed", "turn/completed"} {
		if err := rig.peer.send(protocol.Message{"method": method, "params": protocol.Message{}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"turn/started", "item/started", "item/completed", "turn/completed"} {
		select {
		case got := <-rig.notes:
			if got != want {
				t.Fatalf("notification order: got %q want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing notification %q", want)
		}
	}
}

func TestServerRequestGetsIndependentResponse(t *testing.T) {
	rig := newConnRig(t)
	if err := rig.peer.send(protocol.Message{
		"id":     42,
		"method": "item/commandExecution/requestApproval",
		"params": protocol.Message{"itemId": "cmd-1", "command": "ls"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case params := <-rig.requests:
		if protocol.Str(params, "command") != "ls" {
			t.Errorf("request params = %v", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server request handler was not invoked")
	}
	answer := rig.peer.expect(t, 2*time.Second)
	if protocol.IDKey(answer["id"]) != protocol.IDKey(42) {
		t.Errorf("response id = %v", answer["id"])
	}
	if protocol.Str(protocol.AsMessage(answer["result"]), "decision") != "accept" {
		t.Errorf("response = %v", answer)
	}
}

func TestCloseUnblocksPendingCall(t *testing.T) {
	rig := newConnRig(t)
	go func() {
		time.Sleep(20 * time.Millisecond)
		rig.conn.Close()
	}()
	_, err := rig.conn.Call(context.Background(), "thread/start", nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestStartupErrorIncludesStderr(t *testing.T) {
	tail := newStderrTail(2)
	for _, line := range []string{"first", "second", "third"} {
		tail.Add(line)
	}
	err := StartupError(errors.New("handshake failed"), tail.String())
	if err == nil || err.Error() == "handshake failed" {
		t.Fatalf("stderr should be appended: %v", err)
	}
	if got := tail.String(); got != "second\nthird" {
		t.Errorf("stderr ring = %q", got)
	}
	if err := StartupError(nil, "anything"); err != nil {
		t.Errorf("nil error must stay nil: %v", err)
	}
}
