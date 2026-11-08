// Package agent implements the ACP Agent side: a Go program can serve ACP v1
// over stdio (so Zed / JetBrains can launch it) and, while handling a prompt,
// call back into the Client for permission, fs/* and terminal/* operations.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sync"

	"kimi-acp/acp"
)

// ErrCapabilityNotAdvertised is returned by Emitter when the client did not
// advertise the capability needed for a reverse call.
var ErrCapabilityNotAdvertised = errors.New("acp: client did not advertise this capability")

// HandlerFuncs are the Agent's method implementations. Nil fields are left
// unregistered; the peer then receives -32601 methodNotFound, which is exactly
// how ACP signals an unimplemented optional method.
type HandlerFuncs struct {
	Initialize      func(ctx context.Context, p acp.InitializeParams) (*acp.InitializeResult, *acp.RPCError)
	Authenticate    func(ctx context.Context, p acp.AuthenticateParams) (*acp.EmptyResult, *acp.RPCError)
	Logout          func(ctx context.Context) (*acp.EmptyResult, *acp.RPCError)
	NewSession      func(ctx context.Context, p acp.NewSessionParams) (*acp.NewSessionResult, *acp.RPCError)
	LoadSession     func(ctx context.Context, p acp.LoadSessionParams, e *Emitter) (*acp.SessionStateResult, *acp.RPCError)
	ResumeSession   func(ctx context.Context, p acp.ResumeSessionParams, e *Emitter) (*acp.SessionStateResult, *acp.RPCError)
	ForkSession     func(ctx context.Context, p acp.ForkSessionParams, e *Emitter) (json.RawMessage, *acp.RPCError)
	ListSessions    func(ctx context.Context, p acp.ListSessionsParams) (*acp.ListSessionsResult, *acp.RPCError)
	CloseSession    func(ctx context.Context, p acp.CloseSessionParams) (*acp.EmptyResult, *acp.RPCError)
	DeleteSession   func(ctx context.Context, p acp.DeleteSessionParams) (*acp.EmptyResult, *acp.RPCError)
	SetMode         func(ctx context.Context, p acp.SetSessionModeParams) (*acp.EmptyResult, *acp.RPCError)
	SetConfigOption func(ctx context.Context, p acp.SetSessionConfigOptionParams) (*acp.EmptyResult, *acp.RPCError)
	Prompt          func(ctx context.Context, p acp.PromptParams, e *Emitter) (*acp.PromptResult, *acp.RPCError)
	Cancel          func(ctx context.Context, p acp.CancelNotificationParams)

	ExtRequest      func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RPCError)
	ExtNotification func(ctx context.Context, method string, params json.RawMessage)
}

type Options struct {
	Info        acp.Implementation
	ConnOptions acp.ConnOptions
}

// Agent is a registered ACP server over one Conn.
type Agent struct {
	conn     *acp.Conn
	handlers HandlerFuncs
	opts     Options
	emitter  *Emitter

	mu      sync.Mutex
	caps    acp.ClientCapabilities
	cancels map[string]context.CancelFunc
}

// New wires handlers onto a Conn. Caller must start conn.Run itself; Serve is
// the one-call version.
func New(tr acp.Transport, h HandlerFuncs, opts Options) *Agent {
	if opts.Info.Name == "" {
		opts.Info = acp.Implementation{Name: "kimi-acp-go-agent", Title: "kimi-acp Go agent", Version: "00.00.01"}
	}
	if opts.Info.Version == "" {
		opts.Info.Version = "00.00.01"
	}
	if opts.ConnOptions.Logger == nil {
		opts.ConnOptions.Logger = log.New(io.Discard, "", 0)
	}
	a := &Agent{
		conn:     acp.NewConn(tr, opts.ConnOptions),
		handlers: h,
		opts:     opts,
		cancels:  make(map[string]context.CancelFunc),
	}
	a.emitter = &Emitter{conn: a.conn, agent: a}
	a.register()
	return a
}

func (a *Agent) Conn() *acp.Conn       { return a.conn }
func (a *Agent) Emitter() *Emitter     { return a.emitter }
func (a *Agent) Close()                { a.conn.Close() }
func (a *Agent) Done() <-chan struct{} { return a.conn.Done() }

// Serve runs the ACP stdio server until EOF or ctx cancellation.
func Serve(ctx context.Context, tr acp.Transport, h HandlerFuncs, opts Options) error {
	return New(tr, h, opts).conn.Run(ctx)
}

func (a *Agent) register() {
	h := a.handlers
	c := a.conn

	c.OnRequest(acp.MethodInitialize, func(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
		var p acp.InitializeParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, acp.InvalidParams("%v", err)
			}
		}
		a.mu.Lock()
		a.caps = p.ClientCapabilities
		a.mu.Unlock()
		if h.Initialize != nil {
			return h.Initialize(ctx, p)
		}
		return &acp.InitializeResult{
			ProtocolVersion: acp.ProtocolVersionV1,
			AgentInfo:       &a.opts.Info,
		}, nil
	})

	register(c, acp.MethodAuthenticate, h.Authenticate == nil,
		func() any { return new(acp.AuthenticateParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.Authenticate(ctx, *v.(*acp.AuthenticateParams))
		})
	register(c, acp.MethodLogout, h.Logout == nil,
		func() any { return &struct{}{} },
		func(ctx context.Context, _ any) (any, *acp.RPCError) { return h.Logout(ctx) })
	register(c, acp.MethodSessionNew, h.NewSession == nil,
		func() any { return new(acp.NewSessionParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.NewSession(ctx, *v.(*acp.NewSessionParams))
		})
	register(c, acp.MethodSessionLoad, h.LoadSession == nil,
		func() any { return new(acp.LoadSessionParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.LoadSession(ctx, *v.(*acp.LoadSessionParams), a.emitter)
		})
	register(c, acp.MethodSessionResume, h.ResumeSession == nil,
		func() any { return new(acp.ResumeSessionParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.ResumeSession(ctx, *v.(*acp.ResumeSessionParams), a.emitter)
		})
	register(c, acp.MethodSessionList, h.ListSessions == nil,
		func() any { return new(acp.ListSessionsParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.ListSessions(ctx, *v.(*acp.ListSessionsParams))
		})
	register(c, acp.MethodSessionClose, h.CloseSession == nil,
		func() any { return new(acp.CloseSessionParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.CloseSession(ctx, *v.(*acp.CloseSessionParams))
		})
	register(c, acp.MethodSessionDelete, h.DeleteSession == nil,
		func() any { return new(acp.DeleteSessionParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.DeleteSession(ctx, *v.(*acp.DeleteSessionParams))
		})
	register(c, acp.MethodSessionSetMode, h.SetMode == nil,
		func() any { return new(acp.SetSessionModeParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.SetMode(ctx, *v.(*acp.SetSessionModeParams))
		})
	register(c, acp.MethodSessionSetConfigOpt, h.SetConfigOption == nil,
		func() any { return new(acp.SetSessionConfigOptionParams) },
		func(ctx context.Context, v any) (any, *acp.RPCError) {
			return h.SetConfigOption(ctx, *v.(*acp.SetSessionConfigOptionParams))
		})

	if h.ForkSession != nil {
		c.OnRequest(acp.MethodSessionFork, func(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
			var p acp.ForkSessionParams
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &p); err != nil {
					return nil, acp.InvalidParams("%v", err)
				}
			}
			return h.ForkSession(ctx, p, a.emitter)
		})
	}

	// session/prompt gets a cancellable context; session/cancel cancels it.
	c.OnRequest(acp.MethodSessionPrompt, func(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
		if h.Prompt == nil {
			return nil, acp.MethodNotFound(acp.MethodSessionPrompt)
		}
		var p acp.PromptParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.InvalidParams("%v", err)
		}
		promptCtx, cancel := context.WithCancel(ctx)
		a.mu.Lock()
		a.cancels[p.SessionID] = cancel
		a.mu.Unlock()
		defer func() {
			cancel()
			a.mu.Lock()
			delete(a.cancels, p.SessionID)
			a.mu.Unlock()
		}()
		if p.SessionID == "" {
			return nil, acp.InvalidParams("sessionId is required")
		}
		return h.Prompt(promptCtx, p, a.emitter)
	})

	c.OnNotification(acp.MethodSessionCancel, func(ctx context.Context, raw json.RawMessage) {
		var p acp.CancelNotificationParams
		_ = json.Unmarshal(raw, &p)
		a.mu.Lock()
		cancel := a.cancels[p.SessionID]
		a.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if h.Cancel != nil {
			h.Cancel(ctx, p)
		}
	})

	if h.ExtRequest != nil {
		c.OnDefaultRequest(func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RPCError) {
			return h.ExtRequest(ctx, method, params)
		})
	}
	if h.ExtNotification != nil {
		c.OnDefaultNotification(func(ctx context.Context, method string, params json.RawMessage) {
			h.ExtNotification(ctx, method, params)
		})
	}
}

func register(c *acp.Conn, method string, absent bool, fresh func() any, call func(context.Context, any) (any, *acp.RPCError)) {
	if absent {
		return
	}
	c.OnRequest(method, func(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
		v := fresh()
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, v); err != nil {
				return nil, acp.InvalidParams("%s: %v", method, err)
			}
		}
		return call(ctx, v)
	})
}

// Emitter sends Agent -> Client notifications and reverse calls. It checks the
// capabilities negotiated by initialize before calling optional client methods.
type Emitter struct {
	conn  *acp.Conn
	agent *Agent
}

func (e *Emitter) capabilities() acp.ClientCapabilities {
	e.agent.mu.Lock()
	defer e.agent.mu.Unlock()
	return e.agent.caps
}

func (e *Emitter) SessionUpdate(ctx context.Context, sessionID string, u acp.SessionUpdate) error {
	return e.conn.Notify(ctx, acp.MethodSessionUpdate, acp.SessionNotification{SessionID: sessionID, Update: u})
}

func (e *Emitter) AgentMessage(ctx context.Context, sessionID, text string) error {
	return e.SessionUpdate(ctx, sessionID, acp.AgentMessageChunk(text))
}

func (e *Emitter) AgentThought(ctx context.Context, sessionID, text string) error {
	return e.SessionUpdate(ctx, sessionID, acp.AgentThoughtChunk(text))
}

func (e *Emitter) ToolCall(ctx context.Context, sessionID string, r acp.ToolCallReport) error {
	return e.SessionUpdate(ctx, sessionID, r.SessionUpdate())
}

func (e *Emitter) ToolCallUpdate(ctx context.Context, sessionID string, r acp.ToolCallUpdateReport) error {
	return e.SessionUpdate(ctx, sessionID, r.SessionUpdate())
}

func (e *Emitter) RequestPermission(ctx context.Context, p acp.RequestPermissionParams) (*acp.RequestPermissionResult, error) {
	var out acp.RequestPermissionResult
	if err := e.conn.Call(ctx, acp.MethodSessionRequestPermission, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *Emitter) ReadTextFile(ctx context.Context, p acp.ReadTextFileParams) (*acp.ReadTextFileResult, error) {
	caps := e.capabilities()
	if caps.FS == nil || !caps.FS.ReadTextFile {
		return nil, ErrCapabilityNotAdvertised
	}
	var out acp.ReadTextFileResult
	if err := e.conn.Call(ctx, acp.MethodFSReadTextFile, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *Emitter) WriteTextFile(ctx context.Context, p acp.WriteTextFileParams) error {
	caps := e.capabilities()
	if caps.FS == nil || !caps.FS.WriteTextFile {
		return ErrCapabilityNotAdvertised
	}
	return e.conn.Call(ctx, acp.MethodFSWriteTextFile, p, &acp.EmptyResult{})
}

func (e *Emitter) checkTerminal() error {
	if !e.capabilities().Terminal {
		return ErrCapabilityNotAdvertised
	}
	return nil
}

func (e *Emitter) TerminalCreate(ctx context.Context, p acp.CreateTerminalParams) (*acp.CreateTerminalResult, error) {
	if err := e.checkTerminal(); err != nil {
		return nil, err
	}
	var out acp.CreateTerminalResult
	if err := e.conn.Call(ctx, acp.MethodTerminalCreate, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *Emitter) TerminalOutput(ctx context.Context, p acp.TerminalOutputParams) (*acp.TerminalOutputResult, error) {
	if err := e.checkTerminal(); err != nil {
		return nil, err
	}
	var out acp.TerminalOutputResult
	if err := e.conn.Call(ctx, acp.MethodTerminalOutput, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *Emitter) TerminalWaitForExit(ctx context.Context, p acp.WaitForTerminalExitParams) (*acp.WaitForTerminalExitResult, error) {
	if err := e.checkTerminal(); err != nil {
		return nil, err
	}
	var out acp.WaitForTerminalExitResult
	if err := e.conn.Call(ctx, acp.MethodTerminalWaitForExit, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *Emitter) TerminalKill(ctx context.Context, p acp.KillTerminalParams) error {
	if err := e.checkTerminal(); err != nil {
		return err
	}
	return e.conn.Call(ctx, acp.MethodTerminalKill, p, &acp.EmptyResult{})
}

func (e *Emitter) TerminalRelease(ctx context.Context, p acp.ReleaseTerminalParams) error {
	if err := e.checkTerminal(); err != nil {
		return err
	}
	return e.conn.Call(ctx, acp.MethodTerminalRelease, p, &acp.EmptyResult{})
}
