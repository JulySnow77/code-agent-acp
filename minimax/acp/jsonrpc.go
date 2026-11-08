// Package documentation lives in doc.go. This file, transport.go and conn.go
// implement the generic JSON-RPC 2.0 plumbing; types.go implements the ACP v1
// schema. See the package overview for the method list and vendor
// compatibility notes.
package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// JSONRPCVersion is the only version ACP uses on the JSON-RPC envelope.
const JSONRPCVersion = "2.0"

// JSON-RPC 2.0 standard codes plus the ACP-specific ones from schema/v1.
const (
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodeRequestCancelled = -32800
	CodeAuthRequired     = -32000
	CodeResourceNotFound = -32002
	CodeOther            = -32001
)

// RPCError is the JSON-RPC error object. It also implements error so a failed
// Call can be inspected with errors.As / a type assertion.
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

// Errorf builds an error response object.
func Errorf(code int, format string, args ...any) *RPCError {
	return &RPCError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// MethodNotFound mirrors ACP agents that include the method name in data.
func MethodNotFound(method string) *RPCError {
	return &RPCError{
		Code:    CodeMethodNotFound,
		Message: fmt.Sprintf("%q: %s", "Method not found", method),
		Data:    map[string]any{"method": method},
	}
}

func InvalidParams(format string, args ...any) *RPCError {
	return Errorf(CodeInvalidParams, "Invalid params: "+format, args...)
}

func AuthRequired(message string) *RPCError {
	if message == "" {
		message = "Authentication required"
	}
	return Errorf(CodeAuthRequired, "%s", message)
}

// RequestID is a string or integer JSON-RPC id. ACP forbids null ids; null is
// only tolerated on an error response whose request id could not be parsed.
// The zero value is invalid and must not be marshalled.
type RequestID struct {
	raw      string
	isString bool
	num      int64
	str      string
}

func IntID(n int64) RequestID {
	return RequestID{raw: strconv.FormatInt(n, 10), num: n}
}

func StringID(s string) RequestID {
	b, _ := json.Marshal(s)
	return RequestID{raw: string(b), isString: true, str: s}
}

func (id RequestID) MarshalJSON() ([]byte, error) {
	if id.raw == "" {
		return nil, errors.New("acp: empty request id")
	}
	return []byte(id.raw), nil
}

func (id *RequestID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return fmt.Errorf("acp: request id must be a string or integer, got %q", s)
	}
	if strings.HasPrefix(s, "\"") {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("acp: invalid string request id: %w", err)
		}
		*id = StringID(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("acp: invalid integer request id %q: %w", s, err)
	}
	*id = IntID(n)
	return nil
}

// Key is a stable map key for correlating responses (int 1 and string "1" differ).
func (id RequestID) Key() string { return id.raw }

func (id RequestID) String() string { return id.raw }

func (id RequestID) IsZero() bool { return id.raw == "" }

func (id RequestID) IsString() bool { return id.isString }

func (id RequestID) Int() int64 { return id.num }

func (id RequestID) Text() string { return id.str }

// MessageType classifies an incoming JSON-RPC envelope.
type MessageType int

const (
	MessageRequest MessageType = iota + 1
	MessageNotification
	MessageResponse
)

func (t MessageType) String() string {
	switch t {
	case MessageRequest:
		return "request"
	case MessageNotification:
		return "notification"
	case MessageResponse:
		return "response"
	default:
		return "unknown"
	}
}

// Message is a parsed frame. Params/Result keep their raw bytes so that a
// transparent proxy can forward them without losing unknown fields.
type Message struct {
	Type      MessageType
	ID        RequestID
	HasID     bool
	Method    string
	Params    json.RawMessage
	HasResult bool
	Result    json.RawMessage
	Err       *RPCError
}

// ParseMessage classifies one newline-delimited JSON frame. It never panics on
// malformed input and returns an RPCError suitable for an error response.
func ParseMessage(data []byte) (*Message, *RPCError) {
	// A first pass as a raw object is required to tell `"result": null` apart
	// from an absent result: encoding/json sets *json.RawMessage to nil for null.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, Errorf(CodeParseError, "invalid JSON frame: %v", err)
	}
	if v, ok := obj["jsonrpc"]; !ok || string(v) != `"`+JSONRPCVersion+`"` {
		return nil, Errorf(CodeInvalidRequest, "jsonrpc must be %q", JSONRPCVersion)
	}

	var wire struct {
		Method *string         `json:"method"`
		Params json.RawMessage `json:"params"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, Errorf(CodeParseError, "invalid JSON frame: %v", err)
	}

	m := &Message{Params: wire.Params}
	rawID := strings.TrimSpace(string(obj["id"]))
	if _, hasID := obj["id"]; hasID && rawID != "null" {
		var id RequestID
		if err := id.UnmarshalJSON(obj["id"]); err != nil {
			return nil, Errorf(CodeInvalidRequest, "%v", err)
		}
		m.ID, m.HasID = id, true
	}

	_, hasResult := obj["result"]
	_, hasError := obj["error"]
	switch {
	case wire.Method != nil:
		if *wire.Method == "" {
			return nil, Errorf(CodeInvalidRequest, "method must not be empty")
		}
		if rawID == "null" {
			return nil, Errorf(CodeInvalidRequest, "ACP request ids must not be null")
		}
		m.Method = *wire.Method
		if m.HasID {
			m.Type = MessageRequest
		} else {
			m.Type = MessageNotification
		}
	case hasResult || hasError:
		m.Type = MessageResponse
		if hasResult {
			m.HasResult, m.Result = true, obj["result"]
		}
		m.Err = wire.Error
	default:
		return nil, Errorf(CodeInvalidRequest, "frame is neither a request, notification nor response")
	}
	return m, nil
}

// rawFrame is the canonical envelope used for all outgoing frames.
type rawFrame struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *RequestID       `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

func marshalParam(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(v)
}

func encodeFrame(f rawFrame) ([]byte, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	// ACP stdio frames MUST NOT contain embedded newlines. json.Marshal escapes
	// newlines inside strings, but a json.RawMessage may carry a literal one.
	if strings.ContainsAny(string(b), "\n\r") {
		return nil, errors.New("acp: frame contains an unescaped newline")
	}
	return b, nil
}

func NewRequestFrame(id RequestID, method string, params any) ([]byte, error) {
	p, err := marshalParam(params)
	if err != nil {
		return nil, err
	}
	return encodeFrame(rawFrame{JSONRPC: JSONRPCVersion, ID: &id, Method: method, Params: p})
}

func NewNotificationFrame(method string, params any) ([]byte, error) {
	p, err := marshalParam(params)
	if err != nil {
		return nil, err
	}
	return encodeFrame(rawFrame{JSONRPC: JSONRPCVersion, Method: method, Params: p})
}

func NewResultFrame(id RequestID, result any) ([]byte, error) {
	var raw json.RawMessage
	if result != nil {
		var err error
		raw, err = marshalParam(result)
		if err != nil {
			return nil, err
		}
	} else {
		raw = json.RawMessage("null")
	}
	return encodeFrame(rawFrame{JSONRPC: JSONRPCVersion, ID: &id, Result: &raw})
}

func NewErrorFrame(id RequestID, err *RPCError) ([]byte, error) {
	if err == nil {
		err = Errorf(CodeInternalError, "unknown error")
	}
	return encodeFrame(rawFrame{JSONRPC: JSONRPCVersion, ID: &id, Error: err})
}

// NewErrorFrameNullID is for parse/invalid-request failures where the id could
// not be determined. ACP allows a null id only in this case.
func NewErrorFrameNullID(err *RPCError) ([]byte, error) {
	if err == nil {
		err = Errorf(CodeInternalError, "unknown error")
	}
	return json.Marshal(struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      any       `json:"id"`
		Error   *RPCError `json:"error"`
	}{JSONRPC: JSONRPCVersion, ID: nil, Error: err})
}
