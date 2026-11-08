package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy-acp-bridge/internal/protocol"
	"workbuddy-acp-bridge/internal/testutil"
)

func newLoopbackServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestHandshakeAndInitialize(t *testing.T) {
	fake := testutil.NewFakeGateway()
	defer fake.Close()
	client, err := New(fake.Endpoint(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if result["protocolVersion"] != float64(1) {
		t.Fatalf("unexpected initialize result: %v", result)
	}
	methods := fake.Methods()
	if len(methods) != 1 || methods[0] != "initialize" {
		t.Fatalf("only initialize should be sent after connect: %v", methods)
	}
	for _, accept := range fake.Accepts() {
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			t.Fatalf("dual Accept header missing: %q", accept)
		}
	}
}

func TestCallPropagatesACPErrorWithData(t *testing.T) {
	fake := testutil.NewFakeGateway()
	defer fake.Close()
	client, err := New(fake.Endpoint(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call("session/load", protocol.Message{
		"sessionId": "occupied", "cwd": "/tmp", "mcpServers": []any{},
	}, nil, nil)
	if err == nil {
		t.Fatal("writer_occupied must be propagated")
	}
	typed, ok := err.(*Error)
	if !ok || typed.Category != "acp_error" {
		t.Fatalf("unexpected error: %#v", err)
	}
	data, _ := typed.Data.(map[string]any)
	if !strings.Contains(typed.Message, "external writer") || data["reason"] != "writer_occupied" {
		t.Fatalf("error detail lost: %v %v", typed.Message, typed.Data)
	}
}

func TestCallStreamsEventsAndRequests(t *testing.T) {
	fake := testutil.NewFakeGateway()
	defer fake.Close()
	fake.SetEmitPermission(true)
	client, err := New(fake.Endpoint(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var events, requests []protocol.Message
	result, err := client.Call("session/prompt", protocol.Message{
		"sessionId": "gateway-session", "prompt": []any{},
	}, func(message protocol.Message) { events = append(events, message) },
		func(message protocol.Message) { requests = append(requests, message) })
	if err != nil {
		t.Fatal(err)
	}
	if result["stopReason"] != "end_turn" {
		t.Fatalf("unexpected result: %v", result)
	}
	if len(events) != 4 || len(requests) != 1 {
		t.Fatalf("expected 4 method frames and 1 permission request, got %d/%d", len(events), len(requests))
	}
	if requests[0]["method"] != "session/request_permission" {
		t.Fatalf("unexpected request: %v", requests[0])
	}
}

func TestRespondSendsResultFrame(t *testing.T) {
	fake := testutil.NewFakeGateway()
	defer fake.Close()
	client, err := New(fake.Endpoint(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Respond("perm-1", protocol.Message{
		"outcome": protocol.Message{"outcome": "selected"},
	}); err != nil {
		t.Fatal(err)
	}
	replies := fake.Replies()
	if len(replies) != 1 {
		t.Fatalf("expected one reply, got %v", replies)
	}
	result, _ := replies[0]["result"].(map[string]any)
	outcome, _ := result["outcome"].(map[string]any)
	if outcome["outcome"] != "selected" {
		t.Fatalf("unexpected reply: %v", replies[0])
	}
}

func TestStreamClosedBeforeResultIsClassified(t *testing.T) {
	server := newLoopbackServer(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/acp/connect" {
			_, _ = writer.Write([]byte(`{"connectionId":"c","sessionToken":"t"}`))
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(":ok\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{}}\n\n"))
	})
	client, err := New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call("session/prompt", protocol.Message{}, nil, nil)
	typed, ok := err.(*Error)
	if !ok || typed.Category != "stream" {
		t.Fatalf("expected stream error, got %#v", err)
	}
}

func TestHTTPAndCapabilityErrorsAreClassified(t *testing.T) {
	server := newLoopbackServer(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/acp/connect" {
			writer.WriteHeader(http.StatusNotAcceptable)
			_, _ = writer.Write([]byte("Not Acceptable"))
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	if _, err := New(server.URL, 5*time.Second); err == nil {
		t.Fatal("expected an http error")
	} else if typed, ok := err.(*Error); !ok || typed.Category != "http" {
		t.Fatalf("unexpected error: %#v", err)
	}

	empty := newLoopbackServer(t, func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{}`))
	})
	if _, err := New(empty.URL, 5*time.Second); err == nil {
		t.Fatal("expected a capability error")
	} else if typed, ok := err.(*Error); !ok || typed.Category != "capability" {
		t.Fatalf("unexpected error: %#v", err)
	}
}
