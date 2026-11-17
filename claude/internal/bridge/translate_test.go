package bridge

import (
	"testing"

	"claude-acp-bridge/internal/protocol"
)

func TestTranslatorMapsAssistantUserAndUsage(t *testing.T) {
	tr := newTranslator()
	var updates []protocol.Message
	events := []protocol.Message{
		{"type": "assistant", "message": protocol.Message{
			"role": "assistant", "content": []any{protocol.Message{"type": "text", "text": "hi"}},
			"usage": protocol.Message{"input_tokens": 4, "output_tokens": 3,
				"cache_read_input_tokens": 5, "cache_creation_input_tokens": 0},
		}},
		{"type": "assistant", "message": protocol.Message{
			"role": "assistant", "content": []any{protocol.Message{
				"type": "thinking", "thinking": "hmm", "signature": "sig",
			}},
		}},
		{"type": "assistant", "message": protocol.Message{
			"role": "assistant", "content": []any{protocol.Message{
				"type": "tool_use", "id": "toolu-1", "name": "Edit",
				"input": protocol.Message{"file_path": "/tmp/x"},
			}},
		}},
		{"type": "user", "message": protocol.Message{
			"role": "user", "content": []any{protocol.Message{
				"type": "tool_result", "tool_use_id": "toolu-1", "content": "ok", "is_error": false,
			}},
		}},
	}
	for _, event := range events {
		updates = append(updates, tr.translate(event)...)
	}
	if len(updates) != 4 {
		t.Fatalf("got %d updates, want 4: %v", len(updates), updates)
	}
	assertKind := func(index int, want string) {
		t.Helper()
		if got := protocol.Str(updates[index], "sessionUpdate"); got != want {
			t.Errorf("update[%d] = %q, want %q", index, got, want)
		}
	}
	assertKind(0, protocol.UpdateAgentMessageChunk)
	assertKind(1, protocol.UpdateAgentThoughtChunk)
	assertKind(2, protocol.UpdateToolCallUpdate)
	assertKind(3, protocol.UpdateToolCallUpdate)
	if protocol.Str(updates[2], "kind") != protocol.ToolKindEdit {
		t.Errorf("tool kind = %v", updates[2]["kind"])
	}
	if protocol.Str(updates[3], "status") != protocol.ToolStatusCompleted {
		t.Errorf("final tool status = %v", updates[3]["status"])
	}
	if usage := tr.usageSnapshot(); protocol.Num(usage, "used") != 12 {
		t.Errorf("usage = %v", usage)
	}
}

func TestTranslatorStreamDeltasAndDedup(t *testing.T) {
	tr := newTranslator()
	updates := tr.translate(protocol.Message{
		"type": "stream_event", "event": protocol.Message{
			"type": "content_block_delta", "index": 0,
			"delta": protocol.Message{"type": "text_delta", "text": "live"},
		},
	})
	if len(updates) != 1 || protocol.Str(updates[0], "sessionUpdate") != protocol.UpdateAgentMessageChunk {
		t.Fatalf("delta update = %v", updates)
	}
	// The complete assistant block must be suppressed (already streamed).
	updates = tr.translate(protocol.Message{
		"type": "assistant", "message": protocol.Message{
			"role":    "assistant",
			"content": []any{protocol.Message{"type": "text", "text": "live"}},
		},
	})
	if len(updates) != 0 {
		t.Errorf("assistant block should be deduplicated: %v", updates)
	}
}

func TestTranslatorToolUseFromStreamStart(t *testing.T) {
	tr := newTranslator()
	updates := tr.translate(protocol.Message{
		"type": "stream_event", "event": protocol.Message{
			"type": "content_block_start",
			"content_block": protocol.Message{
				"type": "tool_use", "id": "toolu-2", "name": "Bash",
			},
		},
	})
	if len(updates) != 1 || protocol.Str(updates[0], "toolCallId") != "toolu-2" {
		t.Fatalf("tool start update = %v", updates)
	}
	if protocol.Str(updates[0], "kind") != protocol.ToolKindExecute {
		t.Errorf("tool kind = %v", updates[0]["kind"])
	}
}

func TestToolKindMapping(t *testing.T) {
	for name, want := range map[string]string{
		"Read":     protocol.ToolKindRead,
		"Edit":     protocol.ToolKindEdit,
		"Bash":     protocol.ToolKindExecute,
		"WebFetch": protocol.ToolKindFetch,
		"Custom":   protocol.ToolKindOther,
	} {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	if got := stripANSI("\x1b[31mred\x1b[0m text"); got != "red text" {
		t.Errorf("stripANSI = %q", got)
	}
}
