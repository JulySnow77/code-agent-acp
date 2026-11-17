// Package bridge translates ACP v1 (client side) to the Claude Code
// stream-json protocol (agent side). ACP hosts see a normal ACP agent, while
// the real agent is a `claude -p --input-format stream-json` child process.
//
// Unlike Codex's app-server (one process, many threads), Claude binds one CLI
// process to one conversation, so each ACP session owns exactly one child.
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

	"claude-acp-bridge/internal/claudecode"
	"claude-acp-bridge/internal/protocol"
)

// ClaudeSession and Claude are aliases for the claudecode interfaces. Keeping
// them here lets tests drive the whole ACP surface with a deterministic fake
// instead of a real CLI process, without an import cycle.
type ClaudeSession = claudecode.Session
type Claude = claudecode.Factory

// Config holds bridge behaviour, overridable from the CLI / env.
type Config struct {
	// Bin is the Claude CLI executable.
	Bin string
	// ExtraArgs are appended to every CLI launch.
	ExtraArgs []string
	// Model is the default model id.
	Model string
	// PermissionMode is the CLI mode applied at launch.
	PermissionMode string
	// CWD is the fallback working directory for session/new.
	CWD string
	// TurnTimeout bounds one ACP prompt.
	TurnTimeout time.Duration
	// PermissionFallback decides the answer when the ACP client cannot handle
	// session/request_permission: "decline" (default) or "accept".
	PermissionFallback string
	// Log optionally records raw ACP frames.
	Log *protocol.WireLog
}

// Mode names exposed over ACP (consistent with zcode/codex bridges). They map
// onto Claude permission modes for session/set_mode.
const (
	ModeAsk       = "ask"
	ModeAuto      = "auto"
	ModeUntrusted = "untrusted"
)

var availableModes = []protocol.Message{
	{"id": ModeAsk, "name": "Ask", "description": "Ask before running tools (permission-mode: manual)"},
	{"id": ModeAuto, "name": "Auto", "description": "Accept file edits automatically (permission-mode: acceptEdits)"},
	{"id": ModeUntrusted, "name": "Untrusted", "description": "Restrictive planning mode (permission-mode: plan)"},
}

// Server is an ACP v1 stdio server backed by one Claude CLI process per session.
//
// A single reader goroutine owns stdin. Requests and notifications are handled
// on their own goroutines, and replies to server-initiated calls (permissions)
// are routed back by JSON-RPC id. This is what makes a nested request possible:
// Claude asks us for approval -> we ask the ACP client -> the reply arrives on
// the same stdin while the prompt handler is still running.
type Server struct {
	in       io.Reader
	inCloser io.Closer
	out      io.Writer
	writeM   sync.Mutex
	claude   Claude
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

// New builds a server. Run closes all sessions when it returns.
func New(claude Claude, in io.Reader, out io.Writer, cfg Config) *Server {
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = 30 * time.Minute
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "manual"
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
		claude:   claude,
		cfg:      cfg,
		log:      cfg.Log,
		pending:  make(map[string]chan protocol.Message),
		sessions: make(map[string]*Session),
		closed:   make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	return s
}

// Run serves ACP frames until stdin closes or the process is interrupted. A
// clean EOF is not an error.
func (s *Server) Run() error {
	defer func() {
		s.shutdown()
		s.wg.Wait()
		s.closeAllSessions()
	}()

	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 64*1024), claudecode.MaxFrameSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		s.log.Record("from-acp", line)
		message, err := protocol.ParseFrame(line, true)
		if err != nil {
			claudecode.Diagnostic("ignoring malformed ACP frame: %v", err)
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
		claudecode.Diagnostic("cannot send %s: %v", method, err)
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
	id := fmt.Sprintf("cl-%d", s.clientSeq.Add(1))
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
	result, rpcErr := s.dispatch(method, params)
	var frame protocol.Message
	if rpcErr != nil {
		frame = protocol.ErrorResponse(id, rpcErr)
	} else {
		frame = protocol.Result(id, result)
	}
	if err := s.send(frame); err != nil {
		claudecode.Diagnostic("cannot send %s response: %v", method, err)
	}
}

func (s *Server) dispatch(method string, params protocol.Message) (protocol.Message, *protocol.RPCError) {
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
			"authenticate is not supported: Claude Code manages its own credentials")
	default:
		return nil, rpcError(protocol.CodeMethodNotFound, "Method not supported: "+method)
	}
}

func (s *Server) handleACPNotification(method string, params protocol.Message) {
	switch method {
	case protocol.MethodSessionCancel:
		s.cancelTurn(protocol.Str(params, "sessionId"))
	default:
		claudecode.Diagnostic("ignoring ACP notification %s", method)
	}
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

func (s *Server) closeAllSessions() {
	s.sessMu.RLock()
	all := make([]*Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		all = append(all, session)
	}
	s.sessMu.RUnlock()
	for _, session := range all {
		_ = session.Close()
	}
}
