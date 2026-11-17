package bridge

import (
	"sync"

	"claude-acp-bridge/internal/protocol"
)

// Session is one ACP session backed by exactly one Claude CLI process. The ACP
// sessionId is a generated UUID passed to the CLI via --session-id (new) or
// --resume (load), so it is known before the first prompt.
type Session struct {
	ID       string
	CWD      string
	Mode     string
	Model    string
	CLI      ClaudeSession
	InitInfo protocol.Message

	mu   sync.Mutex
	turn *turnState
}

// Close terminates the underlying CLI process.
func (s *Session) Close() error {
	if s.CLI == nil {
		return nil
	}
	return s.CLI.Close()
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

// markCancelled flags the active turn as cancelled and returns it.
func (s *Session) markCancelled() *turnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		return nil
	}
	s.turn.markCancelled()
	return s.turn
}

func (s *Session) setMode(mode string) {
	s.mu.Lock()
	s.Mode = mode
	s.mu.Unlock()
}

func (s *Session) modeSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Mode
}

func (s *Session) setModel(model string) {
	s.mu.Lock()
	s.Model = model
	s.mu.Unlock()
}

func (s *Session) modelSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Model
}

// turnState is the lifecycle of one ACP session/prompt. It is created BEFORE
// the user message is written, because the CLI may emit events immediately.
type turnState struct {
	translator *Translator

	mu         sync.Mutex
	cancelled  bool
	done       chan struct{}
	once       sync.Once
	stopReason string
	errText    string
}

func newTurnState() *turnState {
	return &turnState{
		translator: newTranslator(),
		done:       make(chan struct{}),
	}
}

func (t *turnState) markCancelled() {
	t.mu.Lock()
	t.cancelled = true
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
