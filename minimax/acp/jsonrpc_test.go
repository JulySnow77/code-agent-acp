package acp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRequestIDRoundTrip(t *testing.T) {
	cases := []RequestID{IntID(0), IntID(1), IntID(-42), StringID("abc"), StringID("1"), StringID("中文\"id")}
	for _, id := range cases {
		b, err := json.Marshal(id)
		if err != nil {
			t.Fatalf("marshal %s: %v", id, err)
		}
		var got RequestID
		if err := got.UnmarshalJSON(b); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got.Key() != id.Key() {
			t.Fatalf("round trip: got %s want %s", got.Key(), id.Key())
		}
	}
	if IntID(1).Key() == StringID("1").Key() {
		t.Fatal("integer 1 and string \"1\" must be distinct ids")
	}
}

func TestRequestIDRejectsInvalid(t *testing.T) {
	for _, raw := range []string{"null", "1.5", "true", "{}"} {
		var id RequestID
		if err := id.UnmarshalJSON([]byte(raw)); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
	var zero RequestID
	if _, err := json.Marshal(zero); err == nil {
		t.Fatal("zero RequestID must not marshal")
	}
}

func TestParseMessageKinds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind MessageType
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, MessageRequest},
		{"string id", `{"jsonrpc":"2.0","id":"x-1","method":"session/update","params":{}}`, MessageRequest},
		{"notification", `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`, MessageNotification},
		{"result", `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`, MessageResponse},
		{"null result", `{"jsonrpc":"2.0","id":1,"result":null}`, MessageResponse},
		{"error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`, MessageResponse},
		{"error with null id", `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"bad"}}`, MessageResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, perr := ParseMessage([]byte(tc.raw))
			if perr != nil {
				t.Fatalf("unexpected parse error: %v", perr)
			}
			if m.Type != tc.kind {
				t.Fatalf("kind = %v want %v", m.Type, tc.kind)
			}
		})
	}
}

func TestParseMessageRejectsInvalid(t *testing.T) {
	cases := []struct {
		raw  string
		code int
	}{
		{`not json`, CodeParseError},
		{`{"id":1,"method":"x"}`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1,"method":""}`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":null,"method":"x"}`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1.5,"method":"x"}`, CodeInvalidRequest},
	}
	for _, tc := range cases {
		_, perr := ParseMessage([]byte(tc.raw))
		if perr == nil || perr.Code != tc.code {
			t.Fatalf("%s: got %v, want code %d", tc.raw, perr, tc.code)
		}
	}
}

func TestFrameConstructors(t *testing.T) {
	req, err := NewRequestFrame(IntID(7), "session/prompt", map[string]any{"text": "line1\nline2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(req), "\n\r") {
		t.Fatalf("frame contains a literal newline: %s", req)
	}
	var m map[string]any
	if err := json.Unmarshal(req, &m); err != nil {
		t.Fatal(err)
	}
	if m["jsonrpc"] != "2.0" || m["id"] != float64(7) || m["method"] != "session/prompt" {
		t.Fatalf("unexpected frame: %s", req)
	}

	note, err := NewNotificationFrame("session/update", map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(note), `"id"`) {
		t.Fatalf("notification must not have an id: %s", note)
	}

	res, err := NewResultFrame(StringID("r"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res), `"result":null`) {
		t.Fatalf("nil result must be JSON null: %s", res)
	}

	ef, err := NewErrorFrameNullID(Errorf(CodeParseError, "bad"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ef), `"id":null`) {
		t.Fatalf("parse-error response must use a null id: %s", ef)
	}
}

func TestRPCErrorImplementsError(t *testing.T) {
	var err error = MethodNotFound("x/y")
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("errors.As failed: %v", err)
	}
	if data, ok := rpcErr.Data.(map[string]any); !ok || data["method"] != "x/y" {
		t.Fatalf("unexpected error data: %#v", rpcErr.Data)
	}
}
