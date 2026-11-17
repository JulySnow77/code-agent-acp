// Package bridge translates ACP v1 (client side) to the Codex app-server
// protocol (agent side). It is the Go equivalent of the TypeScript
// @agentclientprotocol/codex-acp adapter: ACP hosts see a normal ACP agent,
// while the real agent is a `codex app-server` child process.
package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"codex-acp-bridge/internal/codexapp"
	"codex-acp-bridge/internal/protocol"
)

// Codex is the subset of the app-server client the bridge depends on. Keeping
// it as an interface lets the e2e tests drive the whole ACP surface with a
// deterministic in-memory fake instead of a real Codex process.
type Codex interface {
	InitializeResult() protocol.Message
	ThreadStart(ctx context.Context, params protocol.Message) (protocol.Message, error)
	ThreadResume(ctx context.Context, threadID string, params protocol.Message) (protocol.Message, error)
	TurnStart(ctx context.Context, params protocol.Message) (protocol.Message, error)
	TurnInterrupt(ctx context.Context, threadID, turnID string) error
	Respond(id any, result protocol.Message, rpcErr *protocol.RPCError) error
	SetHandlers(
		onNotification func(method string, params protocol.Message),
		onRequest func(id any, method string, params protocol.Message),
	)
	Done() <-chan struct{}
	Close() error
}

// Config holds bridge behaviour. All of it is overridable from the CLI / env.
type Config struct {
	// ApprovalPolicy maps to app-server's `approvalPolicy`: on-request (default),
	// never (yolo) or untrusted.
	ApprovalPolicy string
	// Sandbox maps to thread/start.sandbox: read-only | workspace-write |
	// danger-full-access.
	Sandbox string
	// Model is an optional model id applied at thread/start and turn/start.
	Model string
	// CWD is the fallback working directory for session/new.
	CWD string
	// TurnTimeout bounds one ACP prompt.
	TurnTimeout time.Duration
	// PermissionFallback decides what to answer when the ACP client cannot
	// handle session/request_permission: "decline" (default) or "accept".
	PermissionFallback string
	// Log optionally records raw ACP frames.
	Log *protocol.WireLog
}

// Mode names exposed over ACP. They map directly onto app-server's
// AskForApproval so session/set_mode has a well-defined effect.
const (
	ModeAsk       = "ask"
	ModeAuto      = "auto"
	ModeUntrusted = "untrusted"
)

var availableModes = []protocol.Message{
	{"id": ModeAsk, "name": "Ask", "description": "Ask before every command or file change (approvalPolicy: on-request)"},
	{"id": ModeAuto, "name": "Auto", "description": "Run without asking (approvalPolicy: never)"},
	{"id": ModeUntrusted, "name": "Untrusted", "description": "Ask for everything, including reads (approvalPolicy: untrusted)"},
}

// Server is an ACP v1 stdio server backed by one app-server connection.
//
// A single reader goroutine owns stdin. Requests and notifications are handled
// on their own goroutines, and replies to server-initiated calls (permissions)
// are routed back by JSON-RPC id. This is what makes a nested request possible:
// Codex asks us for an approval -> we ask the ACP client -> the reply arrives
// on the same stdin while the prompt handler is still running.
type Server struct {
	in       io.Reader
	inCloser io.Closer
	out      io.Writer
	writeM   sync.Mutex
	codex    Codex
	cfg      Config
	log      *protocol.WireLog

	clientSeq atomic.Int64
	pending   map[string]chan protocol.Message
	pendMu    sync.Mutex

	sessions map[string]*Session
	sessMu   sync.RWMutex

	closed    chan struct{}
	closeOnce sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// New builds a server. The caller owns the Codex lifetime; Run closes it.
func New(codex Codex, in io.Reader, out io.Writer, cfg Config) *Server {
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = 30 * time.Minute
	}
	if cfg.ApprovalPolicy == "" {
		cfg.ApprovalPolicy = "on-request"
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "workspace-write"
	}
	if cfg.PermissionFallback == "" {
		cfg.PermissionFallback = "decline"
	}
	ctx, cancel := context.WithCancel(context.Background())
	inCloser, _ := in.(io.Closer)
	s := &Server{
		in:       in,
		inCloser: inCloser,
		out:      out,
		codex:    codex,
		cfg:      cfg,
		log:      cfg.Log,
		pending:  make(map[string]chan protocol.Message),
		sessions: make(map[string]*Session),
		closed:   make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	codex.SetHandlers(s.handleCodexNotification, s.handleCodexRequest)
	return s
}

// Run serves ACP frames until stdin closes, the app-server dies, or the
// process is interrupted. A clean EOF is not an error.
func (s *Server) Run() error {
	defer func() {
		s.shutdown()
		s.wg.Wait()
		_ = s.codex.Close()
	}()

	go func() {
		select {
		case <-s.codex.Done():
			codexapp.Diagnostic("app-server connection closed; ending ACP session")
			s.shutdown()
		case <-s.closed:
		}
	}()

	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 64*1024), codexapp.MaxFrameSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		s.log.Record("from-acp", line)
		message, err := protocol.ParseFrame(line, true)
		if err != nil {
			codexapp.Diagnostic("ignoring malformed ACP frame: %v", err)
			continue
		}
		method, hasMethod := message["method"]
		id, hasID := message["id"]
		switch {
		case hasMethod && hasID:
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serveRequest(id, protocol.StringValue(method), paramsOf(message))
			}()
		case hasMethod:
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleACPNotification(protocol.StringValue(method), paramsOf(message))
			}()
		case hasID:
			s.deliverClientReply(id, message)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// Shutdown stops the server from another goroutine (signals, fatal errors).
func (s *Server) Shutdown() { s.shutdown() }

func (s *Server) shutdown() {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.cancel()
		// Unblock the stdin reader: Run's scanner cannot observe the closed
		// channel by itself.
		if s.inCloser != nil {
			_ = s.inCloser.Close()
		}
	})
}

func (s *Server) send(frame protocol.Message) error {
	body, err := protocol.ACPFrame(frame)
	if err != nil {
		return err
	}
	s.writeM.Lock()
	defer s.writeM.Unlock()
	if _, err := s.out.Write(body); err != nil {
		return err
	}
	if flusher, ok := s.out.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return err
		}
	}
	s.log.Record("to-acp", body[:len(body)-1])
	return nil
}

func (s *Server) notify(method string, params protocol.Message) {
	if err := s.send(protocol.Message{"method": method, "params": params}); err != nil {
		codexapp.Diagnostic("cannot send %s: %v", method, err)
	}
}

func (s *Server) emitUpdate(sessionID string, update protocol.Message) {
	if update == nil {
		return
	}
	s.notify(protocol.MethodSessionUpdate, protocol.Message{
		"sessionId": sessionID,
		"update":    update,
	})
}

// callClient performs a server -> client JSON-RPC request and waits for the
// matching reply. The reader goroutine keeps running, so unrelated frames are
// never lost or misinterpreted as the reply.
func (s *Server) callClient(ctx context.Context, method string, params protocol.Message) (protocol.Message, *protocol.RPCError) {
	id := fmt.Sprintf("cx-%d", s.clientSeq.Add(1))
	key := protocol.IDKey(id)
	reply := make(chan protocol.Message, 1)

	s.pendMu.Lock()
	s.pending[key] = reply
	s.pendMu.Unlock()
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, key)
		s.pendMu.Unlock()
	}()

	if err := s.send(protocol.Message{"id": id, "method": method, "params": params}); err != nil {
		return nil, &protocol.RPCError{Code: protocol.CodeInternalError, Message: err.Error()}
	}
	select {
	case message := <-reply:
		if raw := message["error"]; raw != nil {
			if object := protocol.AsMessage(raw); object != nil {
				return nil, &protocol.RPCError{
					Code:    int(protocol.Num(object, "code")),
					Message: protocol.Str(object, "message"),
					Data:    object["data"],
				}
			}
			return nil, &protocol.RPCError{Code: protocol.CodeInternalError, Message: protocol.StringValue(raw)}
		}
		result := protocol.Sub(message, "result")
		if result == nil {
			result = protocol.Message{}
		}
		return result, nil
	case <-ctx.Done():
		return nil, &protocol.RPCError{Code: protocol.CodeRequestCancelled, Message: ctx.Err().Error()}
	case <-s.closed:
		return nil, &protocol.RPCError{Code: protocol.CodeInternalError, Message: "ACP connection closed"}
	}
}

func (s *Server) deliverClientReply(id any, message protocol.Message) {
	key := protocol.IDKey(id)
	s.pendMu.Lock()
	reply := s.pending[key]
	delete(s.pending, key)
	s.pendMu.Unlock()
	if reply != nil {
		reply <- message
	}
}

func (s *Server) serveRequest(id any, method string, params protocol.Message) {
	result, rpcErr := s.dispatch(id, method, params)
	var frame protocol.Message
	if rpcErr != nil {
		frame = protocol.ErrorResponse(id, rpcErr)
	} else {
		frame = protocol.Result(id, result)
	}
	if err := s.send(frame); err != nil {
		codexapp.Diagnostic("cannot send %s response: %v", method, err)
	}
}

func (s *Server) dispatch(id any, method string, params protocol.Message) (protocol.Message, *protocol.RPCError) {
	switch method {
	case protocol.MethodInitialize:
		return s.handleInitialize(params)
	case protocol.MethodSessionNew:
		return s.handleSessionNew(params)
	case protocol.MethodSessionLoad:
		return s.handleSessionLoad(params)
	case protocol.MethodSessionPrompt:
		return s.handleSessionPrompt(params)
	case protocol.MethodSessionSetMode:
		return s.handleSetMode(params)
	case protocol.MethodSessionSetModel:
		return s.handleSetModel(params)
	case protocol.MethodSessionSetConfigOpt:
		return s.handleSetConfigOption(params)
	case protocol.MethodSessionCancel:
		// ACP v1 defines this as a notification, but a few hosts send it as a
		// request. Accept both shapes.
		s.cancelTurn(protocol.Str(params, "sessionId"))
		return protocol.Message{}, nil
	case protocol.MethodAuthenticate:
		return nil, rpcError(protocol.CodeMethodNotFound,
			"authenticate is not supported: Codex manages its own credentials in ~/.codex")
	default:
		return nil, rpcError(protocol.CodeMethodNotFound, "Method not supported: "+method)
	}
}

func (s *Server) handleACPNotification(method string, params protocol.Message) {
	switch method {
	case protocol.MethodSessionCancel:
		s.cancelTurn(protocol.Str(params, "sessionId"))
	default:
		codexapp.Diagnostic("ignoring ACP notification %s", method)
	}
}

func (s *Server) cancelTurn(sessionID string) {
	session := s.session(sessionID)
	if session == nil {
		return
	}
	turn, turnID := session.markCancelled()
	if turn == nil {
		return
	}
	if turnID == "" {
		codexapp.Diagnostic("cancel received before turn/start returned; will interrupt once known")
		return
	}
	if err := s.codex.TurnInterrupt(s.ctx, sessionID, turnID); err != nil {
		codexapp.Diagnostic("turn/interrupt failed: %v", err)
	}
}

// ---- session registry ----

func (s *Server) addSession(session *Session) {
	s.sessMu.Lock()
	s.sessions[session.ID] = session
	s.sessMu.Unlock()
}

func (s *Server) session(id string) *Session {
	if id == "" {
		return nil
	}
	s.sessMu.RLock()
	session := s.sessions[id]
	s.sessMu.RUnlock()
	return session
}

func paramsOf(message protocol.Message) protocol.Message {
	params := protocol.Sub(message, "params")
	if params == nil {
		return protocol.Message{}
	}
	return params
}

func rpcError(code int, format string, args ...any) *protocol.RPCError {
	return &protocol.RPCError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func internalError(err error) *protocol.RPCError {
	if err == nil {
		return nil
	}
	var rpcErr *protocol.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	if errors.Is(err, context.Canceled) {
		return rpcError(protocol.CodeRequestCancelled, "request cancelled")
	}
	return rpcError(protocol.CodeInternalError, "%v", err)
}
