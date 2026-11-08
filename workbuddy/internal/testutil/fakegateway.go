// Package testutil provides an in-process WorkBuddy loopback ACP gateway
// simulator. It binds 127.0.0.1 on an ephemeral port, reproduces the
// handshake constraints observed on the real client (dual Accept header,
// Acp-Connection-Id) and can stream session/update events, permission
// requests and writer_occupied errors. No real conversation is touched.
package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"workbuddy-acp-bridge/internal/protocol"
)

// RequestRecord is one observed JSON-RPC request.
type RequestRecord struct {
	Body    protocol.Message
	Headers http.Header
}

// FakeGateway is a deterministic loopback ACP gateway for tests.
type FakeGateway struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []RequestRecord
	replies  []protocol.Message

	emitPermission bool
	promptText     string
}

// NewFakeGateway starts the simulator.
func NewFakeGateway() *FakeGateway {
	gateway := &FakeGateway{promptText: `{"value":"works"}`}
	gateway.server = httptest.NewServer(http.HandlerFunc(gateway.handle))
	return gateway
}

// Endpoint returns the loopback URL.
func (f *FakeGateway) Endpoint() string { return f.server.URL }

// Close stops the simulator.
func (f *FakeGateway) Close() { f.server.Close() }

// SetEmitPermission makes session/prompt emit one permission request.
func (f *FakeGateway) SetEmitPermission(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitPermission = value
}

// SetPromptText overrides the final agent_message_chunk text.
func (f *FakeGateway) SetPromptText(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promptText = value
}

func (f *FakeGateway) settings() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emitPermission, f.promptText
}

// Methods returns the JSON-RPC methods observed so far.
func (f *FakeGateway) Methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	methods := make([]string, 0, len(f.requests))
	for _, request := range f.requests {
		if method, ok := request.Body["method"].(string); ok {
			methods = append(methods, method)
		}
	}
	return methods
}

// Accepts returns the Accept headers observed so far.
func (f *FakeGateway) Accepts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	values := make([]string, 0, len(f.requests))
	for _, request := range f.requests {
		values = append(values, request.Headers.Get("Accept"))
	}
	return values
}

// Replies returns result-only frames sent back by the ACP client.
func (f *FakeGateway) Replies() []protocol.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Message(nil), f.replies...)
}

func (f *FakeGateway) handle(writer http.ResponseWriter, request *http.Request) {
	var body protocol.Message
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, protocol.Message{"error": "invalid json"})
		return
	}
	switch request.URL.Path {
	case "/api/v1/acp/connect":
		writeJSON(writer, http.StatusOK, protocol.Message{
			"connectionId": "fake-connection", "sessionToken": "fake-token",
		})
		return
	case "/api/v1/acp":
		// handled below
	default:
		writeJSON(writer, http.StatusNotFound, protocol.Message{"error": "not found"})
		return
	}
	accept := request.Header.Get("Accept")
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		writeSSE(writer, http.StatusNotAcceptable, []protocol.Message{{
			"jsonrpc": "2.0", "id": nil,
			"error": protocol.Message{
				"code": -32000,
				"message": "Not Acceptable: Client must accept both application/json " +
					"and text/event-stream",
			},
		}})
		return
	}
	if request.Header.Get("Acp-Connection-Id") == "" {
		writeJSON(writer, http.StatusUnauthorized, protocol.Message{"error": "missing connection id"})
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, RequestRecord{Body: body, Headers: request.Header.Clone()})
	f.mu.Unlock()
	if _, hasMethod := body["method"]; !hasMethod {
		f.mu.Lock()
		f.replies = append(f.replies, body)
		f.mu.Unlock()
		writeSSE(writer, http.StatusOK, nil)
		return
	}
	writeSSE(writer, http.StatusOK, f.respond(body))
}

func (f *FakeGateway) respond(body protocol.Message) []protocol.Message {
	method, _ := body["method"].(string)
	identifier := body["id"]
	params := protocol.Message{}
	if raw, ok := body["params"].(map[string]any); ok {
		params = protocol.Message(raw)
	}
	emitPermission, promptText := f.settings()
	sessionID, _ := params["sessionId"].(string)
	var result protocol.Message
	events := []protocol.Message{}
	switch method {
	case "initialize":
		result = protocol.Message{
			"protocolVersion": 1,
			"agentCapabilities": protocol.Message{
				"loadSession":        true,
				"promptCapabilities": protocol.Message{"image": true},
				"mcpCapabilities":    protocol.Message{"http": true, "sse": true},
				"multitaskSupport":   true,
			},
			"authMethods": []any{protocol.Message{"id": "internal"}},
		}
	case "session/new":
		events = append(events, protocol.Message{
			"jsonrpc": "2.0", "method": "session/update",
			"params": protocol.Message{
				"sessionId": "gateway-session",
				"update": protocol.Message{
					"sessionUpdate": "config_option_update",
					"configOptions": []any{protocol.Message{"id": "mode", "currentValue": "fullAccess"}},
				},
			},
		})
		result = protocol.Message{"sessionId": "gateway-session"}
	case "session/load":
		if params["sessionId"] == "occupied" {
			return []protocol.Message{{
				"jsonrpc": "2.0", "id": identifier,
				"error": protocol.Message{
					"code":    -32000,
					"message": "Persistent Session already has an external writer",
					"data":    protocol.Message{"reason": "writer_occupied"},
				},
			}}
		}
		result = protocol.Message{}
	case "session/prompt":
		events = append(events, chunk(sessionID, "Working on the task.\n"))
		if emitPermission {
			events = append(events, protocol.Message{
				"jsonrpc": "2.0", "id": "perm-1", "method": "session/request_permission",
				"params": protocol.Message{
					"sessionId": sessionID,
					"toolCall":  protocol.Message{"toolCallId": "tool-1", "kind": "read"},
					"options":   []any{protocol.Message{"optionId": "allow", "kind": "allow_once"}},
				},
			})
		}
		events = append(events, chunk(sessionID, promptText))
		events = append(events, protocol.Message{
			"jsonrpc": "2.0", "method": "session/update",
			"params": protocol.Message{
				"sessionId": sessionID,
				"update": protocol.Message{
					"sessionUpdate": "tool_call", "toolCallId": "tool-1",
				},
			},
		})
		result = protocol.Message{"stopReason": "end_turn"}
	case "session/cancel":
		result = protocol.Message{}
	default:
		return []protocol.Message{{
			"jsonrpc": "2.0", "id": identifier,
			"error": protocol.Message{"code": -32601, "message": "Method not found"},
		}}
	}
	return append(events, protocol.Message{
		"jsonrpc": "2.0", "id": identifier, "result": result,
	})
}

func chunk(sessionID, text string) protocol.Message {
	return protocol.Message{
		"jsonrpc": "2.0", "method": "session/update",
		"params": protocol.Message{
			"sessionId": sessionID,
			"update": protocol.Message{
				"sessionUpdate": "agent_message_chunk",
				"content":       protocol.Message{"type": "text", "text": text},
			},
		},
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, _ := protocol.Encode(value)
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func writeSSE(writer http.ResponseWriter, status int, messages []protocol.Message) {
	var builder strings.Builder
	builder.WriteString(":ok\n\n")
	for _, message := range messages {
		encoded, err := protocol.Encode(message)
		if err != nil {
			continue
		}
		builder.WriteString("event: message\ndata: ")
		builder.Write(encoded)
		builder.WriteString("\n\n")
	}
	body := []byte(builder.String())
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

// ClientFrames serializes ACP client frames for stdio tests.
func ClientFrames(messages ...protocol.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		frame, err := protocol.Frame(message)
		if err != nil {
			panic(err)
		}
		builder.Write(frame)
	}
	return builder.String()
}
