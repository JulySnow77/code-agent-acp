// Package client implements the ACP Client side: it drives an ACP Agent
// (for example `mcode acp`) and serves the agent's reverse calls such as
// session/request_permission, fs/* and terminal/*.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"minimax-acp/acp"
)

var ErrUnsupportedProtocolVersion = errors.New("acp: agent selected an unsupported protocol version")

// HandlerFuncs are the callbacks an Agent may invoke on the Client. Nil fields
// are not registered, so the peer receives -32601 methodNotFound (which is the
// correct answer for a capability the client did not advertise).
type HandlerFuncs struct {
	SessionUpdate     func(notification *acp.SessionNotification)
	RequestPermission func(ctx context.Context, p *acp.RequestPermissionParams) (*acp.RequestPermissionResult, *acp.RPCError)
	ReadTextFile      func(ctx context.Context, p acp.ReadTextFileParams) (*acp.ReadTextFileResult, *acp.RPCError)
	WriteTextFile     func(ctx context.Context, p acp.WriteTextFileParams) (*acp.EmptyResult, *acp.RPCError)
	TerminalCreate    func(ctx context.Context, p acp.CreateTerminalParams) (*acp.CreateTerminalResult, *acp.RPCError)
	TerminalOutput    func(ctx context.Context, p acp.TerminalOutputParams) (*acp.TerminalOutputResult, *acp.RPCError)
	TerminalWaitExit  func(ctx context.Context, p acp.WaitForTerminalExitParams) (*acp.WaitForTerminalExitResult, *acp.RPCError)
	TerminalKill      func(ctx context.Context, p acp.KillTerminalParams) (*acp.EmptyResult, *acp.RPCError)
	TerminalRelease   func(ctx context.Context, p acp.ReleaseTerminalParams) (*acp.EmptyResult, *acp.RPCError)

	// ExtRequest / ExtNotification catch vendor methods (prefixed with "_").
	ExtRequest      func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RPCError)
	ExtNotification func(method string, params json.RawMessage)
}

type Options struct {
	Info            acp.Implementation
	Capabilities    acp.ClientCapabilities
	ProtocolVersion int
	Handlers        HandlerFuncs
	ConnOptions     acp.ConnOptions
}

// Client wraps a full-duplex Conn with ACP-specific convenience methods.
type Client struct {
	conn *acp.Conn
	opts Options

	mu   sync.RWMutex
	init *acp.InitializeResult
}

// NewClient wires handlers onto an existing Conn. The caller must start
// conn.Run itself; use Dial for the common case.
func NewClient(conn *acp.Conn, opts Options) *Client {
	applyDefaults(&opts)
	c := &Client{conn: conn, opts: opts}
	c.registerHandlers()
	return c
}

// Dial creates a Conn over tr, registers handlers and starts the read loop.
// The connection lives until Close (or transport EOF), independent of any
// per-request context.
func Dial(tr acp.Transport, opts Options) *Client {
	applyDefaults(&opts)
	c := &Client{conn: acp.NewConn(tr, opts.ConnOptions), opts: opts}
	c.registerHandlers()
	go func() { _ = c.conn.Run(context.Background()) }()
	return c
}

func applyDefaults(o *Options) {
	if o.Info.Name == "" {
		o.Info = acp.Implementation{Name: "minimax-acp-go", Title: "minimax-acp Go client", Version: "00.00.01"}
	}
	if o.Info.Version == "" {
		o.Info.Version = "00.00.01"
	}
	if o.ProtocolVersion == 0 {
		o.ProtocolVersion = acp.ProtocolVersionV1
	}
}

func (c *Client) Conn() *acp.Conn       { return c.conn }
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }
func (c *Client) Close()                { c.conn.Close() }

// InitializeResult returns the negotiated capabilities, or nil before init.
func (c *Client) InitializeResult() *acp.InitializeResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.init
}

func (c *Client) registerHandlers() {
	h := c.opts.Handlers
	conn := c.conn

	conn.OnNotification(acp.MethodSessionUpdate, func(_ context.Context, raw json.RawMessage) {
		if h.SessionUpdate == nil {
			return
		}
		var n acp.SessionNotification
		if err := json.Unmarshal(raw, &n); err != nil {
			c.logf("cannot decode session/update: %v", err)
			return
		}
		h.SessionUpdate(&n)
	})

	registerRequest(conn, acp.MethodSessionRequestPermission, h.RequestPermission == nil,
		func() any { return new(acp.RequestPermissionParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			r, e := h.RequestPermission(ctx, v.(*acp.RequestPermissionParams))
			if e != nil {
				return nil, e
			}
			if r == nil {
				return acp.CancelledOutcome(), nil
			}
			return r, nil
		})
	registerRequest(conn, acp.MethodFSReadTextFile, h.ReadTextFile == nil,
		func() any { return new(acp.ReadTextFileParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.ReadTextFile(ctx, *v.(*acp.ReadTextFileParams))
		})
	registerRequest(conn, acp.MethodFSWriteTextFile, h.WriteTextFile == nil,
		func() any { return new(acp.WriteTextFileParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.WriteTextFile(ctx, *v.(*acp.WriteTextFileParams))
		})
	registerRequest(conn, acp.MethodTerminalCreate, h.TerminalCreate == nil,
		func() any { return new(acp.CreateTerminalParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.TerminalCreate(ctx, *v.(*acp.CreateTerminalParams))
		})
	registerRequest(conn, acp.MethodTerminalOutput, h.TerminalOutput == nil,
		func() any { return new(acp.TerminalOutputParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.TerminalOutput(ctx, *v.(*acp.TerminalOutputParams))
		})
	registerRequest(conn, acp.MethodTerminalWaitForExit, h.TerminalWaitExit == nil,
		func() any { return new(acp.WaitForTerminalExitParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.TerminalWaitExit(ctx, *v.(*acp.WaitForTerminalExitParams))
		})
	registerRequest(conn, acp.MethodTerminalKill, h.TerminalKill == nil,
		func() any { return new(acp.KillTerminalParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.TerminalKill(ctx, *v.(*acp.KillTerminalParams))
		})
	registerRequest(conn, acp.MethodTerminalRelease, h.TerminalRelease == nil,
		func() any { return new(acp.ReleaseTerminalParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.TerminalRelease(ctx, *v.(*acp.ReleaseTerminalParams))
		})

	if h.ExtRequest != nil {
		conn.OnDefaultRequest(func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RPCError) {
			return h.ExtRequest(ctx, method, params)
		})
	}
	if h.ExtNotification != nil {
		conn.OnDefaultNotification(func(_ context.Context, method string, params json.RawMessage) {
			h.ExtNotification(method, params)
		})
	}
}

// registerRequest wires a typed handler. If the callback is absent, the method
// is deliberately left unregistered so acp.Conn answers methodNotFound.
func registerRequest(conn *acp.Conn, method string, absent bool, fresh func() any, call func(context.Context, any) (any, *acp.RPCError)) {
	if absent {
		return
	}
	conn.OnRequest(method, func(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
		v := fresh()
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, v); err != nil {
				return nil, acp.InvalidParams("%s: %v", method, err)
			}
		}
		return call(ctx, v)
	})
}

func (c *Client) logf(format string, args ...any) {
	if c.opts.ConnOptions.Logger != nil {
		c.opts.ConnOptions.Logger.Printf(format, args...)
	}
}

// ---------- Agent methods ----------

func (c *Client) Initialize(ctx context.Context) (*acp.InitializeResult, error) {
	params := acp.InitializeParams{
		ProtocolVersion:    c.opts.ProtocolVersion,
		ClientCapabilities: c.opts.Capabilities,
		ClientInfo:         &c.opts.Info,
	}
	var result acp.InitializeResult
	if err := c.conn.Call(ctx, acp.MethodInitialize, params, &result); err != nil {
		return nil, err
	}
	if result.ProtocolVersion != acp.ProtocolVersionV1 {
		return nil, fmt.Errorf("%w: got %d (client supports 1)", ErrUnsupportedProtocolVersion, result.ProtocolVersion)
	}
	c.mu.Lock()
	c.init = &result
	c.mu.Unlock()
	return &result, nil
}

func (c *Client) Authenticate(ctx context.Context, methodID string) error {
	return c.conn.Call(ctx, acp.MethodAuthenticate, acp.AuthenticateParams{MethodID: methodID}, &acp.EmptyResult{})
}

func (c *Client) Logout(ctx context.Context) error {
	return c.conn.Call(ctx, acp.MethodLogout, map[string]any{}, &acp.EmptyResult{})
}

func (c *Client) NewSession(ctx context.Context, params acp.NewSessionParams) (*acp.NewSessionResult, error) {
	if params.MCPServers == nil {
		params.MCPServers = []acp.MCPServer{} // required array in schema/v1
	}
	var out acp.NewSessionResult
	if err := c.conn.Call(ctx, acp.MethodSessionNew, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LoadSession replays history via session/update notifications; the result is
// usually empty but may carry modes/configOptions.
func (c *Client) LoadSession(ctx context.Context, params acp.LoadSessionParams) (*acp.SessionStateResult, error) {
	if params.MCPServers == nil {
		params.MCPServers = []acp.MCPServer{}
	}
	var out acp.SessionStateResult
	if err := c.conn.Call(ctx, acp.MethodSessionLoad, params, &out); err != nil {
		return nil, err
	}
	_ = c.FlushNotifications(context.Background()) // replayed history
	return &out, nil
}

func (c *Client) ResumeSession(ctx context.Context, params acp.ResumeSessionParams) (*acp.SessionStateResult, error) {
	var out acp.SessionStateResult
	if err := c.conn.Call(ctx, acp.MethodSessionResume, params, &out); err != nil {
		return nil, err
	}
	_ = c.FlushNotifications(context.Background())
	return &out, nil
}

func (c *Client) ListSessions(ctx context.Context, params acp.ListSessionsParams) (*acp.ListSessionsResult, error) {
	var out acp.ListSessionsResult
	if err := c.conn.Call(ctx, acp.MethodSessionList, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	return c.conn.Call(ctx, acp.MethodSessionClose, acp.CloseSessionParams{SessionID: sessionID}, &acp.EmptyResult{})
}

func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	return c.conn.Call(ctx, acp.MethodSessionDelete, acp.DeleteSessionParams{SessionID: sessionID}, &acp.EmptyResult{})
}

func (c *Client) ForkSession(ctx context.Context, params acp.ForkSessionParams) (json.RawMessage, error) {
	return c.conn.CallRaw(ctx, acp.MethodSessionFork, mustMarshal(params))
}

func (c *Client) SetMode(ctx context.Context, sessionID, modeID string) error {
	return c.conn.Call(ctx, acp.MethodSessionSetMode,
		acp.SetSessionModeParams{SessionID: sessionID, ModeID: modeID}, &acp.EmptyResult{})
}

func (c *Client) SetConfigOption(ctx context.Context, sessionID, configID, value string) error {
	return c.conn.Call(ctx, acp.MethodSessionSetConfigOpt,
		acp.SetSessionConfigOptionParams{SessionID: sessionID, ConfigID: configID, Value: value}, &acp.EmptyResult{})
}

func (c *Client) Prompt(ctx context.Context, sessionID string, blocks []acp.ContentBlock) (*acp.PromptResult, error) {
	if sessionID == "" {
		return nil, errors.New("acp: empty sessionId")
	}
	if len(blocks) == 0 {
		return nil, errors.New("acp: empty prompt")
	}
	var out acp.PromptResult
	if err := c.conn.Call(ctx, acp.MethodSessionPrompt,
		acp.PromptParams{SessionID: sessionID, Prompt: blocks}, &out); err != nil {
		return nil, err
	}
	// The agent sends session/update before its response, but our ordered
	// notification worker may still be rendering the tail chunks. Flush so
	// callers see all streamed text before they inspect stopReason.
	if err := c.FlushNotifications(context.Background()); err != nil {
		c.logf("flush session/update: %v", err)
	}
	return &out, nil
}

// FlushNotifications waits for all notifications received so far to be handled.
func (c *Client) FlushNotifications(ctx context.Context) error {
	flushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.conn.FlushNotifications(flushCtx)
}

// Cancel is a notification; it never waits for a response. The pending
// session/prompt call finishes later with stopReason=cancelled.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.conn.Notify(ctx, acp.MethodSessionCancel, acp.CancelNotificationParams{SessionID: sessionID})
}

// RawCall forwards an arbitrary ACP method (including vendor extensions).
func (c *Client) RawCall(ctx context.Context, method string, params any, out any) error {
	return c.conn.Call(ctx, method, params, out)
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
