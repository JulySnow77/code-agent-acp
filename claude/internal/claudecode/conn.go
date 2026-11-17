// Package claudecode drives one `claude -p --input-format stream-json
// --output-format stream-json` child process. The stream-json protocol is plain
// NDJSON (no JSON-RPC envelope): the CLI emits typed events on stdout and
// accepts user messages plus control responses on stdin.
//
// Unlike Codex's app-server (one process, many threads), Claude Code binds one
// CLI process to one conversation. The bridge therefore opens one Conn per ACP
// session.
package claudecode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"claude-acp-bridge/internal/protocol"
)

// MaxFrameSize bounds one ndjson frame; tool results and hook payloads can be
// large, but an unbounded line is still a memory-exhaustion vector.
const MaxFrameSize = 32 * 1024 * 1024

// ErrClosed is returned by Call/Send when the CLI connection is gone.
var ErrClosed = errors.New("claude stream-json connection closed")

// ConnOptions configures a Conn.
type ConnOptions struct {
	Closers   []io.Closer
	Log       *protocol.WireLog
	OnEvent   func(message protocol.Message)
	OnControl func(requestID string, request protocol.Message)
}

// Conn is a bidirectional stream-json connection to one Claude CLI process.
// One reader goroutine decodes frames; one ordered pump delivers ordinary
// events; every CLI control request gets its own goroutine so a slow human
// approval never stalls the event stream. Control requests we issue (interrupt,
// set_model, set_permission_mode) are correlated by request_id.
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
	events   chan protocol.Message

	onEvent   func(protocol.Message)
	onControl func(string, protocol.Message)

	wg sync.WaitGroup
}

// NewConn starts the reader and event pumps over an arbitrary reader/writer
// pair. Tests use in-memory pipes; production uses child stdio pipes.
func NewConn(in io.Reader, out io.Writer, opts ConnOptions) *Conn {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), MaxFrameSize)
	c := &Conn{
		out:       out,
		closers:   opts.Closers,
		log:       opts.Log,
		pending:   make(map[string]chan protocol.Message),
		closed:    make(chan struct{}),
		events:    make(chan protocol.Message, 1024),
		onEvent:   opts.OnEvent,
		onControl: opts.OnControl,
	}
	c.wg.Add(2)
	go c.readLoop(scanner)
	go c.eventLoop()
	return c
}

// Send writes one bare stream-json frame (a user message or a control_response
// answering a CLI permission ask).
func (c *Conn) Send(message protocol.Message) error {
	body, err := protocol.StreamFrame(message)
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
	c.log.Record("to-claude", body[:len(body)-1])
	return nil
}

// Call issues a control_request to the CLI and waits for the matching
// control_response (or ctx cancellation / connection close). The returned
// Message is the inner `response.response` object when present.
func (c *Conn) Call(ctx context.Context, subtype string, fields protocol.Message) (protocol.Message, error) {
	id := fmt.Sprintf("acp-%d", c.nextID.Add(1))
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

	request := protocol.Message{"subtype": subtype}
	for k, v := range fields {
		request[k] = v
	}
	frame := protocol.Message{
		"type":       protocol.ClaudeTypeControlReq,
		"request_id": id,
		"request":    request,
	}
	if err := c.Send(frame); err != nil {
		c.dropPending(key)
		return nil, err
	}

	select {
	case message := <-reply:
		// A response with subtype=error is surfaced as an error.
		if protocol.Str(message, "subtype") == "error" {
			errText := protocol.Str(message, "error")
			if errText == "" {
				errText = protocol.StringValue(message["error"])
			}
			return nil, errors.New(errText)
		}
		inner := protocol.Sub(message, "response")
		if inner == nil {
			inner = protocol.Message{}
		}
		return inner, nil
	case <-ctx.Done():
		c.dropPending(key)
		return nil, ctx.Err()
	case <-c.closed:
		c.dropPending(key)
		return nil, c.failure()
	}
}

func (c *Conn) readLoop(scanner *bufio.Scanner) {
	defer c.wg.Done()
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		c.log.Record("from-claude", line)
		message, err := protocol.ParseFrame(line, false)
		if err != nil {
			Diagnostic("ignoring malformed stream-json frame: %v", err)
			continue
		}
		switch protocol.Str(message, "type") {
		case protocol.ClaudeTypeControlReq:
			// CLI -> SDK permission ask. Run independently: the handler may
			// wait for a human while events keep flowing.
			requestID := protocol.Str(message, "request_id")
			request := protocol.Sub(message, "request")
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				c.deliverControl(requestID, request)
			}()
		case protocol.ClaudeTypeControlResp:
			c.deliverResponse(message)
		default:
			// Ordered delivery matters for assistant/tool/result streams.
			select {
			case c.events <- message:
			case <-c.closed:
				return
			}
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
		case message := <-c.events:
			if c.onEvent != nil {
				c.onEvent(message)
			}
		case <-c.closed:
			for {
				select {
				case message := <-c.events:
					if c.onEvent != nil {
						c.onEvent(message)
					}
				default:
					return
				}
			}
		}
	}
}

func (c *Conn) deliverControl(requestID string, request protocol.Message) {
	if c.onControl == nil {
		// No handler: deny rather than hang the CLI's turn.
		_ = c.Send(protocol.Message{
			"type": "control_response",
			"response": protocol.Message{
				"subtype":    "success",
				"request_id": requestID,
				"response":   denyControlResponse("no permission handler configured"),
			},
		})
		return
	}
	c.onControl(requestID, request)
}

func (c *Conn) deliverResponse(message protocol.Message) {
	payload := protocol.Sub(message, "response")
	if payload == nil {
		payload = message
	}
	id := protocol.Str(payload, "request_id")
	if id == "" {
		id = protocol.Str(message, "request_id")
	}
	key := protocol.IDKey(id)
	c.pendMu.Lock()
	reply := c.pending[key]
	delete(c.pending, key)
	c.pendMu.Unlock()
	if reply != nil {
		reply <- payload
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
// in-flight handlers; the owning Client reaps the child process.
func (c *Conn) Close() { c.shutdown(io.EOF) }

func (c *Conn) failure() error {
	if c.closeErr != nil && !errors.Is(c.closeErr, io.EOF) {
		return fmt.Errorf("%w: %v", ErrClosed, c.closeErr)
	}
	return ErrClosed
}

// Done is closed when the transport has stopped.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Wait reaps the reader/event goroutines. Call after the child is gone.
func (c *Conn) Wait() { c.wg.Wait() }
