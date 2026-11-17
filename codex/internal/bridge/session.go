package bridge

import (
	"sync"
)

// Session is one ACP session backed by exactly one Codex thread. The ACP
// sessionId IS the Codex thread id, so session/load can resume it directly.
type Session struct {
	ID       string
	ThreadID string
	CWD      string
	Mode     string
	Model    string

	mu   sync.Mutex
	turn *turnState
}

func (s *Session) approvalPolicy() string {
	switch s.Mode {
	case ModeAuto:
		return "never"
	case ModeUntrusted:
		return "untrusted"
	default:
		return "on-request"
	}
}

func (s *Session) startTurn(turn *turnState) {
	s.mu.Lock()
	s.turn = turn
	s.mu.Unlock()
}

func (s *Session) currentTurn() *turnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

func (s *Session) endTurn(turn *turnState) {
	s.mu.Lock()
	if s.turn == turn {
		s.turn = nil
	}
	s.mu.Unlock()
}

// markCancelled returns the active turn and its (possibly not yet known) id.
func (s *Session) markCancelled() (*turnState, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		return nil, ""
	}
	s.turn.markCancelled()
	return s.turn, s.turn.id
}

func (s *Session) setMode(mode string) {
	s.mu.Lock()
	s.Mode = mode
	s.mu.Unlock()
}

func (s *Session) snapshot() (mode, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Mode, s.Model
}

func (s *Session) setModel(model string) {
	s.mu.Lock()
	s.Model = model
	s.mu.Unlock()
}

// turnState is the lifecycle of one ACP session/prompt. It is created BEFORE
// turn/start is sent, because app-server may emit notifications before the RPC
// response arrives.
type turnState struct {
	mu            sync.Mutex
	id            string
	cancelled     bool
	interruptOnID bool
	translator    *Translator
	done          chan struct{}
	once          sync.Once
	stopReason    string
	errText       string
}

func newTurnState() *turnState {
	return &turnState{
		translator: newTranslator(),
		done:       make(chan struct{}),
	}
}

// setID records the Codex turn id. The bool reports whether an interrupt was
// requested before the id was known (session/cancel raced turn/start).
func (t *turnState) setID(id string) bool {
	if id == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id != "" && t.id != id {
		return false
	}
	t.id = id
	interrupt := t.interruptOnID
	t.interruptOnID = false
	return interrupt
}

func (t *turnState) turnID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

func (t *turnState) markCancelled() {
	t.mu.Lock()
	t.cancelled = true
	if t.id == "" {
		t.interruptOnID = true
	}
	t.mu.Unlock()
}

func (t *turnState) isCancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelled
}

func (t *turnState) finish(stopReason, errText string) {
	t.once.Do(func() {
		t.mu.Lock()
		t.stopReason, t.errText = stopReason, errText
		t.mu.Unlock()
		close(t.done)
	})
}

func (t *turnState) outcome() (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopReason, t.errText
}
