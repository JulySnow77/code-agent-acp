package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContentBlockVariants(t *testing.T) {
	cases := []struct {
		name string
		b    ContentBlock
		want []string
	}{
		{"text", TextBlock("hello\n世界"), []string{`"type":"text"`, `"text":"hello\n世界"`}},
		{"empty text", TextBlock(""), []string{`"type":"text"`, `"text":""`}},
		{"image", ImageBlock("AA==", "image/png"), []string{`"type":"image"`, `"data":"AA=="`, `"mimeType":"image/png"`}},
		{"audio", AudioBlock("AA==", "audio/wav"), []string{`"type":"audio"`, `"data":"AA=="`}},
		{"resource_link", ResourceLinkBlock("main.go", "file:///tmp/main.go"), []string{`"type":"resource_link"`, `"name":"main.go"`, `"uri":"file:///tmp/main.go"`}},
		{"embedded", EmbeddedResourceBlock("package main", "file:///tmp/main.go", "text/x-go"), []string{`"type":"resource"`, `"text":"package main"`, `"uri":"file:///tmp/main.go"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.b)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(b), want) {
					t.Fatalf("%s missing %s: %s", tc.name, want, b)
				}
			}
			var back ContentBlock
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back.Type != tc.b.Type {
				t.Fatalf("type = %q want %q", back.Type, tc.b.Type)
			}
			// Marshal the decoded value again: it must stay a valid variant.
			if _, err := json.Marshal(back); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContentBlockRejectsBrokenEmbedded(t *testing.T) {
	_, err := json.Marshal(ContentBlock{Type: ContentTypeEmbedded})
	if err == nil {
		t.Fatal("embedded resource without payload must fail")
	}
}

func TestContentBlockUnknownVariantPreserved(t *testing.T) {
	raw := `{"type":"vendor_blob","payload":{"a":1}}`
	var b ContentBlock
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != raw {
		t.Fatalf("unknown variant changed: %s", out)
	}
}

func TestMCPServerWireShapes(t *testing.T) {
	stdio := MCPServer{Name: "fs", Command: "/usr/bin/fs-server", Args: []string{"--stdio"}}
	b, err := json.Marshal(stdio)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"type"`) {
		t.Fatalf("stdio server must not carry a type: %s", b)
	}
	if !strings.Contains(string(b), `"args":["--stdio"]`) || !strings.Contains(string(b), `"env":[]`) {
		t.Fatalf("stdio server must always send args and env arrays: %s", b)
	}

	httpServer := MCPServer{Name: "api", Type: "http", URL: "https://example.com/mcp", Headers: []HTTPHeader{{Name: "Authorization", Value: "Bearer x"}}}
	b, err = json.Marshal(httpServer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"type":"http"`) || !strings.Contains(string(b), `"headers":[`) {
		t.Fatalf("http server shape wrong: %s", b)
	}

	for _, original := range []MCPServer{stdio, httpServer, {Name: "sse", Type: "sse", URL: "https://x/sse"}} {
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var back MCPServer
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatal(err)
		}
		if back.Name != original.Name || back.Type != original.Type {
			t.Fatalf("round trip changed server: %#v -> %#v", original, back)
		}
	}
}

func TestSessionUpdateHelpers(t *testing.T) {
	u := AgentMessageChunkWithID("msg_1", "你好")
	if u.Type() != UpdateAgentMessageChunk || u.MessageID() != "msg_1" || u.Text() != "你好" {
		t.Fatalf("bad message chunk: %#v", u)
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var raw SessionUpdate
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Text() != "你好" {
		t.Fatalf("decoded update lost text: %#v", raw)
	}

	status := ToolStatusInProgress
	tu := ToolCallUpdateReport{ToolCallID: "call_1", Status: &status}.SessionUpdate()
	if tu.Type() != UpdateToolCallUpdate || tu.ToolCallID() != "call_1" || tu.ToolStatus() != status {
		t.Fatalf("bad tool_call_update: %#v", tu)
	}
	tc := ToolCallReport{ToolCallID: "call_2", Title: "Read", Kind: ToolKindRead, Status: ToolStatusPending}.SessionUpdate()
	if tc.Type() != UpdateToolCall || tc.ToolTitle() != "Read" || tc.ToolKind() != ToolKindRead {
		t.Fatalf("bad tool_call: %#v", tc)
	}
	if ToolCallText("ok")["type"] != "content" || ToolCallDiff("/tmp/a", "", "new")["newText"] != "new" || ToolCallTerminal("t")["terminalId"] != "t" {
		t.Fatal("tool call content constructors failed")
	}
}

func TestOpenCapabilitiesKeepVendorKeys(t *testing.T) {
	raw := `{"loadSession":true,"sessionCapabilities":{"list":{},"fork":{},"vendorFuture":{"x":1}},"auth":{"logout":{}}}`
	var caps AgentCapabilities
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		t.Fatal(err)
	}
	if !caps.CanLoadSession() || !caps.CanList() || !caps.CanFork() || !caps.CanLogout() {
		t.Fatal("capability helpers failed")
	}
	if _, ok := caps.SessionCapabilities["vendorFuture"]; !ok {
		t.Fatal("vendor capability was dropped")
	}
	b, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "vendorFuture") {
		t.Fatalf("round trip dropped vendor key: %s", b)
	}
	caps.SessionCapabilities.Set("close")
	if !caps.CanClose() {
		t.Fatal("Set did not advertise close")
	}
}

func TestNewSessionResultFlattening(t *testing.T) {
	r := NewSessionResult{SessionID: "s1"}
	r.Modes = &SessionModeState{CurrentModeID: "build", AvailableModes: []SessionMode{{ID: "build", Name: "Build"}}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SessionStateResult") || !strings.Contains(string(b), `"sessionId":"s1"`) {
		t.Fatalf("result fields not flattened: %s", b)
	}
	var back NewSessionResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.SessionID != "s1" || back.Modes == nil || back.Modes.CurrentModeID != "build" {
		t.Fatalf("round trip failed: %#v", back)
	}
}
