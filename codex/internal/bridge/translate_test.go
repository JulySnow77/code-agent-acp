package bridge

import (
	"testing"

	"codex-acp-bridge/internal/protocol"
)

func TestTranslatorMapsDeltasToolsAndUsage(t *testing.T) {
	translator := newTranslator()
	var updates []protocol.Message
	events := []struct {
		method string
		params protocol.Message
	}{
		{"item/agentMessage/delta", protocol.Message{"delta": "hi"}},
		{"item/reasoning/textDelta", protocol.Message{"delta": "thinking"}},
		{"item/started", protocol.Message{"item": protocol.Message{
			"type": "fileChange", "id": "patch-1", "status": "inProgress",
		}}},
		{"item/fileChange/outputDelta", protocol.Message{"itemId": "patch-1", "delta": "+line"}},
		{"item/completed", protocol.Message{"item": protocol.Message{
			"type": "fileChange", "id": "patch-1", "status": "completed",
		}}},
		{"thread/tokenUsage/updated", protocol.Message{"tokenUsage": protocol.Message{
			"total": protocol.Message{"totalTokens": 12}, "modelContextWindow": 200000,
		}}},
	}
	for _, event := range events {
		updates = append(updates, translator.translate(event.method, event.params)...)
	}
	if len(updates) != 6 {
		t.Fatalf("got %d updates, want 6: %v", len(updates), updates)
	}
	assertKind := func(index int, want string) {
		t.Helper()
		if got := protocol.Str(updates[index], "sessionUpdate"); got != want {
			t.Errorf("update[%d] = %q, want %q", index, got, want)
		}
	}
	assertKind(0, protocol.UpdateAgentMessageChunk)
	assertKind(1, protocol.UpdateAgentThoughtChunk)
	assertKind(2, protocol.UpdateToolCall)
	assertKind(3, protocol.UpdateToolCallUpdate)
	assertKind(4, protocol.UpdateToolCallUpdate)
	assertKind(5, protocol.UpdateUsage)
	if protocol.Str(updates[2], "kind") != protocol.ToolKindEdit {
		t.Errorf("fileChange kind = %v", updates[2]["kind"])
	}
	if protocol.Str(updates[4], "status") != protocol.ToolStatusCompleted {
		t.Errorf("final tool status = %v", updates[4]["status"])
	}
	if protocol.Num(updates[5], "used") != 12 || protocol.Num(updates[5], "size") != 200000 {
		t.Errorf("usage = %v", updates[5])
	}
}

func TestTranslatorSynthesizesToolForOrphanOutput(t *testing.T) {
	translator := newTranslator()
	updates := translator.translate("command/exec/outputDelta", protocol.Message{
		"itemId": "orphan", "delta": "output",
	})
	if len(updates) != 2 {
		t.Fatalf("expected tool_call + tool_call_update, got %v", updates)
	}
	if protocol.Str(updates[0], "sessionUpdate") != protocol.UpdateToolCall {
		t.Errorf("first update = %v", updates[0])
	}
}

func TestTranslatorAccumulatesPlanUntilItemCompletes(t *testing.T) {
	translator := newTranslator()
	if got := translator.translate("item/plan/delta", protocol.Message{"delta": "- step one\n- step two\n"}); len(got) != 0 {
		t.Errorf("plan deltas should not emit partial updates: %v", got)
	}
	updates := translator.translate("item/completed", protocol.Message{
		"item": protocol.Message{"type": "plan", "id": "plan-1"},
	})
	if len(updates) != 1 || protocol.Str(updates[0], "sessionUpdate") != protocol.UpdatePlan {
		t.Fatalf("plan update = %v", updates)
	}
	entries, _ := updates[0]["entries"].([]protocol.Message)
	if len(entries) != 2 || protocol.Str(entries[0], "content") != "step one" {
		t.Errorf("plan entries = %v", entries)
	}
}

func TestTranslatorRecordsNonRetryableError(t *testing.T) {
	translator := newTranslator()
	updates := translator.translate("error", protocol.Message{
		"error": protocol.Message{"message": "context window exceeded"}, "willRetry": false,
	})
	if len(updates) != 1 || translator.lastErrorText() != "context window exceeded" {
		t.Fatalf("error translation = %v (lastError=%q)", updates, translator.lastErrorText())
	}
	if got := translator.translate("error", protocol.Message{
		"error": protocol.Message{"message": "transient"}, "willRetry": true,
	}); len(got) != 0 {
		t.Errorf("retryable errors should not enter the transcript: %v", got)
	}
}

func TestDecisionMapping(t *testing.T) {
	cases := []struct {
		method   string
		optionID string
		want     string
	}{
		{MethodCommandApproval, protocol.PermissionAllowOnce, "accept"},
		{MethodCommandApproval, protocol.PermissionAllowAlways, "acceptForSession"},
		{MethodCommandApproval, protocol.PermissionRejectOnce, "decline"},
		{MethodCommandApproval, protocol.PermissionRejectAlways, "cancel"},
		{MethodLegacyExecApproval, protocol.PermissionAllowOnce, "approved"},
		{MethodLegacyPatchApproval, protocol.PermissionAllowAlways, "approved_for_session"},
		{MethodLegacyExecApproval, protocol.PermissionRejectAlways, "abort"},
	}
	for _, tc := range cases {
		if got := decisionFor(tc.method, tc.optionID); got != tc.want {
			t.Errorf("decisionFor(%s, %s) = %q, want %q", tc.method, tc.optionID, got, tc.want)
		}
	}
	if fallbackDecision(MethodCommandApproval, "accept") != "accept" {
		t.Error("accept fallback")
	}
	if fallbackDecision(MethodCommandApproval, "anything") != "decline" {
		t.Error("default fallback must be decline")
	}
}

func TestMCPConfigHTTPAndSSE(t *testing.T) {
	config := mcpConfig([]any{
		protocol.Message{
			"name": "remote", "type": "http", "url": "https://example.test/mcp",
			"headers": []any{protocol.Message{"name": "Authorization", "value": "Bearer x"}},
		},
	})
	servers := protocol.Sub(config, "mcp_servers")
	remote := protocol.Sub(servers, "remote")
	if protocol.Str(remote, "url") != "https://example.test/mcp" {
		t.Errorf("http MCP config = %v", remote)
	}
	if protocol.Str(protocol.Sub(remote, "http_headers"), "Authorization") != "Bearer x" {
		t.Errorf("headers = %v", remote["http_headers"])
	}
	if mcpConfig(nil) != nil {
		t.Error("empty mcpServers must not add a config override")
	}
}

func TestBlocksToUserInputCoversEmbeddedContext(t *testing.T) {
	inputs, err := blocksToUserInput([]any{
		protocol.Message{"type": "text", "text": "look"},
		protocol.Message{"type": "audio", "data": "AA==", "mimeType": "audio/wav"},
		protocol.Message{"type": "resource_link", "name": "spec", "uri": "file:///tmp/spec.md"},
		protocol.Message{"type": "resource", "resource": protocol.Message{"uri": "file:///tmp/x", "text": "body"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 4 {
		t.Fatalf("inputs = %v", inputs)
	}
	if protocol.Str(inputs[1], "url") != "data:audio/wav;base64,AA==" {
		t.Errorf("audio = %v", inputs[1])
	}
	if protocol.Str(inputs[2], "text") != "[resource spec] file:///tmp/spec.md" {
		t.Errorf("resource_link = %v", inputs[2])
	}
	if protocol.Str(inputs[3], "text") != "body" {
		t.Errorf("resource = %v", inputs[3])
	}
	if _, err := blocksToUserInput([]any{"not an object"}); err == nil {
		t.Error("malformed content block must fail")
	}
}

func TestModePolicyRoundTrip(t *testing.T) {
	for _, policy := range []string{"on-request", "never", "untrusted"} {
		if got := policyForMode(modeForPolicy(policy)); got != policy {
			t.Errorf("mode round trip for %q = %q", policy, got)
		}
	}
	if !validMode(ModeAuto) || validMode("bogus") {
		t.Error("validMode")
	}
}
