// Package backend implements the two backends behind the stdio ACP server.
// Local talks to the WorkBuddy PC client's loopback gateway; Fake is
// deterministic and never touches a real conversation.
package backend

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy-acp-bridge/internal/discovery"
	"workbuddy-acp-bridge/internal/gateway"
	"workbuddy-acp-bridge/internal/protocol"
)

// Error is an ACP-level backend failure with a JSON-RPC code and optional data.
type Error struct {
	Text string
	Code int
	Data any
}

func (e *Error) Error() string { return e.Text }

// EmitFunc streams one `session/update` payload to the upstream ACP client.
type EmitFunc func(protocol.Message)

// RequestClientFunc issues a server-to-client request and waits for its reply.
type RequestClientFunc func(method string, params protocol.Message) (protocol.Message, error)

// Backend is the contract used by the stdio ACP server.
type Backend interface {
	Name() string
	Endpoint() string
	SessionID() string
	SetMode(string)
	SetPermissionMode(string)
	Capabilities() (protocol.Message, error)
	Meta() protocol.Message
	NewSession(cwd string, mcpServers []any, emit EmitFunc) (string, error)
	LoadSession(sessionID, cwd string, mcpServers []any, emit EmitFunc) (string, error)
	SetSessionMode(modeID string, emit EmitFunc) (protocol.Message, error)
	SetConfigOption(configID, value string, emit EmitFunc) (protocol.Message, error)
	Prompt(blocks []any, emit EmitFunc, requestClient RequestClientFunc) (string, error)
	Cancel()
}

// Log writes diagnostics to stderr only. Protocol frames go to stdout.
func Log(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[workbuddy-acp-bridge] "+format+"\n", args...)
}

func asMessage(value any) protocol.Message {
	if message, ok := value.(map[string]any); ok {
		return protocol.Message(message)
	}
	return protocol.Message{}
}

func gatewayFailure(err error) *Error {
	if typed, ok := err.(*gateway.Error); ok {
		return &Error{
			Text: fmt.Sprintf("%s (%s)", typed.Error(), typed.Category),
			Code: -32001,
			Data: typed.Data,
		}
	}
	return &Error{Text: err.Error(), Code: -32001}
}

// Local is the real WorkBuddy loopback backend.
type Local struct {
	mu                sync.Mutex
	endpoint          string
	discoveredSession string
	cwd               string
	timeout           time.Duration
	client            *gateway.Client
	capabilitiesCache protocol.Message
	sessionID         string
	mode              string
	model             string
	permissionMode    string
	cancelled         atomic.Bool
}

// LocalOptions configures the local backend.
type LocalOptions struct {
	Endpoint  string
	SessionID string
	Cwd       string
	Timeout   time.Duration
}

// NewLocal selects a live session and prepares the backend.
func NewLocal(options LocalOptions) (*Local, error) {
	target, err := discovery.Select(discovery.Options{
		SessionID: options.SessionID,
		Cwd:       options.Cwd,
		Endpoint:  options.Endpoint,
		Directory: discovery.DefaultSessionsDir(),
	})
	if err != nil {
		return nil, &Error{Text: err.Error(), Code: -32001}
	}
	cwd := options.Cwd
	if cwd == "" {
		cwd = target.Cwd
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	return &Local{
		endpoint:          target.Endpoint,
		discoveredSession: target.SessionID,
		cwd:               cwd,
		timeout:           options.Timeout,
		mode:              "build",
		permissionMode:    "deny",
	}, nil
}

func (b *Local) Name() string     { return "local" }
func (b *Local) Endpoint() string { return b.endpoint }
func (b *Local) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}
func (b *Local) setSessionID(value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessionID = value
}
func (b *Local) SetMode(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mode = mode
}
func (b *Local) SetPermissionMode(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.permissionMode = mode
}

func (b *Local) ensure() (*gateway.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		client, err := gateway.New(b.endpoint, b.timeout)
		if err != nil {
			return nil, gatewayFailure(err)
		}
		capabilities, err := client.Initialize()
		if err != nil {
			return nil, gatewayFailure(err)
		}
		b.client, b.capabilitiesCache = client, capabilities
	}
	return b.client, nil
}

func (b *Local) Capabilities() (protocol.Message, error) {
	_, err := b.ensure()
	if err != nil {
		return nil, err
	}
	result := protocol.Message{
		"loadSession":        true,
		"promptCapabilities": protocol.Message{"embeddedContext": true},
	}
	b.mu.Lock()
	cached := b.capabilitiesCache
	b.mu.Unlock()
	if upstream, ok := cached["agentCapabilities"].(map[string]any); ok {
		for key, value := range upstream {
			if key != "loadSession" {
				result[key] = value
			}
		}
	}
	return result, nil
}

func (b *Local) Meta() protocol.Message {
	b.mu.Lock()
	mode := b.permissionMode
	b.mu.Unlock()
	return protocol.Message{"workbuddy": protocol.Message{
		"backend":                b.Name(),
		"endpoint":               b.endpoint,
		"sessionBinding":         "WorkBuddy persistent session; session/new attaches an external writer",
		"modelSelectionVerified": false,
		"permissionMode":         mode,
	}}
}

func (b *Local) forward(message protocol.Message, emit EmitFunc) {
	if message["method"] != "session/update" || emit == nil {
		return
	}
	if update, ok := asMessage(message["params"])["update"].(map[string]any); ok {
		emit(protocol.Message(update))
	}
}

func (b *Local) permissionReply(message protocol.Message, requestClient RequestClientFunc) {
	identifier, present := message["id"]
	if !present {
		return
	}
	b.mu.Lock()
	mode, permissionMode := b.mode, b.permissionMode
	b.mu.Unlock()
	outcome := any(protocol.Message{"outcome": "cancelled"})
	if permissionMode == "client" && mode != "plan" && requestClient != nil {
		method, _ := message["method"].(string)
		answer, err := requestClient(method, asMessage(message["params"]))
		if err != nil {
			Log("permission callback failed: %v", err)
		} else if selected, ok := answer["outcome"]; ok && selected != nil {
			outcome = selected
		}
	}
	client, err := b.ensure()
	if err != nil {
		Log("could not answer gateway permission request: %v", err)
		return
	}
	if err := client.Respond(identifier, protocol.Message{"outcome": outcome}); err != nil {
		Log("could not answer gateway permission request: %v", err)
	}
}

func (b *Local) NewSession(cwd string, mcpServers []any, emit EmitFunc) (string, error) {
	client, err := b.ensure()
	if err != nil {
		return "", err
	}
	if cwd == "" {
		cwd = b.cwd
	}
	if mcpServers == nil {
		mcpServers = []any{}
	}
	result, err := client.Call("session/new", protocol.Message{
		"cwd": cwd, "mcpServers": mcpServers,
	}, func(message protocol.Message) { b.forward(message, emit) }, nil)
	if err != nil {
		return "", gatewayFailure(err)
	}
	sessionID, _ := result["sessionId"].(string)
	if sessionID == "" {
		sessionID = b.discoveredSession
	}
	if sessionID == "" {
		return "", &Error{Text: "session/new returned no sessionId", Code: -32002}
	}
	b.setSessionID(sessionID)
	return sessionID, nil
}

func (b *Local) LoadSession(sessionID, cwd string, mcpServers []any, emit EmitFunc) (string, error) {
	client, err := b.ensure()
	if err != nil {
		return "", err
	}
	if cwd == "" {
		cwd = b.cwd
	}
	if mcpServers == nil {
		mcpServers = []any{}
	}
	_, err = client.Call("session/load", protocol.Message{
		"sessionId": sessionID, "cwd": cwd, "mcpServers": mcpServers,
	}, func(message protocol.Message) { b.forward(message, emit) }, nil)
	if err != nil {
		return "", gatewayFailure(err)
	}
	b.setSessionID(sessionID)
	return sessionID, nil
}

func (b *Local) SetSessionMode(modeID string, emit EmitFunc) (protocol.Message, error) {
	b.SetMode(modeID)
	Log("local ACP mode=%s; plan mode denies write/execute permission requests", modeID)
	return protocol.Message{}, nil
}

func (b *Local) SetConfigOption(configID, value string, emit EmitFunc) (protocol.Message, error) {
	b.mu.Lock()
	b.model = value
	b.mu.Unlock()
	if configID == "model" {
		Log("model requested=%q; WorkBuddy owns the session model, selection is not independently verified", value)
	}
	return protocol.Message{"configOptions": []any{
		protocol.Message{"id": configID, "currentValue": value},
	}}, nil
}

func (b *Local) Prompt(blocks []any, emit EmitFunc, requestClient RequestClientFunc) (string, error) {
	b.cancelled.Store(false)
	client, err := b.ensure()
	if err != nil {
		return "", err
	}
	result, err := client.Call("session/prompt", protocol.Message{
		"sessionId": b.SessionID(), "prompt": blocks,
	}, func(message protocol.Message) { b.forward(message, emit) },
		func(message protocol.Message) { b.permissionReply(message, requestClient) })
	if err != nil {
		return "", gatewayFailure(err)
	}
	if b.cancelled.Load() {
		return "cancelled", nil
	}
	stopReason, _ := result["stopReason"].(string)
	if stopReason == "" {
		stopReason = "end_turn"
	}
	return stopReason, nil
}

func (b *Local) Cancel() {
	b.cancelled.Store(true)
	b.mu.Lock()
	client, sessionID := b.client, b.sessionID
	b.mu.Unlock()

	if client != nil && sessionID != "" {
		if _, err := client.Call("session/cancel", protocol.Message{"sessionId": sessionID}, nil, nil); err != nil {
			Log("cancel request failed: %v", err)
		}
	}
}

// Fake is the deterministic ACP backend used by tests and `--backend fake`.
type Fake struct {
	mu             sync.Mutex
	result         any
	events         []string
	sessionID      string
	cwd            string
	mode           string
	model          string
	permissionMode string
	calls          []string
	cancelled      bool
}

// NewFake builds a fake backend. A nil result is read from
// WORKBUDDY_FAKE_RESULT and defaults to {"value":"works"}.
func NewFake(result any, events []string) (*Fake, error) {
	if result == nil {
		raw := os.Getenv("WORKBUDDY_FAKE_RESULT")
		if raw == "" {
			raw = `{"value":"works"}`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, &Error{Text: "invalid WORKBUDDY_FAKE_RESULT: " + err.Error(), Code: -32001}
		}
	}
	if events == nil {
		events = []string{"agent_message_chunk"}
	}
	cwd, _ := os.Getwd()
	permissionMode := os.Getenv("WORKBUDDY_FAKE_PERMISSION_MODE")
	if permissionMode == "" {
		permissionMode = "deny"
	}
	return &Fake{
		result: result, events: events, cwd: cwd, mode: "build",
		permissionMode: permissionMode,
	}, nil
}

func (b *Fake) Name() string     { return "fake" }
func (b *Fake) Endpoint() string { return "" }
func (b *Fake) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

func (b *Fake) SetMode(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mode = mode
}
func (b *Fake) SetPermissionMode(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.permissionMode = mode
}
func (b *Fake) Calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}
func (b *Fake) hasEvent(name string) bool {
	for _, event := range b.events {
		if event == name {
			return true
		}
	}
	return false
}
func (b *Fake) record(call string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
}

func (b *Fake) Capabilities() (protocol.Message, error) {
	return protocol.Message{
		"loadSession":        true,
		"promptCapabilities": protocol.Message{"embeddedContext": true},
	}, nil
}

func (b *Fake) Meta() protocol.Message {
	b.mu.Lock()
	mode := b.permissionMode
	b.mu.Unlock()
	return protocol.Message{"workbuddy": protocol.Message{
		"backend": b.Name(), "modelSelectionVerified": false, "permissionMode": mode,
	}}
}

func (b *Fake) NewSession(cwd string, mcpServers []any, emit EmitFunc) (string, error) {
	b.mu.Lock()
	if cwd != "" {
		b.cwd = cwd
	}
	b.sessionID = fmt.Sprintf("fake-%d", time.Now().UnixNano())
	sessionID := b.sessionID
	b.calls = append(b.calls, "session/new")
	b.mu.Unlock()
	if b.hasEvent("config_option_update") && emit != nil {
		emit(protocol.Message{"sessionUpdate": "config_option_update",
			"configOptions": []any{protocol.Message{"id": "model", "currentValue": "fake/model"}}})
	}
	return sessionID, nil
}

func (b *Fake) LoadSession(sessionID, cwd string, mcpServers []any, emit EmitFunc) (string, error) {
	b.mu.Lock()
	b.sessionID = sessionID
	if cwd != "" {
		b.cwd = cwd
	}
	b.calls = append(b.calls, "session/load")
	b.mu.Unlock()
	return sessionID, nil
}

func (b *Fake) SetSessionMode(modeID string, emit EmitFunc) (protocol.Message, error) {
	b.SetMode(modeID)
	b.record("session/set_mode:" + modeID)
	return protocol.Message{}, nil
}

func (b *Fake) SetConfigOption(configID, value string, emit EmitFunc) (protocol.Message, error) {
	b.mu.Lock()
	b.model = value
	b.mu.Unlock()
	b.record("session/set_config_option:" + configID)
	return protocol.Message{"configOptions": []any{
		protocol.Message{"id": configID, "currentValue": value},
	}}, nil
}

func (b *Fake) Prompt(blocks []any, emit EmitFunc, requestClient RequestClientFunc) (string, error) {
	b.record("session/prompt")
	if b.hasEvent("permission") && requestClient != nil {
		_, err := requestClient("session/request_permission", protocol.Message{
			"sessionId": b.SessionID(),
			"toolCall":  protocol.Message{"toolCallId": "tool-1", "kind": "read"},
			"options":   []any{protocol.Message{"optionId": "allow", "kind": "allow_once"}},
		})
		if err != nil {
			return "", &Error{Text: "permission callback failed: " + err.Error(), Code: -32003}
		}
	}
	if b.hasEvent("progress") && emit != nil {
		emit(protocol.Message{"sessionUpdate": "agent_message_chunk",
			"content": protocol.Message{"type": "text", "text": "Reading the fixed skill.\n"}})
	}
	encoded, err := protocol.Encode(b.result)
	if err != nil {
		return "", &Error{Text: err.Error(), Code: -32000}
	}
	if emit != nil {
		emit(protocol.Message{"sessionUpdate": "agent_message_chunk",
			"content": protocol.Message{"type": "text", "text": string(encoded)}})
	}
	b.mu.Lock()
	cancelled := b.cancelled
	b.mu.Unlock()
	if cancelled {
		return "cancelled", nil
	}
	return "end_turn", nil
}

func (b *Fake) Cancel() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = true
	b.calls = append(b.calls, "session/cancel")
}

// ParseMCPServers converts an ACP `mcpServers` parameter to an array,
// rejecting non-array values instead of silently dropping them.
func ParseMCPServers(value any) ([]any, error) {
	if value == nil {
		return []any{}, nil
	}
	if servers, ok := value.([]any); ok {
		return servers, nil
	}
	return nil, &Error{Text: "mcpServers must be an array", Code: -32602}
}
