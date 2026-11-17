// Package protocol implements the two newline-delimited JSON-RPC dialects the
// bridge speaks:
//
//   - ACP v1 on stdin/stdout: standard JSON-RPC 2.0, every frame carries
//     "jsonrpc":"2.0".
//   - Codex app-server on the child process pipes: JSON-RPC *lite*, i.e. the
//     same {id,method,params} / {id,result} / {method,params} envelopes but
//     WITHOUT the "jsonrpc" member (codex-rs/app-server-protocol/
//     jsonrpc_lite.rs). A "jsonrpc":"2.0" member is tolerated on input.
//
// Both dialects are one compact JSON object per line. Nothing else may ever be
// written to stdout while `serve` is running; diagnostics go to stderr.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ACP protocol constants.
const (
	Version        = 1
	AgentName      = "codex-acp-bridge"
	VersionString  = "00.00.01"
	JSONRPCVersion = "2.0"
)

// JSON-RPC 2.0 error codes used by the bridge.
const (
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodeRequestCancelled = -32800
	CodeAuthRequired     = -32000
)

// ACP client -> agent methods.
const (
	MethodInitialize          = "initialize"
	MethodAuthenticate        = "authenticate"
	MethodSessionNew          = "session/new"
	MethodSessionLoad         = "session/load"
	MethodSessionPrompt       = "session/prompt"
	MethodSessionCancel       = "session/cancel"
	MethodSessionSetMode      = "session/set_mode"
	MethodSessionSetModel     = "session/set_model" // extension, mirrors zcode-acp-bridge
	MethodSessionSetConfigOpt = "session/set_config_option"
)

// ACP agent -> client methods and notifications.
const (
	MethodSessionRequestPermission = "session/request_permission"
	MethodSessionUpdate            = "session/update"
)

// session/update discriminators.
const (
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdatePlan              = "plan"
	UpdateCurrentMode       = "current_mode_update"
	UpdateUsage             = "usage_update"
)

// StopReason values.
const (
	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
	StopRefusal   = "refusal"
)

// ToolCall kinds / statuses.
const (
	ToolKindRead    = "read"
	ToolKindEdit    = "edit"
	ToolKindExecute = "execute"
	ToolKindFetch   = "fetch"
	ToolKindThink   = "think"
	ToolKindOther   = "other"

	ToolStatusPending    = "pending"
	ToolStatusInProgress = "in_progress"
	ToolStatusCompleted  = "completed"
	ToolStatusFailed     = "failed"
)

// Permission option kinds / outcomes (ACP schema/v1).
const (
	PermissionAllowOnce    = "allow_once"
	PermissionAllowAlways  = "allow_always"
	PermissionRejectOnce   = "reject_once"
	PermissionRejectAlways = "reject_always"

	OutcomeCancelled = "cancelled"
	OutcomeSelected  = "selected"
)

// Message is one JSON-RPC object. Keeping it as a map preserves vendor fields
// and lets the same type carry both dialects.
type Message map[string]any

// RPCError is a JSON-RPC error object that also implements error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("acp error %d: %s", e.Code, e.Message)
}

// Encode serializes compact UTF-8 JSON without HTML escaping.
func Encode(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// ACPFrame serializes one ACP stdio frame (jsonrpc header + newline).
func ACPFrame(message Message) ([]byte, error) {
	out := make(Message, len(message)+1)
	for key, value := range message {
		out[key] = value
	}
	out["jsonrpc"] = JSONRPCVersion
	return encodeLine(out)
}

// LiteFrame serializes one Codex app-server frame (no jsonrpc header).
func LiteFrame(message Message) ([]byte, error) {
	out := make(Message, len(message))
	for key, value := range message {
		if key != "jsonrpc" {
			out[key] = value
		}
	}
	return encodeLine(out)
}

func encodeLine(message Message) ([]byte, error) {
	body, err := Encode(message)
	if err != nil {
		return nil, err
	}
	if bytes.ContainsAny(body, "\n\r") {
		return nil, errors.New("protocol: frame contains an unescaped newline")
	}
	return append(body, '\n'), nil
}

// ParseFrame decodes one ndjson frame. When requireVersion is true (ACP) the
// frame must carry "jsonrpc":"2.0"; the Codex dialect omits it.
func ParseFrame(line []byte, requireVersion bool) (Message, error) {
	var message Message
	if err := json.Unmarshal(line, &message); err != nil {
		return nil, fmt.Errorf("protocol: invalid JSON frame: %w", err)
	}
	if message == nil {
		return nil, errors.New("protocol: frame is null")
	}
	if requireVersion && message["jsonrpc"] != JSONRPCVersion {
		return nil, errors.New("protocol: ACP frame must be a JSON-RPC 2.0 object")
	}
	return message, nil
}

// Result builds a successful JSON-RPC response.
func Result(id any, result Message) Message {
	if result == nil {
		result = Message{}
	}
	return Message{"id": id, "result": result}
}

// RPCErrorResponse builds an error response.
func RPCErrorResponse(id any, code int, message string, data any) Message {
	errObject := Message{"code": code, "message": message}
	if data != nil {
		errObject["data"] = data
	}
	return Message{"id": id, "error": errObject}
}

// ErrorResponse adapts an *RPCError to a frame.
func ErrorResponse(id any, err *RPCError) Message {
	return RPCErrorResponse(id, err.Code, err.Message, err.Data)
}

// IDKey returns a stable key for a JSON-RPC id (int 1 and string "1" differ).
func IDKey(id any) string {
	body, err := Encode(id)
	if err != nil {
		return fmt.Sprintf("%T:%v", id, id)
	}
	return string(body)
}

// ---- small map helpers used by the translator ----

func Str(m Message, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return value
}

// AsMessage accepts both JSON-decoded objects and in-process Message values.
func AsMessage(value any) Message {
	switch typed := value.(type) {
	case map[string]any:
		return Message(typed)
	case Message:
		return typed
	}
	return nil
}

func Sub(m Message, key string) Message {
	if m == nil {
		return nil
	}
	return AsMessage(m[key])
}

// AsSlice accepts the shapes arrays take in decoded JSON and in-process code.
func AsSlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []Message:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	}
	return nil
}

func Raw(m Message, key string) []any {
	if m == nil {
		return nil
	}
	return AsSlice(m[key])
}

func Num(m Message, key string) float64 {
	if m == nil {
		return 0
	}
	switch value := m[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		n, _ := value.Float64()
		return n
	}
	return 0
}

func Bool(m Message, key string) bool {
	if m == nil {
		return false
	}
	value, _ := m[key].(bool)
	return value
}

// StringValue tolerates non-string scalar values (some Codex versions send
// numeric ids for statuses/timestamps) without panicking.
func StringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		body, err := Encode(typed)
		if err != nil {
			return ""
		}
		return string(body)
	}
}
