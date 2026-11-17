package claudecode

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

	"claude-acp-bridge/internal/protocol"
)

// Config describes how to launch one Claude CLI stream-json process.
type Config struct {
	// Bin is the Claude CLI executable (default "claude").
	Bin string
	// ExtraArgs are appended to the fixed stream-json flag set (e.g. --effort).
	ExtraArgs []string
	// ExtraEnv is appended to the inherited environment.
	ExtraEnv []string
	// Dir is the child process working directory.
	Dir string
	// SessionID is the UUID passed via --session-id for a brand-new session.
	// Exactly one of SessionID / ResumeID should be set.
	SessionID string
	// ResumeID is passed via --resume to continue an existing conversation.
	ResumeID string
	// Model is the optional --model id.
	Model string
	// PermissionMode is the CLI mode: manual|acceptEdits|auto|bypassPermissions|plan|dontAsk.
	PermissionMode string
	// MCPConfigPath is an optional path to a JSON MCP config (--mcp-config).
	MCPConfigPath string
	// Log is an optional wire recorder.
	Log *protocol.WireLog
}

// Client owns one Claude CLI process plus its stream-json connection.
type Client struct {
	cmd  *exec.Cmd
	conn *Conn

	handlerMu sync.RWMutex
	onEvent   func(protocol.Message)
	onControl func(string, protocol.Message)
	initInfo  protocol.Message

	stderr    *stderrTail
	waitCh    chan error
	closeOnce sync.Once
}

// Launch starts one Claude CLI in stream-json mode. It does not send a prompt
// and does not call the model; the CLI emits its system/init event when the
// first user message arrives, so there is no RPC-style startup handshake.
func Launch(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Bin == "" {
		cfg.Bin = "claude"
	}
	args := BuildArgs(cfg)

	cmd := exec.CommandContext(ctx, cfg.Bin, args...)
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
		Closers:   []io.Closer{stdin},
		Log:       cfg.Log,
		OnEvent:   client.emitEvent,
		OnControl: client.emitControl,
	})
	go client.collectStderr(stderrPipe)
	go func() { client.waitCh <- cmd.Wait() }()
	return client, nil
}

// BuildArgs returns the exact argv used to put the CLI in bidirectional
// stream-json mode. Kept separate so doctor/tests can inspect it.
func BuildArgs(cfg Config) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
	}
	mode := cfg.PermissionMode
	if mode == "" {
		mode = "manual"
	}
	args = append(args, "--permission-mode", mode)
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.MCPConfigPath != "" {
		args = append(args, "--mcp-config", cfg.MCPConfigPath)
	}
	if cfg.ResumeID != "" {
		args = append(args, "--resume", cfg.ResumeID)
	} else if cfg.SessionID != "" {
		args = append(args, "--session-id", cfg.SessionID)
	}
	return append(args, cfg.ExtraArgs...)
}

// SetHandlers installs the bridge callbacks. Control requests are dispatched on
// their own goroutines, so a handler may block on a human decision.
func (c *Client) SetHandlers(
	onEvent func(protocol.Message),
	onControl func(requestID string, request protocol.Message),
) {
	c.handlerMu.Lock()
	c.onEvent, c.onControl = onEvent, onControl
	c.handlerMu.Unlock()
}

// emitEvent captures system/init before forwarding to the bridge handler.
func (c *Client) emitEvent(message protocol.Message) {
	if protocol.Str(message, "type") == protocol.ClaudeTypeSystem &&
		protocol.Str(message, "subtype") == protocol.ClaudeSubsystemInit {
		c.handlerMu.Lock()
		c.initInfo = message
		c.handlerMu.Unlock()
	}
	c.handlerMu.RLock()
	handler := c.onEvent
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(message)
	}
}

func (c *Client) emitControl(requestID string, request protocol.Message) {
	c.handlerMu.RLock()
	handler := c.onControl
	c.handlerMu.RUnlock()
	if handler != nil {
		handler(requestID, request)
		return
	}
	_ = c.conn.Send(protocol.Message{
		"type": "control_response",
		"response": protocol.Message{
			"subtype":    "success",
			"request_id": requestID,
			"response":   denyControlResponse("no permission handler configured"),
		},
	})
}

// InitInfo returns the captured system/init payload (may be empty until the
// first user message, when the CLI starts the conversation).
func (c *Client) InitInfo() protocol.Message {
	c.handlerMu.RLock()
	defer c.handlerMu.RUnlock()
	return c.initInfo
}

// SessionID returns the CLI-reported session id from system/init, or empty.
func (c *Client) SessionID() string { return protocol.Str(c.InitInfo(), "session_id") }

// SendUserMessage writes one user turn to stdin. The content is an Anthropic
// content block array already converted from ACP ContentBlocks.
func (c *Client) SendUserMessage(content []any) error {
	return c.conn.Send(protocol.Message{
		"type": "user",
		"message": protocol.Message{
			"role":    "user",
			"content": content,
		},
	})
}

// RespondControl answers a CLI permission ask (can_use_tool). The response is
// the inner object (behavior + updatedInput/message).
func (c *Client) RespondControl(requestID string, response protocol.Message) error {
	return c.conn.Send(protocol.Message{
		"type": "control_response",
		"response": protocol.Message{
			"subtype":    "success",
			"request_id": requestID,
			"response":   response,
		},
	})
}

// Interrupt asks the CLI to abort the in-flight turn.
func (c *Client) Interrupt(ctx context.Context) error {
	_, err := c.conn.Call(ctx, protocol.ClaudeControlInterrupt, nil)
	return err
}

// SetModel switches the model for subsequent turns.
func (c *Client) SetModel(ctx context.Context, model string) error {
	_, err := c.conn.Call(ctx, protocol.ClaudeControlSetModel, protocol.Message{"model": model})
	return err
}

// SetMode switches the permission mode via the CLI control API.
func (c *Client) SetMode(ctx context.Context, mode string) error {
	_, err := c.conn.Call(ctx, protocol.ClaudeControlSetPermissionMode, protocol.Message{"mode": mode})
	return err
}

// Done is closed when the CLI connection ends.
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
		if os.Getenv("CLAUDE_ACP_VERBOSE") != "" {
			Diagnostic("claude stderr: %s", line)
		}
	}
}

// ---- control_response helpers ----

// denyControlResponse builds the can_use_tool "deny" answer. A message is
// required by the CLI on deny.
func denyControlResponse(message string) protocol.Message {
	if strings.TrimSpace(message) == "" {
		message = "User denied this action"
	}
	return protocol.Message{
		"behavior": protocol.ClaudeBehaviorDeny,
		"message":  message,
	}
}

// StartupError augments a launch failure with the child's stderr.
func StartupError(err error, tail string) error {
	if err == nil {
		return nil
	}
	if strings.TrimSpace(tail) == "" {
		return err
	}
	return errors.New(err.Error() + "\nclaude stderr (last lines):\n" + tail)
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
