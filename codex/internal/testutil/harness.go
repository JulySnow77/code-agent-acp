package testutil

import (
	"bufio"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"codex-acp-bridge/internal/protocol"
)

// Harness is a minimal ACP client for tests: it writes requests, reads frames,
// collects session/update notifications and answers permission requests.
type Harness struct {
	in  io.Writer
	out io.Reader

	writeMu sync.Mutex
	seq     atomic.Int64

	mu           sync.Mutex
	pending      map[string]chan protocol.Message
	updates      []protocol.Message
	updateCh     chan struct{}
	permRequests []protocol.Message

	// PermissionChoice: allow_once | allow_always | reject_once |
	// reject_always | cancelled | error | manual.
	PermissionChoice string
}

func NewHarness(in io.Writer, out io.Reader, permissionChoice string) *Harness {
	if permissionChoice == "" {
		permissionChoice = protocol.PermissionAllowOnce
	}
	return &Harness{
		in:               in,
		out:              out,
		pending:          make(map[string]chan protocol.Message),
		updateCh:         make(chan struct{}, 4096),
		PermissionChoice: permissionChoice,
	}
}

// Start begins the frame reader. It must be called before any Call.
func (h *Harness) Start() {
	scanner := bufio.NewScanner(h.out)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	go func() {
		for scanner.Scan() {
			message, err := protocol.ParseFrame(scanner.Bytes(), true)
			if err != nil {
				continue
			}
			method, hasMethod := message["method"]
			id, hasID := message["id"]
			switch {
			case hasID && !hasMethod:
				h.deliver(id, message)
			case hasMethod && hasID && protocol.StringValue(method) == protocol.MethodSessionRequestPermission:
				h.handlePermission(id, message)
			case hasMethod && !hasID && protocol.StringValue(method) == protocol.MethodSessionUpdate:
				h.mu.Lock()
				if params := protocol.Sub(message, "params"); params != nil {
					h.updates = append(h.updates, params)
				}
				h.mu.Unlock()
				select {
				case h.updateCh <- struct{}{}:
				default:
				}
			}
		}
	}()
}

func (h *Harness) write(frame protocol.Message) error {
	body, err := protocol.ACPFrame(frame)
	if err != nil {
		return err
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err = h.in.Write(body)
	return err
}

// Call issues a request and waits for the matching response.
func (h *Harness) Call(method string, params protocol.Message) (protocol.Message, *protocol.RPCError) {
	return h.CallWithTimeout(method, params, 10*time.Second)
}

func (h *Harness) CallWithTimeout(method string, params protocol.Message, timeout time.Duration) (protocol.Message, *protocol.RPCError) {
	id := fmt.Sprintf("test-%d", h.seq.Add(1))
	key := protocol.IDKey(id)
	reply := make(chan protocol.Message, 1)
	h.mu.Lock()
	h.pending[key] = reply
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, key)
		h.mu.Unlock()
	}()
	if err := h.write(protocol.Message{"id": id, "method": method, "params": params}); err != nil {
		return nil, &protocol.RPCError{Code: -32603, Message: err.Error()}
	}
	select {
	case message := <-reply:
		if raw := message["error"]; raw != nil {
			object, _ := raw.(map[string]any)
			return nil, &protocol.RPCError{
				Code:    int(protocol.Num(protocol.Message(object), "code")),
				Message: protocol.Str(protocol.Message(object), "message"),
			}
		}
		result := protocol.Sub(message, "result")
		if result == nil {
			result = protocol.Message{}
		}
		return result, nil
	case <-time.After(timeout):
		return nil, &protocol.RPCError{Code: -32000, Message: "timed out waiting for " + method}
	}
}

// Notify sends a client notification (e.g. session/cancel).
func (h *Harness) Notify(method string, params protocol.Message) error {
	return h.write(protocol.Message{"method": method, "params": params})
}

// Respond sends a manual answer to a server-initiated request.
func (h *Harness) Respond(id any, result protocol.Message) error {
	return h.write(protocol.Result(id, result))
}

func (h *Harness) deliver(id any, message protocol.Message) {
	key := protocol.IDKey(id)
	h.mu.Lock()
	reply := h.pending[key]
	delete(h.pending, key)
	h.mu.Unlock()
	if reply != nil {
		reply <- message
	}
}

func (h *Harness) handlePermission(id any, message protocol.Message) {
	h.mu.Lock()
	if params := protocol.Sub(message, "params"); params != nil {
		h.permRequests = append(h.permRequests, params)
	}
	choice := h.PermissionChoice
	h.mu.Unlock()

	switch choice {
	case "manual":
		return
	case "error":
		_ = h.write(protocol.RPCErrorResponse(id, -32601, "client does not implement permissions", nil))
	case "cancelled":
		_ = h.Respond(id, protocol.Message{
			"outcome": protocol.Message{"outcome": protocol.OutcomeCancelled},
		})
	default:
		_ = h.Respond(id, protocol.Message{
			"outcome": protocol.Message{
				"outcome":  protocol.OutcomeSelected,
				"optionId": choice,
			},
		})
	}
}

// Updates returns a snapshot of received session/update params.
func (h *Harness) Updates() []protocol.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.Message(nil), h.updates...)
}

// PermissionRequests returns the permission params seen so far.
func (h *Harness) PermissionRequests() []protocol.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.Message(nil), h.permRequests...)
}

// WaitUpdates blocks until at least n session/update notifications arrived.
func (h *Harness) WaitUpdates(n int, timeout time.Duration) ([]protocol.Message, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if len(h.Updates()) >= n {
			return h.Updates(), nil
		}
		select {
		case <-h.updateCh:
		case <-deadline.C:
			return h.Updates(), fmt.Errorf("only %d/%d updates after %s", len(h.Updates()), n, timeout)
		}
	}
}
