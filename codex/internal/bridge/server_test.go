package bridge

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"codex-acp-bridge/internal/protocol"
	"codex-acp-bridge/internal/testutil"
)

type testRig struct {
	fake    *testutil.FakeCodex
	client  *testutil.Harness
	server  *Server
	done    chan error
	closers []io.Closer
}

func newTestRig(t *testing.T, permissionChoice string, tweak func(*Config)) *testRig {
	t.Helper()
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()

	fake := testutil.NewFakeCodex()
	cfg := Config{
		ApprovalPolicy:     "on-request",
		Sandbox:            "workspace-write",
		TurnTimeout:        10 * time.Second,
		PermissionFallback: "decline",
	}
	if tweak != nil {
		tweak(&cfg)
	}
	server := New(fake, toServerR, fromServerW, cfg)
	done := make(chan error, 1)
	go func() { done <- server.Run() }()

	client := testutil.NewHarness(toServerW, fromServerR, permissionChoice)
	client.Start()
	rig := &testRig{
		fake:    fake,
		client:  client,
		server:  server,
		done:    done,
		closers: []io.Closer{toServerW, fromServerR, toServerR, fromServerW},
	}
	t.Cleanup(func() {
		server.Shutdown()
		for _, closer := range rig.closers {
			_ = closer.Close()
		}
		select {
		case <-rig.done:
		case <-time.After(3 * time.Second):
			t.Error("server.Run did not exit after shutdown")
		}
	})
	return rig
}

func (r *testRig) initialize(t *testing.T) protocol.Message {
	t.Helper()
	result, rpcErr := r.client.Call(protocol.MethodInitialize, protocol.Message{
		"protocolVersion": 1,
		"clientInfo":      protocol.Message{"name": "test-host", "version": "1"},
	})
	if rpcErr != nil {
		t.Fatalf("initialize failed: %v", rpcErr)
	}
	return result
}

func (r *testRig) newSession(t *testing.T, params protocol.Message) string {
	t.Helper()
	result, rpcErr := r.client.Call(protocol.MethodSessionNew, params)
	if rpcErr != nil {
		t.Fatalf("session/new failed: %v", rpcErr)
	}
	id := protocol.Str(result, "sessionId")
	if id == "" {
		t.Fatalf("session/new returned no sessionId: %v", result)
	}
	return id
}

func waitForRequest(t *testing.T, fake *testutil.FakeCodex, method string, timeout time.Duration) protocol.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if params, ok := fake.FindRequest(method); ok {
			return params
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("app-server request %s was never issued", method)
	return nil
}

func updateKinds(updates []protocol.Message) []string {
	kinds := make([]string, 0, len(updates))
	for _, update := range updates {
		kinds = append(kinds, protocol.Str(protocol.Sub(update, "update"), "sessionUpdate"))
	}
	return kinds
}

func updateText(update protocol.Message) string {
	return protocol.Str(protocol.Sub(protocol.Sub(update, "update"), "content"), "text")
}

func hasUpdate(updates []protocol.Message, kind, contains string) bool {
	for _, update := range updates {
		body := protocol.Sub(update, "update")
		if protocol.Str(body, "sessionUpdate") != kind {
			continue
		}
		if contains == "" || strings.Contains(updateText(update), contains) {
			return true
		}
	}
	return false
}

func TestInitializeAndNegotiation(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	result := rig.initialize(t)
	if int(protocol.Num(result, "protocolVersion")) != protocol.Version {
		t.Fatalf("protocolVersion = %v, want %d", result["protocolVersion"], protocol.Version)
	}
	capabilities := protocol.Sub(result, "agentCapabilities")
	if !protocol.Bool(capabilities, "loadSession") {
		t.Error("agentCapabilities.loadSession must be true (session/load is implemented)")
	}
	meta := protocol.Sub(protocol.Sub(result, "_meta"), "codex")
	if protocol.Str(meta, "userAgent") == "" {
		t.Error("initialize _meta should expose the app-server handshake details")
	}

	unknown, rpcErr := rig.client.Call("session/does_not_exist", protocol.Message{})
	if rpcErr == nil || rpcErr.Code != protocol.CodeMethodNotFound {
		t.Fatalf("unknown method should return -32601, got %v (%v)", rpcErr, unknown)
	}
	_, rpcErr = rig.client.Call(protocol.MethodAuthenticate, protocol.Message{})
	if rpcErr == nil {
		t.Error("authenticate must not pretend to work (Codex owns its credentials)")
	}
}

func TestSessionNewPromptAndStreaming(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{
		"cwd": "/tmp/work",
		"mcpServers": []any{
			protocol.Message{
				"name": "fs", "command": "npx", "args": []any{"-y", "mcp-fs"},
				"env": []any{protocol.Message{"name": "ROOT", "value": "/tmp"}},
			},
		},
	})

	threadParams, _ := rig.fake.FindRequest("thread/start")
	if protocol.Str(threadParams, "cwd") != "/tmp/work" {
		t.Errorf("thread/start cwd = %q", protocol.Str(threadParams, "cwd"))
	}
	mcpServers := protocol.Sub(protocol.Sub(threadParams, "config"), "mcp_servers")
	fsServer := protocol.Sub(mcpServers, "fs")
	if protocol.Str(fsServer, "command") != "npx" {
		t.Errorf("stdio MCP server not translated: %v", threadParams["config"])
	}
	if protocol.Str(protocol.Sub(fsServer, "env"), "ROOT") != "/tmp" {
		t.Errorf("MCP env not translated: %v", fsServer["env"])
	}

	rig.fake.SetScript(
		testutil.FakeEvent{Method: "turn/started", Params: protocol.Message{}},
		testutil.FakeEvent{Method: "item/agentMessage/delta", Params: protocol.Message{"itemId": "msg-1", "delta": "Hello "}},
		testutil.FakeEvent{Method: "item/agentMessage/delta", Params: protocol.Message{"itemId": "msg-1", "delta": "world"}},
		testutil.FakeEvent{Method: "item/started", Params: protocol.Message{
			"item": protocol.Message{
				"type": "commandExecution", "id": "cmd-1", "command": "ls -la", "cwd": "/tmp/work",
			},
		}},
		testutil.FakeEvent{Method: "item/commandExecution/outputDelta", Params: protocol.Message{"itemId": "cmd-1", "delta": "a.txt\n"}},
		testutil.FakeEvent{Method: "item/completed", Params: protocol.Message{
			"item": protocol.Message{
				"type": "commandExecution", "id": "cmd-1", "command": "ls -la",
				"aggregatedOutput": "a.txt\n", "exitCode": 0, "status": "completed",
			},
		}},
		testutil.FakeEvent{Method: "thread/tokenUsage/updated", Params: protocol.Message{
			"tokenUsage": protocol.Message{
				"total": protocol.Message{"totalTokens": 42}, "modelContextWindow": 128000,
			},
		}},
		testutil.FakeEvent{Method: "turn/completed", Params: protocol.Message{
			"turn": protocol.Message{"id": "turn-1", "status": "completed"},
		}},
	)

	result, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt": []any{
			protocol.Message{"type": "text", "text": "say hi"},
			protocol.Message{"type": "image", "data": "QUJD", "mimeType": "image/png"},
		},
	})
	if rpcErr != nil {
		t.Fatalf("session/prompt failed: %v", rpcErr)
	}
	if protocol.Str(result, "stopReason") != protocol.StopEndTurn {
		t.Errorf("stopReason = %q, want end_turn", protocol.Str(result, "stopReason"))
	}

	turnParams := waitForRequest(t, rig.fake, "turn/start", 2*time.Second)
	inputs, _ := turnParams["input"].([]protocol.Message)
	if len(inputs) != 2 {
		t.Fatalf("turn/start input has %d items, want 2: %v", len(inputs), turnParams["input"])
	}
	if protocol.Str(inputs[0], "text") != "say hi" {
		t.Errorf("text input = %v", inputs[0])
	}
	if protocol.Str(inputs[1], "url") != "data:image/png;base64,QUJD" {
		t.Errorf("image input = %v", inputs[1])
	}
	if protocol.Str(turnParams, "approvalPolicy") != "on-request" {
		t.Errorf("approvalPolicy = %v", turnParams["approvalPolicy"])
	}

	updates, err := rig.client.WaitUpdates(6, 3*time.Second)
	if err != nil {
		t.Fatalf("%v; kinds=%v", err, updateKinds(rig.client.Updates()))
	}
	if !hasUpdate(updates, protocol.UpdateAgentMessageChunk, "") {
		t.Error("missing agent_message_chunk")
	}
	var fullText string
	for _, update := range updates {
		body := protocol.Sub(update, "update")
		if protocol.Str(body, "sessionUpdate") == protocol.UpdateAgentMessageChunk {
			fullText += updateText(update)
		}
	}
	if fullText != "Hello world" {
		t.Errorf("streamed text = %q, want %q", fullText, "Hello world")
	}
	if !hasUpdate(updates, protocol.UpdateToolCall, "") {
		t.Errorf("missing tool_call; kinds=%v", updateKinds(updates))
	}
	if !hasUpdate(updates, protocol.UpdateUsage, "") {
		t.Errorf("missing usage_update; kinds=%v", updateKinds(updates))
	}
	for _, update := range updates {
		body := protocol.Sub(update, "update")
		if protocol.Str(body, "sessionUpdate") == protocol.UpdateToolCall {
			if protocol.Str(body, "kind") != protocol.ToolKindExecute {
				t.Errorf("tool kind = %q", protocol.Str(body, "kind"))
			}
		}
	}
}

func TestSetModeAndModelAffectNextTurn(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})

	if _, err := rig.client.Call(protocol.MethodSessionSetMode, protocol.Message{
		"sessionId": sessionID, "modeId": ModeAuto,
	}); err != nil {
		t.Fatalf("set_mode failed: %v", err)
	}
	if _, err := rig.client.Call(protocol.MethodSessionSetModel, protocol.Message{
		"sessionId": sessionID, "modelId": "gpt-5-codex",
	}); err != nil {
		t.Fatalf("set_model failed: %v", err)
	}
	if _, err := rig.client.Call(protocol.MethodSessionSetMode, protocol.Message{
		"sessionId": sessionID, "modeId": "bogus",
	}); err == nil {
		t.Error("unknown modeId should be rejected")
	}

	rig.fake.SetScript(
		testutil.FakeEvent{Method: "turn/completed", Params: protocol.Message{
			"turn": protocol.Message{"status": "completed"},
		}},
	)
	if _, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt":    []any{protocol.Message{"type": "text", "text": "go"}},
	}); rpcErr != nil {
		t.Fatalf("prompt failed: %v", rpcErr)
	}
	turnParams := waitForRequest(t, rig.fake, "turn/start", 2*time.Second)
	if protocol.Str(turnParams, "approvalPolicy") != "never" {
		t.Errorf("mode=auto should map to approvalPolicy never, got %v", turnParams["approvalPolicy"])
	}
	if protocol.Str(turnParams, "model") != "gpt-5-codex" {
		t.Errorf("model = %v", turnParams["model"])
	}
}

func TestCancelInterruptsTurn(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	rig.fake.SetScript(testutil.FakeEvent{Method: "turn/started", Params: protocol.Message{}})

	type promptResult struct {
		result protocol.Message
		err    *protocol.RPCError
	}
	reply := make(chan promptResult, 1)
	go func() {
		result, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
			"sessionId": sessionID,
			"prompt":    []any{protocol.Message{"type": "text", "text": "long task"}},
		})
		reply <- promptResult{result, rpcErr}
	}()
	waitForRequest(t, rig.fake, "turn/start", 2*time.Second)
	if err := rig.client.Notify(protocol.MethodSessionCancel, protocol.Message{"sessionId": sessionID}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-reply:
		if got.err != nil {
			t.Fatalf("cancel should not fail the RPC: %v", got.err)
		}
		if protocol.Str(got.result, "stopReason") != protocol.StopCancelled {
			t.Errorf("stopReason = %q, want cancelled", protocol.Str(got.result, "stopReason"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not finish after session/cancel")
	}
	interrupt := waitForRequest(t, rig.fake, "turn/interrupt", 2*time.Second)
	if protocol.Str(interrupt, "threadId") != sessionID {
		t.Errorf("turn/interrupt threadId = %q", protocol.Str(interrupt, "threadId"))
	}
}

func TestPermissionRequestIsBridged(t *testing.T) {
	for _, tc := range []struct {
		choice   string
		expected string
	}{
		{"allow_always", "acceptForSession"},
		{"reject_once", "decline"},
		{"reject_always", "cancel"},
		{"cancelled", "cancel"},
		{"error", "decline"}, // client lacks the method -> configured fallback
	} {
		t.Run(tc.choice, func(t *testing.T) {
			rig := newTestRig(t, tc.choice, nil)
			rig.initialize(t)
			sessionID := rig.newSession(t, protocol.Message{})
			rig.fake.SetScript(
				testutil.FakeEvent{Method: "__server_request__", Params: protocol.Message{
					"id":     77,
					"method": MethodCommandApproval,
					"params": protocol.Message{
						"threadId": sessionID, "itemId": "cmd-9",
						"command": "rm -rf build", "cwd": "/tmp/work",
					},
				}},
				testutil.FakeEvent{Method: "turn/completed", Params: protocol.Message{
					"turn": protocol.Message{"status": "completed"},
				}},
			)
			if _, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
				"sessionId": sessionID,
				"prompt":    []any{protocol.Message{"type": "text", "text": "clean"}},
			}); rpcErr != nil {
				t.Fatalf("prompt failed: %v", rpcErr)
			}
			answers := rig.fake.Answers()
			if len(answers) != 1 {
				t.Fatalf("expected one app-server answer, got %v", answers)
			}
			if protocol.Str(answers[0], "decision") != tc.expected {
				t.Errorf("decision = %q, want %q", protocol.Str(answers[0], "decision"), tc.expected)
			}
			if tc.choice != "error" {
				requests := rig.client.PermissionRequests()
				if len(requests) != 1 {
					t.Fatalf("expected one ACP permission request, got %d", len(requests))
				}
				call := protocol.Sub(requests[0], "toolCall")
				if protocol.Str(call, "kind") != protocol.ToolKindExecute {
					t.Errorf("permission toolCall kind = %q", protocol.Str(call, "kind"))
				}
				if len(protocol.Raw(requests[0], "options")) != 4 {
					t.Errorf("permission options = %v", requests[0]["options"])
				}
			}
		})
	}
}

func TestFailedTurnIsVisible(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	rig.fake.SetScript(
		testutil.FakeEvent{Method: "turn/completed", Params: protocol.Message{
			"turn": protocol.Message{
				"status": "failed",
				"error":  protocol.Message{"message": "rate limit reached"},
			},
		}},
	)
	_, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt":    []any{protocol.Message{"type": "text", "text": "hi"}},
	})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "rate limit reached") {
		t.Fatalf("failed turn must surface an RPC error, got %v", rpcErr)
	}
	updates, err := rig.client.WaitUpdates(1, 3*time.Second)
	if err != nil {
		t.Fatalf("%v; kinds=%v", err, updateKinds(rig.client.Updates()))
	}
	if !hasUpdate(updates, protocol.UpdateAgentMessageChunk, "rate limit reached") {
		t.Errorf("error text missing from transcript: %v", updateKinds(updates))
	}
}

func TestTurnStartFailure(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	rig.fake.FailTurnStart = errors.New("app-server rejected turn/start")
	_, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt":    []any{protocol.Message{"type": "text", "text": "hi"}},
	})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "rejected") {
		t.Fatalf("turn/start failure must be visible, got %v", rpcErr)
	}
}

func TestSessionLoadReplaysHistory(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	rig.fake.SetHistory(protocol.Message{
		"id": "thr-history",
		"turns": []any{
			protocol.Message{
				"id": "turn-old",
				"items": []any{
					protocol.Message{"type": "userMessage", "content": []any{
						protocol.Message{"type": "text", "text": "what is 1+1?"},
					}},
					protocol.Message{"type": "agentMessage", "text": "2"},
					protocol.Message{"type": "commandExecution", "id": "cmd-old", "command": "echo 2", "exitCode": 0},
				},
			},
		},
	})
	if _, rpcErr := rig.client.Call(protocol.MethodSessionLoad, protocol.Message{
		"sessionId": "thr-history",
		"cwd":       "/tmp",
	}); rpcErr != nil {
		t.Fatalf("session/load failed: %v", rpcErr)
	}
	updates, err := rig.client.WaitUpdates(4, 3*time.Second)
	if err != nil {
		t.Fatalf("%v; kinds=%v", err, updateKinds(rig.client.Updates()))
	}
	if !hasUpdate(updates, protocol.UpdateUserMessageChunk, "what is 1+1?") {
		t.Errorf("missing replayed user message: %v", updateKinds(updates))
	}
	if !hasUpdate(updates, protocol.UpdateAgentMessageChunk, "2") {
		t.Errorf("missing replayed agent message: %v", updateKinds(updates))
	}
	if !hasUpdate(updates, protocol.UpdateToolCall, "") || !hasUpdate(updates, protocol.UpdateToolCallUpdate, "") {
		t.Errorf("missing replayed tool call: %v", updateKinds(updates))
	}
	resume, ok := rig.fake.FindRequest("thread/resume")
	if !ok || protocol.Str(resume, "threadId") != "thr-history" {
		t.Errorf("thread/resume params = %v", resume)
	}
}

func TestPromptValidation(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	if _, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": "missing", "prompt": []any{},
	}); rpcErr == nil {
		t.Error("unknown sessionId must be rejected")
	}
	if _, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
	}); rpcErr == nil || rpcErr.Code != protocol.CodeInvalidParams {
		t.Errorf("missing prompt must be -32602, got %v", rpcErr)
	}
	if _, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt":    []any{protocol.Message{"type": "unknown"}},
	}); rpcErr == nil || rpcErr.Code != protocol.CodeInvalidParams {
		t.Errorf("unusable prompt must be -32602, got %v", rpcErr)
	}
}
