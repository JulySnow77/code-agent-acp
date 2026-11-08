package acp

import "encoding/json"

// Documentation for these types: https://agentclientprotocol.com/protocol/v1/schema
// The structs mirror schema/v1 (downloaded 2026-11-08). Map-typed capability
// objects keep unknown / vendor fields (Kimi advertises `fork`, for example)
// instead of silently discarding them.

// CapabilityObject is an open capability object such as sessionCapabilities.list.
// A non-nil value, even `{}`, advertises support.
type CapabilityObject map[string]json.RawMessage

func (c CapabilityObject) Has(name string) bool {
	_, ok := c[name]
	return ok
}

func (c CapabilityObject) Set(name string) {
	if c != nil {
		c[name] = json.RawMessage(`{}`)
	}
}

type Implementation struct {
	Name    string         `json:"name"`
	Title   string         `json:"title,omitempty"`
	Version string         `json:"version"`
	Meta    map[string]any `json:"_meta,omitempty"`
}

// ---------- capabilities ----------

type FileSystemCapabilities struct {
	ReadTextFile  bool           `json:"readTextFile,omitempty"`
	WriteTextFile bool           `json:"writeTextFile,omitempty"`
	Meta          map[string]any `json:"_meta,omitempty"`
}

type AuthCapabilities struct {
	Terminal bool           `json:"terminal,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// ClientSessionCapabilities covers compaction/configOptions/notices and future
// additions without breaking on new keys.
type ClientSessionCapabilities map[string]json.RawMessage

type ClientCapabilities struct {
	FS          *FileSystemCapabilities    `json:"fs,omitempty"`
	Terminal    bool                       `json:"terminal,omitempty"`
	Auth        *AuthCapabilities          `json:"auth,omitempty"`
	Session     ClientSessionCapabilities  `json:"session,omitempty"`
	Elicitation map[string]json.RawMessage `json:"elicitation,omitempty"`
	Meta        map[string]any             `json:"_meta,omitempty"`
}

type PromptCapabilities struct {
	Image           bool           `json:"image,omitempty"`
	Audio           bool           `json:"audio,omitempty"`
	EmbeddedContext bool           `json:"embeddedContext,omitempty"`
	Meta            map[string]any `json:"_meta,omitempty"`
}

type MCPCapabilities struct {
	HTTP bool           `json:"http,omitempty"`
	SSE  bool           `json:"sse,omitempty"`
	Meta map[string]any `json:"_meta,omitempty"`
}

// SessionCapabilities / AgentAuthCapabilities are open objects: presence of a
// key such as "resume", "fork" or "logout" advertises that method.
type SessionCapabilities map[string]json.RawMessage
type AgentAuthCapabilities map[string]json.RawMessage

func (s SessionCapabilities) Has(name string) bool {
	_, ok := s[name]
	return ok
}

func (s SessionCapabilities) Set(names ...string) {
	for _, n := range names {
		s[n] = json.RawMessage(`{}`)
	}
}

type AgentCapabilities struct {
	LoadSession         bool                  `json:"loadSession,omitempty"`
	PromptCapabilities  *PromptCapabilities   `json:"promptCapabilities,omitempty"`
	MCPCapabilities     *MCPCapabilities      `json:"mcpCapabilities,omitempty"`
	SessionCapabilities SessionCapabilities   `json:"sessionCapabilities,omitempty"`
	Auth                AgentAuthCapabilities `json:"auth,omitempty"`
	Meta                map[string]any        `json:"_meta,omitempty"`
}

func (c AgentCapabilities) CanLoadSession() bool { return c.LoadSession }
func (c AgentCapabilities) CanResume() bool      { return c.SessionCapabilities.Has("resume") }
func (c AgentCapabilities) CanList() bool        { return c.SessionCapabilities.Has("list") }
func (c AgentCapabilities) CanClose() bool       { return c.SessionCapabilities.Has("close") }
func (c AgentCapabilities) CanDelete() bool      { return c.SessionCapabilities.Has("delete") }
func (c AgentCapabilities) CanFork() bool        { return c.SessionCapabilities.Has("fork") }
func (c AgentCapabilities) CanLogout() bool {
	_, ok := c.Auth["logout"]
	return ok
}

type AuthMethod struct {
	ID          string   `json:"id"`
	Type        string   `json:"type,omitempty"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Args        []string `json:"args,omitempty"`
	// Env is open on purpose: schema/v1 declares EnvVariable[], but Kimi Code
	// 2.1.1 sends an object (`{}`). Keeping raw bytes lets both shapes through.
	Env  json.RawMessage `json:"env,omitempty"`
	Meta map[string]any  `json:"_meta,omitempty"`
}

// ---------- MCP servers ----------

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer is a tagged union. Type is "" (stdio), "http" or "sse".
// Custom marshalling guarantees stdio sends required args/env arrays and does
// not send `type`, while http/sse send type/url/headers.
type MCPServer struct {
	Name    string
	Type    string
	Command string
	Args    []string
	Env     []EnvVariable
	URL     string
	Headers []HTTPHeader
	Meta    map[string]any
}

func (s MCPServer) MarshalJSON() ([]byte, error) {
	m := map[string]any{"name": s.Name}
	if s.Meta != nil {
		m["_meta"] = s.Meta
	}
	switch s.Type {
	case "", "stdio":
		m["command"] = s.Command
		args := s.Args
		if args == nil {
			args = []string{}
		}
		env := s.Env
		if env == nil {
			env = []EnvVariable{}
		}
		m["args"], m["env"] = args, env
	case "http", "sse":
		headers := s.Headers
		if headers == nil {
			headers = []HTTPHeader{}
		}
		m["type"], m["url"], m["headers"] = s.Type, s.URL, headers
	default:
		m["type"] = s.Type
		if s.URL != "" {
			m["url"] = s.URL
		}
	}
	return json.Marshal(m)
}

func (s *MCPServer) UnmarshalJSON(data []byte) error {
	var aux struct {
		Name    string         `json:"name"`
		Type    string         `json:"type"`
		Command string         `json:"command"`
		Args    []string       `json:"args"`
		Env     []EnvVariable  `json:"env"`
		URL     string         `json:"url"`
		Headers []HTTPHeader   `json:"headers"`
		Meta    map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*s = MCPServer{
		Name: aux.Name, Type: aux.Type, Command: aux.Command, Args: aux.Args,
		Env: aux.Env, URL: aux.URL, Headers: aux.Headers, Meta: aux.Meta,
	}
	return nil
}

// SystemPrompt is open (append / override variants).
type SystemPrompt map[string]any

// ---------- initialization ----------

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
	Meta               map[string]any     `json:"_meta,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities,omitempty"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	Meta              map[string]any    `json:"_meta,omitempty"`
}

type AuthenticateParams struct {
	MethodID string         `json:"methodId"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

type EmptyResult struct{}

// ---------- sessions ----------

type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// SessionConfigOption is an open union (select / boolean).
type SessionConfigOption map[string]any

type SessionInfo struct {
	SessionID             string   `json:"sessionId"`
	CWD                   string   `json:"cwd"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	Title                 string   `json:"title,omitempty"`
	UpdatedAt             string   `json:"updatedAt,omitempty"`
}

type NewSessionParams struct {
	CWD                   string         `json:"cwd"`
	MCPServers            []MCPServer    `json:"mcpServers"`
	AdditionalDirectories []string       `json:"additionalDirectories,omitempty"`
	SystemPrompt          SystemPrompt   `json:"systemPrompt,omitempty"`
	Meta                  map[string]any `json:"_meta,omitempty"`
}

type SessionStateResult struct {
	Modes         *SessionModeState     `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
	Meta          map[string]any        `json:"_meta,omitempty"`
}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
	SessionStateResult
}

// Embedding SessionStateResult flattens its fields into the same object.
func (r NewSessionResult) MarshalJSON() ([]byte, error) {
	m := map[string]any{"sessionId": r.SessionID}
	if r.Modes != nil {
		m["modes"] = r.Modes
	}
	if len(r.ConfigOptions) > 0 {
		m["configOptions"] = r.ConfigOptions
	}
	if r.Meta != nil {
		m["_meta"] = r.Meta
	}
	return json.Marshal(m)
}

func (r *NewSessionResult) UnmarshalJSON(data []byte) error {
	var aux struct {
		SessionID     string                `json:"sessionId"`
		Modes         *SessionModeState     `json:"modes"`
		ConfigOptions []SessionConfigOption `json:"configOptions"`
		Meta          map[string]any        `json:"_meta"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = NewSessionResult{SessionID: aux.SessionID, SessionStateResult: SessionStateResult{
		Modes: aux.Modes, ConfigOptions: aux.ConfigOptions, Meta: aux.Meta,
	}}
	return nil
}

type LoadSessionParams struct {
	SessionID             string         `json:"sessionId"`
	CWD                   string         `json:"cwd"`
	MCPServers            []MCPServer    `json:"mcpServers"`
	AdditionalDirectories []string       `json:"additionalDirectories,omitempty"`
	SystemPrompt          SystemPrompt   `json:"systemPrompt,omitempty"`
	Meta                  map[string]any `json:"_meta,omitempty"`
}

type ResumeSessionParams struct {
	SessionID             string         `json:"sessionId"`
	CWD                   string         `json:"cwd"`
	MCPServers            []MCPServer    `json:"mcpServers,omitempty"`
	AdditionalDirectories []string       `json:"additionalDirectories,omitempty"`
	SystemPrompt          SystemPrompt   `json:"systemPrompt,omitempty"`
	Meta                  map[string]any `json:"_meta,omitempty"`
}

// ForkSessionParams is Kimi's session/fork extension; schema/v1 advertises the
// capability but does not define a request body, so fields stay open.
type ForkSessionParams map[string]any

type ListSessionsParams struct {
	CWD    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type ListSessionsResult struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type CloseSessionParams struct {
	SessionID string `json:"sessionId"`
}
type DeleteSessionParams struct {
	SessionID string `json:"sessionId"`
}

type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

type PromptResult struct {
	StopReason string         `json:"stopReason"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

type CancelNotificationParams struct {
	SessionID string         `json:"sessionId"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

type SetSessionModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type SetSessionConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type SessionNotification struct {
	SessionID string         `json:"sessionId"`
	Update    SessionUpdate  `json:"update"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

// ---------- plans, tool calls, permissions ----------

type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

type AvailableCommand struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Input       map[string]any `json:"input,omitempty"`
}

type ToolCallLocation struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

// ToolCallContent is an open union: content / diff / terminal.
type ToolCallContent map[string]any

func ToolCallText(text string) ToolCallContent {
	return ToolCallContent{"type": "content", "content": TextBlock(text)}
}

func ToolCallDiff(path, oldText, newText string) ToolCallContent {
	m := ToolCallContent{"type": "diff", "path": path, "newText": newText}
	if oldText != "" {
		m["oldText"] = oldText
	}
	return m
}

func ToolCallTerminal(terminalID string) ToolCallContent {
	return ToolCallContent{"type": "terminal", "terminalId": terminalID}
}

type ToolCallReport struct {
	ToolCallID string             `json:"toolCallId"`
	Title      string             `json:"title"`
	Name       string             `json:"name,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Status     string             `json:"status,omitempty"`
	Content    []ToolCallContent  `json:"content,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	RawInput   any                `json:"rawInput,omitempty"`
	RawOutput  any                `json:"rawOutput,omitempty"`
	Meta       map[string]any     `json:"_meta,omitempty"`
}

type ToolCallUpdateReport struct {
	ToolCallID string             `json:"toolCallId"`
	Title      *string            `json:"title,omitempty"`
	Name       *string            `json:"name,omitempty"`
	Kind       *string            `json:"kind,omitempty"`
	Status     *string            `json:"status,omitempty"`
	Content    []ToolCallContent  `json:"content,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	RawInput   any                `json:"rawInput,omitempty"`
	RawOutput  any                `json:"rawOutput,omitempty"`
	Meta       map[string]any     `json:"_meta,omitempty"`
}

func StringPtr(s string) *string { return &s }

type PermissionOption struct {
	OptionID string         `json:"optionId"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

type RequestPermissionParams struct {
	SessionID string               `json:"sessionId"`
	ToolCall  ToolCallUpdateReport `json:"toolCall"`
	Options   []PermissionOption   `json:"options"`
	Meta      map[string]any       `json:"_meta,omitempty"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
	Meta    map[string]any    `json:"_meta,omitempty"`
}

// SelectedOutcome / CancelledOutcome are convenience constructors.
func SelectedOutcome(optionID string) RequestPermissionResult {
	return RequestPermissionResult{Outcome: PermissionOutcome{Outcome: OutcomeSelected, OptionID: optionID}}
}

func CancelledOutcome() RequestPermissionResult {
	return RequestPermissionResult{Outcome: PermissionOutcome{Outcome: OutcomeCancelled}}
}

// ---------- client-side fs / terminal methods ----------

type ReadTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

type ReadTextFileResult struct {
	Content string `json:"content"`
}

type WriteTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

type CreateTerminalParams struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	CWD             string        `json:"cwd,omitempty"`
	OutputByteLimit *int          `json:"outputByteLimit,omitempty"`
}

type CreateTerminalResult struct {
	TerminalID string `json:"terminalId"`
}

type TerminalOutputParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type TerminalExitStatus struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

type TerminalOutputResult struct {
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
}

type WaitForTerminalExitParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type WaitForTerminalExitResult struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

type ReleaseTerminalParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type KillTerminalParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}
