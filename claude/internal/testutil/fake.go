// Package testutil provides a deterministic, in-memory stand-in for the
// Claude CLI stream-json process and a small ACP client harness. No real CLI,
// model call or network is involved in the unit tests.
package testutil

import (
	"context"
	"sync"
	"time"

	"claude-acp-bridge/internal/claudecode"
	"claude-acp-bridge/internal/protocol"
)

// FakeEvent is a stream-json event emitted by the fake after SendUserMessage.
type FakeEvent struct {
	Message protocol.Message
	Delay   time.Duration
}

// FakeSession implements bridge.ClaudeSession structurally (no import cycle).
type FakeSession struct {
	mu        sync.Mutex
	onEvent   func(protocol.Message)
	onControl func(string, protocol.Message)
	script    []FakeEvent
	prompts   []protocol.Message
	answers   []protocol.Message
	done      chan struct{}
	closeOne  sync.Once

	// Recorded control calls.
	interrupts []bool
	models     []string
	modes      []string
}

func NewFakeSession() *FakeSession {
	return &FakeSession{done: make(chan struct{})}
}

func (f *FakeSession) SetHandlers(
	onEvent func(protocol.Message),
	onControl func(requestID string, request protocol.Message),
) {
	f.mu.Lock()
	f.onEvent, f.onControl = onEvent, onControl
	f.mu.Unlock()
}

// SetScript installs events emitted (in order) after the next user message.
func (f *FakeSession) SetScript(events ...FakeEvent) {
	f.mu.Lock()
	f.script = append([]FakeEvent(nil), events...)
	f.mu.Unlock()
}

func (f *FakeSession) SendUserMessage(content []any) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, protocol.Message{"type": "user", "content": content})
	script := append([]FakeEvent(nil), f.script...)
	f.mu.Unlock()
	go func() {
		for _, event := range script {
			if event.Delay > 0 {
				time.Sleep(event.Delay)
			}
			f.Emit(event.Message)
		}
	}()
	return nil
}

func (f *FakeSession) RespondControl(requestID string, response protocol.Message) error {
	f.mu.Lock()
	f.answers = append(f.answers, response)
	f.mu.Unlock()
	return nil
}

func (f *FakeSession) Interrupt(context.Context) error {
	f.mu.Lock()
	f.interrupts = append(f.interrupts, true)
	script := append([]FakeEvent(nil), f.script...)
	f.mu.Unlock()
	// Simulate the CLI ending the turn after an interrupt.
	go func() {
		for _, event := range script {
			f.Emit(event.Message)
		}
	}()
	return nil
}

func (f *FakeSession) SetModel(_ context.Context, model string) error {
	f.mu.Lock()
	f.models = append(f.models, model)
	f.mu.Unlock()
	return nil
}

func (f *FakeSession) SetMode(_ context.Context, mode string) error {
	f.mu.Lock()
	f.modes = append(f.modes, mode)
	f.mu.Unlock()
	return nil
}

func (f *FakeSession) InitInfo() protocol.Message {
	return protocol.Message{
		"type": "system", "subtype": "init", "session_id": "fake-init",
		"model": "fake-model", "cwd": "/tmp",
	}
}

func (f *FakeSession) Done() <-chan struct{} { return f.done }
func (f *FakeSession) Close() error {
	f.closeOne.Do(func() { close(f.done) })
	return nil
}

// Emit injects a stream-json event into the bridge.
func (f *FakeSession) Emit(message protocol.Message) {
	f.mu.Lock()
	handler := f.onEvent
	f.mu.Unlock()
	if handler != nil {
		handler(message)
	}
}

// Ask injects a CLI control request into the bridge.
func (f *FakeSession) Ask(requestID string, request protocol.Message) {
	f.mu.Lock()
	handler := f.onControl
	f.mu.Unlock()
	if handler != nil {
		handler(requestID, request)
	}
}

func (f *FakeSession) Prompts() []protocol.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Message(nil), f.prompts...)
}

func (f *FakeSession) Answers() []protocol.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Message(nil), f.answers...)
}

func (f *FakeSession) InterruptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.interrupts)
}

func (f *FakeSession) Models() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.models...)
}

func (f *FakeSession) Modes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.modes...)
}

// FakeClaude implements bridge.Claude: it opens one FakeSession per ACP session.
type FakeClaude struct {
	mu       sync.Mutex
	sessions map[string]*FakeSession
	opened   []protocol.Message
	failOpen error
}

func NewFakeClaude() *FakeClaude {
	return &FakeClaude{sessions: map[string]*FakeSession{}}
}

func (f *FakeClaude) Open(_ context.Context, cfg claudecode.Config) (claudecode.Session, error) {
	if f.failOpen != nil {
		return nil, f.failOpen
	}
	session := NewFakeSession()
	id := cfg.SessionID
	if id == "" {
		id = cfg.ResumeID
	}
	record := protocol.Message{
		"sessionId":      id,
		"resumeId":       cfg.ResumeID,
		"cwd":            cfg.Dir,
		"model":          cfg.Model,
		"permissionMode": cfg.PermissionMode,
		"mcpConfig":      cfg.MCPConfigPath,
	}
	f.mu.Lock()
	f.sessions[id] = session
	f.opened = append(f.opened, record)
	f.mu.Unlock()
	return session, nil
}

// Session returns the fake session opened for a session id.
func (f *FakeClaude) Session(id string) *FakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id]
}

func (f *FakeClaude) Opened() []protocol.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Message(nil), f.opened...)
}

func (f *FakeClaude) FailOpen(err error) {
	f.mu.Lock()
	f.failOpen = err
	f.mu.Unlock()
}
