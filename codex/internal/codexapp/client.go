package codexapp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"codex-acp-bridge/internal/protocol"
)

// Config describes how to launch and drive `codex app-server`.
type Config struct {
	// Bin is the Codex CLI executable (default "codex").
	Bin string
	// ExtraArgs are appended after the "app-server" subcommand, e.g.
	// "--listen", "stdio://" or "-c" overrides.
	ExtraArgs []string
	// ExtraEnv is appended to the inherited environment.
	ExtraEnv []string
	// Dir is the child process working directory.
	Dir string
	// OptOut lists app-server notification methods the client wants suppressed.
	OptOut []string
	// Log is an optional wire recorder.
	Log *protocol.WireLog
}

// Client owns one app-server process plus its connection.
type Client struct {
	conn *Conn
	cmd  *exec.Cmd

	handlerMu   sync.RWMutex
	noteHandler func(method string, params protocol.Message)
	reqHandler  func(id any, method string, params protocol.Message)

	initResult protocol.Message
	stderr     *stderrTail
	waitCh     chan error
	closeOnce  sync.Once
}

// Launch starts `codex app-server`, performs the mandatory initialize /
// initialized handshake and returns a ready client. It never sends a prompt.
func Launch(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Bin == "" {
		cfg.Bin = "codex"
	}
	args := append([]string{"app-server"}, cfg.ExtraArgs...)
	cmd := exec.Command(cfg.Bin, args...)
	cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	cmd.Dir = cfg.Dir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	client := &Client{
		cmd:    cmd,
		stderr: newStderrTail(20),
		waitCh: make(chan error, 1),
	}
	client.conn = NewConn(stdout, stdin, ConnOptions{
		Closers:        []io.Closer{stdin},
		Log:            cfg.Log,
		OnNotification: client.dispatchNotification,
		OnRequest:      client.dispatchRequest,
	})
	go client.collectStderr(stderrPipe)
	go func() { client.waitCh <- cmd.Wait() }()

	capabilities := protocol.Message{"experimentalApi": false}
	if len(cfg.OptOut) > 0 {
		capabilities["optOutNotificationMethods"] = cfg.OptOut
	}
	initResult, err := client.conn.Call(ctx, "initialize", protocol.Message{
		"clientInfo": protocol.Message{
			"name":    protocol.AgentName,
			"title":   "Codex ACP Bridge (Go)",
			"version": protocol.VersionString,
		},
		"capabilities": capabilities,
	})
	if err != nil {
		_ = client.Close()
		return nil, StartupError(err, client.stderr.String())
	}
	if err := client.conn.Notify("initialized", nil); err != nil {
		_ = client.Close()
		return nil, StartupError(err, client.stderr.String())
	}
	client.initResult = initResult
	return client, nil
}

// SetHandlers installs the bridge callbacks. Requests are dispatched on their
// own goroutines, so a handler may block on a human decision.
func (c *Client) SetHandlers(
	onNotification func(method string, params protocol.Message),
	onRequest func(id any, method string, params protocol.Message),
) {
	c.handlerMu.Lock()
	c.noteHandler, c.reqHandler = onNotification, onRequest
	c.handlerMu.Unlock()
}

func (c *Client) dispatchNotification(method string, params protocol.Message) {
	c.handlerMu.RLock()
	handler := c.noteHandler
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(method, params)
	}
}

func (c *Client) dispatchRequest(id any, method string, params protocol.Message) {
	c.handlerMu.RLock()
	handler := c.reqHandler
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(id, method, params)
		return
	}
	_ = c.Respond(id, nil, &protocol.RPCError{
		Code:    protocol.CodeMethodNotFound,
		Message: "unsupported app-server request: " + method,
	})
}

// InitializeResult is the initialize response (userAgent, codexHome, ...).
func (c *Client) InitializeResult() protocol.Message { return c.initResult }

// ThreadStart creates a new Codex thread and returns the `thread` object.
func (c *Client) ThreadStart(ctx context.Context, params protocol.Message) (protocol.Message, error) {
	result, err := c.conn.Call(ctx, "thread/start", params)
	if err != nil {
		return nil, err
	}
	return protocol.Sub(result, "thread"), nil
}

// ThreadResume reopens a stored thread; the response carries the full history.
func (c *Client) ThreadResume(ctx context.Context, threadID string, params protocol.Message) (protocol.Message, error) {
	resume := make(protocol.Message, len(params)+1)
	for key, value := range params {
		resume[key] = value
	}
	resume["threadId"] = threadID
	result, err := c.conn.Call(ctx, "thread/resume", resume)
	if err != nil {
		return nil, err
	}
	return protocol.Sub(result, "thread"), nil
}

// TurnStart submits user input and returns the `turn` object. Progress arrives
// afterwards as notifications, never in this response.
func (c *Client) TurnStart(ctx context.Context, params protocol.Message) (protocol.Message, error) {
	result, err := c.conn.Call(ctx, "turn/start", params)
	if err != nil {
		return nil, err
	}
	return protocol.Sub(result, "turn"), nil
}

// TurnInterrupt aborts an in-flight turn.
func (c *Client) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	_, err := c.conn.Call(ctx, "turn/interrupt", protocol.Message{
		"threadId": threadID,
		"turnId":   turnID,
	})
	return err
}

// Respond answers a server-initiated request.
func (c *Client) Respond(id any, result protocol.Message, rpcErr *protocol.RPCError) error {
	return c.conn.Respond(id, result, rpcErr)
}

// Done is closed when the app-server connection ends.
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

// StderrTail returns the last lines of child stderr, for actionable failures.
func (c *Client) StderrTail() string { return c.stderr.String() }

// Close closes stdin, gives the child a short grace period, then kills it.
func (c *Client) Close() error {
	var waitErr error
	c.closeOnce.Do(func() {
		c.conn.Close()
		select {
		case waitErr = <-c.waitCh:
		case <-time.After(2 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			waitErr = <-c.waitCh
		}
	})
	return waitErr
}

func (c *Client) collectStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		c.stderr.Add(line)
		if os.Getenv("CODEX_ACP_VERBOSE") != "" {
			Diagnostic("codex stderr: %s", line)
		}
	}
}

// stderrTail keeps a bounded, thread-safe ring of child stderr lines.
type stderrTail struct {
	mu    sync.Mutex
	limit int
	lines []string
}

func newStderrTail(limit int) *stderrTail { return &stderrTail{limit: limit} }

func (t *stderrTail) Add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.limit {
		t.lines = t.lines[len(t.lines)-t.limit:]
	}
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(append([]string(nil), t.lines...), "\n")
}

// StartupError augments a launch/handshake failure with the child's stderr.
func StartupError(err error, tail string) error {
	if err == nil {
		return nil
	}
	if strings.TrimSpace(tail) == "" {
		return err
	}
	return errors.New(err.Error() + "\ncodex app-server stderr (last lines):\n" + tail)
}
