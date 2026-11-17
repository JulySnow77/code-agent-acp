// Package codexapp drives a `codex app-server` child process over its stdio
// JSON-RPC-lite protocol. It owns request/response correlation, ordered
// notification delivery and the reverse (server -> client) request channel
// used for approvals.
package codexapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"codex-acp-bridge/internal/protocol"
)

// MaxFrameSize bounds one ndjson frame; ACP/Codex have no Content-Length
// framing, so an unbounded line is a memory-exhaustion vector.
const MaxFrameSize = 32 * 1024 * 1024

// ErrClosed is returned by Call when the app-server connection is gone.
var ErrClosed = errors.New("codex app-server connection closed")

type notification struct {
	method string
	params protocol.Message
}

// Conn is a bidirectional JSON-RPC-lite connection to one app-server process.
// One reader goroutine decodes frames; one ordered pump goroutine delivers
// notifications; every server request gets its own goroutine so a slow human
// approval never stalls the notification stream.
type Conn struct {
	out     io.Writer
	closers []io.Closer
	log     *protocol.WireLog

	writeMu  sync.Mutex
	nextID   atomic.Int64
	pending  map[string]chan protocol.Message
	pendMu   sync.Mutex
	closed   chan struct{}
	closeOne sync.Once
	closeErr error
	events   chan notification

	onNotification func(method string, params protocol.Message)
	onRequest      func(id any, method string, params protocol.Message)

	wg sync.WaitGroup
}

// ConnOptions configures a Conn.
type ConnOptions struct {
	Closers        []io.Closer
	Log            *protocol.WireLog
	OnNotification func(method string, params protocol.Message)
	OnRequest      func(id any, method string, params protocol.Message)
}

// NewConn starts the reader and notification pumps over an arbitrary
// reader/writer pair. Tests use in-memory pipes; production uses child pipes.
func NewConn(in io.Reader, out io.Writer, opts ConnOptions) *Conn {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), MaxFrameSize)
	c := &Conn{
		out:            out,
		closers:        opts.Closers,
		log:            opts.Log,
		pending:        make(map[string]chan protocol.Message),
		closed:         make(chan struct{}),
		events:         make(chan notification, 1024),
		onNotification: opts.OnNotification,
		onRequest:      opts.OnRequest,
	}
	c.wg.Add(2)
	go c.readLoop(scanner)
	go c.eventLoop()
	return c
}

// Call issues a request and waits for its response, ctx cancellation, or the
// connection closing. The reply's `result` is returned as a Message.
func (c *Conn) Call(ctx context.Context, method string, params protocol.Message) (protocol.Message, error) {
	id := c.nextID.Add(1)
	reply := make(chan protocol.Message, 1)
	key := protocol.IDKey(id)

	c.pendMu.Lock()
	select {
	case <-c.closed:
		c.pendMu.Unlock()
		return nil, ErrClosed
	default:
	}
	c.pending[key] = reply
	c.pendMu.Unlock()

	frame := protocol.Message{"id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if err := c.write(frame); err != nil {
		c.dropPending(key)
		return nil, err
	}

	select {
	case message := <-reply:
		if raw, ok := message["error"]; ok && raw != nil {
			return nil, decodeRPCError(raw)
		}
		result := protocol.Sub(message, "result")
		if result == nil {
			result = protocol.Message{}
		}
		return result, nil
	case <-ctx.Done():
		c.dropPending(key)
		return nil, ctx.Err()
	case <-c.closed:
		c.dropPending(key)
		return nil, c.failure()
	}
}

// Notify sends a client notification. A nil params omits the member entirely
// (the `initialized` notification has no params in app-server's schema).
func (c *Conn) Notify(method string, params protocol.Message) error {
	frame := protocol.Message{"method": method}
	if params != nil {
		frame["params"] = params
	}
	return c.write(frame)
}

// Respond answers a server-initiated request (approval / elicitation / ...).
func (c *Conn) Respond(id any, result protocol.Message, rpcErr *protocol.RPCError) error {
	var frame protocol.Message
	if rpcErr != nil {
		frame = protocol.RPCErrorResponse(id, rpcErr.Code, rpcErr.Message, rpcErr.Data)
	} else {
		frame = protocol.Result(id, result)
	}
	return c.write(frame)
}

func (c *Conn) write(frame protocol.Message) error {
	body, err := protocol.LiteFrame(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	if _, err := c.out.Write(body); err != nil {
		return err
	}
	if flusher, ok := c.out.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return err
		}
	}
	c.log.Record("to-codex", body[:len(body)-1])
	return nil
}

func (c *Conn) readLoop(scanner *bufio.Scanner) {
	defer c.wg.Done()
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		c.log.Record("from-codex", line)
		message, err := protocol.ParseFrame(line, false)
		if err != nil {
			Diagnostic("ignoring malformed app-server frame: %v", err)
			continue
		}
		_, hasMethod := message["method"]
		id, hasID := message["id"]
		switch {
		case hasMethod && hasID:
			// Server-initiated request. Run independently: its handler may wait
			// for a human decision while notifications keep flowing.
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				c.deliverRequest(id, protocol.Str(message, "method"), paramsOf(message))
			}()
		case hasMethod:
			// Ordered delivery matters for session/update streams.
			select {
			case c.events <- notification{method: protocol.Str(message, "method"), params: paramsOf(message)}:
			case <-c.closed:
				return
			}
		case hasID:
			c.deliverResponse(id, message)
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.shutdown(err)
}

func (c *Conn) eventLoop() {
	defer c.wg.Done()
	for {
		select {
		case event := <-c.events:
			if c.onNotification != nil {
				c.onNotification(event.method, event.params)
			}
		case <-c.closed:
			// Drain what was already decoded before the reader stopped.
			for {
				select {
				case event := <-c.events:
					if c.onNotification != nil {
						c.onNotification(event.method, event.params)
					}
				default:
					return
				}
			}
		}
	}
}

func (c *Conn) deliverRequest(id any, method string, params protocol.Message) {
	if c.onRequest == nil {
		_ = c.Respond(id, nil, &protocol.RPCError{
			Code: protocol.CodeMethodNotFound, Message: "unsupported app-server request: " + method,
		})
		return
	}
	c.onRequest(id, method, params)
}

func (c *Conn) deliverResponse(id any, message protocol.Message) {
	key := protocol.IDKey(id)
	c.pendMu.Lock()
	reply := c.pending[key]
	delete(c.pending, key)
	c.pendMu.Unlock()
	if reply != nil {
		reply <- message
	}
}

func (c *Conn) dropPending(key string) {
	c.pendMu.Lock()
	delete(c.pending, key)
	c.pendMu.Unlock()
}

func (c *Conn) shutdown(err error) {
	c.closeOne.Do(func() {
		c.closeErr = err
		close(c.closed)
		for _, closer := range c.closers {
			if closer != nil {
				_ = closer.Close()
			}
		}
	})
}

// Close closes the transport and unblocks pending calls. It does not wait for
// in-flight request handlers; the owning Client is responsible for reaping the
// child process.
func (c *Conn) Close() {
	c.shutdown(io.EOF)
}

func (c *Conn) failure() error {
	if c.closeErr != nil && !errors.Is(c.closeErr, io.EOF) {
		return fmt.Errorf("%w: %v", ErrClosed, c.closeErr)
	}
	return ErrClosed
}

// Done is closed when the transport has stopped.
func (c *Conn) Done() <-chan struct{} { return c.closed }

func paramsOf(message protocol.Message) protocol.Message {
	params := protocol.Sub(message, "params")
	if params == nil {
		return protocol.Message{}
	}
	return params
}

func decodeRPCError(raw any) error {
	object := protocol.AsMessage(raw)
	if object == nil {
		return &protocol.RPCError{Code: protocol.CodeInternalError, Message: protocol.StringValue(raw)}
	}
	return &protocol.RPCError{
		Code:    int(protocol.Num(object, "code")),
		Message: protocol.Str(object, "message"),
		Data:    object["data"],
	}
}
