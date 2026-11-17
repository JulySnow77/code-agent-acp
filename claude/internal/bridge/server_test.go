package bridge

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"claude-acp-bridge/internal/protocol"
	"claude-acp-bridge/internal/testutil"
)

type testRig struct {
	fake    *testutil.FakeClaude
	client  *testutil.Harness
	server  *Server
	done    chan error
	closers []io.Closer
}

func newTestRig(t *testing.T, permissionChoice string, tweak func(*Config)) *testRig {
	t.Helper()
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()

	fake := testutil.NewFakeClaude()
	cfg := Config{
		Bin:                "claude",
		PermissionMode:     "manual",
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
		t.Error("agentCapabilities.loadSession must be true")
	}
	_, rpcErr := rig.client.Call("session/does_not_exist", protocol.Message{})
	if rpcErr == nil || rpcErr.Code != protocol.CodeMethodNotFound {
		t.Fatalf("unknown method should return -32601, got %v", rpcErr)
	}
	_, rpcErr = rig.client.Call(protocol.MethodAuthenticate, protocol.Message{})
	if rpcErr == nil {
		t.Error("authenticate must not pretend to work")
	}
}

func TestSessionNewLaunchesCLIWithFlags(t *testing.T) {
	rig := newTestRig(t, "allow_once", func(c *Config) { c.Model = "claude-sonnet-4" })
	rig.initialize(t)
	id := rig.newSession(t, protocol.Message{"cwd": "/tmp/work"})
	if _, err := newUUID(); err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 {
		t.Errorf("sessionId = %q, want a UUID", id)
	}
	opened := rig.fake.Opened()
	if len(opened) != 1 {
		t.Fatalf("expected one CLI launch, got %v", opened)
	}
	if protocol.Str(opened[0], "sessionId") != id {
		t.Errorf("CLI sessionId = %v, want %q", opened[0]["sessionId"], id)
	}
	if protocol.Str(opened[0], "cwd") != "/tmp/work" {
		t.Errorf("CLI cwd = %v", opened[0]["cwd"])
	}
	if protocol.Str(opened[0], "model") != "claude-sonnet-4" {
		t.Errorf("CLI model = %v", opened[0]["model"])
	}
	if protocol.Str(opened[0], "permissionMode") != "manual" {
		t.Errorf("CLI permissionMode = %v", opened[0]["permissionMode"])
	}
}

func TestSessionPromptStreamingAndTools(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	fakeSession := rig.fake.Session(sessionID)
	fakeSession.SetScript(
		testutil.FakeEvent{Message: protocol.Message{
			"type": "system", "subtype": "init", "session_id": sessionID, "cwd": "/tmp",
		}},
		testutil.FakeEvent{Message: protocol.Message{
			"type": "assistant", "session_id": sessionID,
			"message": protocol.Message{
				"role":    "assistant",
				"content": []any{protocol.Message{"type": "text", "text": "Hello "}},
				"usage": protocol.Message{"input_tokens": 3, "output_tokens": 2,
					"cache_read_input_tokens": 1, "cache_creation_input_tokens": 0},
			},
		}},
		testutil.FakeEvent{Message: protocol.Message{
			"type": "assistant", "session_id": sessionID,
			"message": protocol.Message{
				"role":    "assistant",
				"content": []any{protocol.Message{"type": "text", "text": "world"}},
			},
		}},
		testutil.FakeEvent{Message: protocol.Message{
			"type": "assistant", "session_id": sessionID,
			"message": protocol.Message{
				"role": "assistant",
				"content": []any{protocol.Message{
					"type": "tool_use", "id": "toolu-1", "name": "Bash",
					"input": protocol.Message{"command": "echo hi"},
				}},
			},
		}},
		testutil.FakeEvent{Message: protocol.Message{
			"type": "user", "session_id": sessionID,
			"message": protocol.Message{
				"role": "user",
				"content": []any{protocol.Message{
					"type": "tool_result", "tool_use_id": "toolu-1", "content": "hi\n",
				}},
			},
		}},
		testutil.FakeEvent{Message: protocol.Message{
			"type": "result", "subtype": "success", "is_error": false,
			"session_id": sessionID, "stop_reason": "end_turn",
			"result": "done",
			"usage":  protocol.Message{"input_tokens": 3, "output_tokens": 2},
			"modelUsage": protocol.Message{"claude-sonnet-4": protocol.Message{
				"contextWindow": 200000,
			}},
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

	prompts := fakeSession.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("expected one user message, got %v", prompts)
	}
	content := protocol.Raw(prompts[0], "content")
	if len(content) != 2 {
		t.Fatalf("user content has %d blocks, want 2: %v", len(content), content)
	}
	image := protocol.AsMessage(content[1])
	source := protocol.Sub(image, "source")
	if protocol.Str(source, "type") != "base64" || protocol.Str(source, "data") != "QUJD" {
		t.Errorf("image source = %v", source)
	}

	updates, err := rig.client.WaitUpdates(5, 3*time.Second)
	if err != nil {
		t.Fatalf("%v; kinds=%v", err, updateKinds(rig.client.Updates()))
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
	if !hasUpdate(updates, protocol.UpdateToolCallUpdate, "") {
		t.Errorf("missing tool call update; kinds=%v", updateKinds(updates))
	}
	if !hasUpdate(updates, protocol.UpdateUsage, "") {
		t.Errorf("missing usage_update; kinds=%v", updateKinds(updates))
	}
}

func TestStreamDeltasAndAssistantNotDuplicated(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	fakeSession := rig.fake.Session(sessionID)
	fakeSession.SetScript(
		testutil.FakeEvent{Message: protocol.Message{
			"type": "stream_event", "event": protocol.Message{
				"type": "content_block_delta", "index": 0,
				"delta": protocol.Message{"type": "text_delta", "text": "abc"},
			},
		}},
		testutil.FakeEvent{Message: protocol.Message{
			"type": "assistant", "message": protocol.Message{
				"role":    "assistant",
				"content": []any{protocol.Message{"type": "text", "text": "abc"}},
			},
		}},
		testutil.FakeEvent{Message: protocol.Message{
			"type": "result", "subtype": "success", "is_error": false,
		}},
	)
	if _, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt":    []any{protocol.Message{"type": "text", "text": "go"}},
	}); rpcErr != nil {
		t.Fatalf("prompt failed: %v", rpcErr)
	}
	updates := rig.client.Updates()
	var fullText string
	for _, update := range updates {
		body := protocol.Sub(update, "update")
		if protocol.Str(body, "sessionUpdate") == protocol.UpdateAgentMessageChunk {
			fullText += updateText(update)
		}
	}
	if fullText != "abc" {
		t.Errorf("text = %q, want %q (no duplication)", fullText, "abc")
	}
}

func TestCancelInterruptsTurn(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	fakeSession := rig.fake.Session(sessionID)
	fakeSession.SetScript(
		testutil.FakeEvent{Message: protocol.Message{"type": "system", "subtype": "init"}},
	)

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
	// Wait for the prompt to register a turn before cancelling.
	deadline := time.Now().Add(2 * time.Second)
	for len(fakeSession.Prompts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
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
	if fakeSession.InterruptCount() != 1 {
		t.Errorf("interrupt count = %d, want 1", fakeSession.InterruptCount())
	}
}

func TestPermissionRequestIsBridged(t *testing.T) {
	for _, tc := range []struct {
		choice string
		allow  bool
	}{
		{"allow_once", true},
		{"allow_always", true},
		{"reject_once", false},
		{"reject_always", false},
		{"cancelled", false},
		{"error", false}, // client lacks the method -> configured fallback
	} {
		t.Run(tc.choice, func(t *testing.T) {
			rig := newTestRig(t, tc.choice, nil)
			rig.initialize(t)
			sessionID := rig.newSession(t, protocol.Message{})
			fakeSession := rig.fake.Session(sessionID)
			fakeSession.SetScript(
				testutil.FakeEvent{Message: protocol.Message{
					"type": "result", "subtype": "success", "is_error": false,
				}},
			)
			promptDone := make(chan *protocol.RPCError, 1)
			go func() {
				_, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
					"sessionId": sessionID,
					"prompt":    []any{protocol.Message{"type": "text", "text": "clean"}},
				})
				promptDone <- rpcErr
			}()
			deadline := time.Now().Add(2 * time.Second)
			for len(fakeSession.Prompts()) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			fakeSession.Ask("req-1", protocol.Message{
				"subtype": "can_use_tool", "tool_name": "Bash", "tool_use_id": "toolu-9",
				"input": protocol.Message{"command": "rm -rf build"},
			})
			if rpcErr := <-promptDone; rpcErr != nil {
				t.Fatalf("prompt failed: %v", rpcErr)
			}
			answers := fakeSession.Answers()
			if len(answers) != 1 {
				t.Fatalf("expected one CLI answer, got %v", answers)
			}
			behavior := protocol.Str(answers[0], "behavior")
			if tc.allow && behavior != protocol.ClaudeBehaviorAllow {
				t.Errorf("behavior = %q, want allow", behavior)
			}
			if !tc.allow && behavior != protocol.ClaudeBehaviorDeny {
				t.Errorf("behavior = %q, want deny", behavior)
			}
			if tc.choice != "error" {
				requests := rig.client.PermissionRequests()
				if len(requests) != 1 {
					t.Fatalf("expected one ACP permission request, got %d", len(requests))
				}
				if len(protocol.Raw(requests[0], "options")) != 4 {
					t.Errorf("permission options = %v", requests[0]["options"])
				}
			}
		})
	}
}

func TestFailedResultIsVisible(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	fakeSession := rig.fake.Session(sessionID)
	fakeSession.SetScript(
		testutil.FakeEvent{Message: protocol.Message{
			"type": "result", "subtype": "error_during_execution", "is_error": true,
			"result": "rate limit reached",
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
		t.Fatalf("%v", err)
	}
	if !hasUpdate(updates, protocol.UpdateAgentMessageChunk, "rate limit reached") {
		t.Errorf("error text missing from transcript: %v", updateKinds(updates))
	}
}

func TestMaxTokensStopReason(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	rig.fake.Session(sessionID).SetScript(
		testutil.FakeEvent{Message: protocol.Message{
			"type": "result", "subtype": "error_max_turns",
		}},
	)
	result, rpcErr := rig.client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt":    []any{protocol.Message{"type": "text", "text": "hi"}},
	})
	if rpcErr != nil {
		t.Fatalf("prompt failed: %v", rpcErr)
	}
	if protocol.Str(result, "stopReason") != protocol.StopMaxTokens {
		t.Errorf("stopReason = %q, want max_tokens", protocol.Str(result, "stopReason"))
	}
}

func TestSetModeAndModelAffectCLI(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	sessionID := rig.newSession(t, protocol.Message{})
	fakeSession := rig.fake.Session(sessionID)

	if _, rpcErr := rig.client.Call(protocol.MethodSessionSetMode, protocol.Message{
		"sessionId": sessionID, "modeId": ModeAuto,
	}); rpcErr != nil {
		t.Fatalf("set_mode failed: %v", rpcErr)
	}
	if _, rpcErr := rig.client.Call(protocol.MethodSessionSetModel, protocol.Message{
		"sessionId": sessionID, "modelId": "opus",
	}); rpcErr != nil {
		t.Fatalf("set_model failed: %v", rpcErr)
	}
	if _, rpcErr := rig.client.Call(protocol.MethodSessionSetMode, protocol.Message{
		"sessionId": sessionID, "modeId": "bogus",
	}); rpcErr == nil {
		t.Error("unknown modeId should be rejected")
	}
	modes := fakeSession.Modes()
	if len(modes) != 1 || modes[0] != "acceptEdits" {
		t.Errorf("CLI modes = %v", modes)
	}
	models := fakeSession.Models()
	if len(models) != 1 || models[0] != "opus" {
		t.Errorf("CLI models = %v", models)
	}
}

func TestSessionLoadUsesResume(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	result, rpcErr := rig.client.Call(protocol.MethodSessionLoad, protocol.Message{
		"sessionId": "existing-uuid", "cwd": "/tmp",
	})
	if rpcErr != nil {
		t.Fatalf("session/load failed: %v", rpcErr)
	}
	if protocol.Num(result, "protocolVersion") != 0 {
		t.Errorf("load result = %v", result)
	}
	opened := rig.fake.Opened()
	if len(opened) != 1 || protocol.Str(opened[0], "resumeId") != "existing-uuid" {
		t.Errorf("CLI resume params = %v", opened)
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

func TestSessionNewFailureIsVisible(t *testing.T) {
	rig := newTestRig(t, "allow_once", nil)
	rig.initialize(t)
	rig.fake.FailOpen(errors.New("claude not logged in"))
	_, rpcErr := rig.client.Call(protocol.MethodSessionNew, protocol.Message{})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "not logged in") {
		t.Fatalf("session/new failure must be visible, got %v", rpcErr)
	}
}
