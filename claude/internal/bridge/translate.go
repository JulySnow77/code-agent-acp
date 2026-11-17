package bridge

import (
	"strings"
	"sync"

	"claude-acp-bridge/internal/protocol"
)

// Translator turns one Claude turn's stream-json events into ACP session/update
// payloads. It is stateful: tool_use blocks are keyed by tool id, and stream
// deltas are de-duplicated against the complete assistant blocks that follow.
type Translator struct {
	mu         sync.Mutex
	seenTools  map[string]bool
	toolTitles map[string]string
	streamed   map[int]string // stream content-block index -> accumulated text (text/thinking)
	lastError  string
	usage      protocol.Message
}

func newTranslator() *Translator {
	return &Translator{
		seenTools:  make(map[string]bool),
		toolTitles: make(map[string]string),
		streamed:   make(map[int]string),
	}
}

// lastErrorText returns the most recent terminal error text.
func (t *Translator) lastErrorText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastError
}

// usageSnapshot returns the latest usage numbers (used/size) if seen.
func (t *Translator) usageSnapshot() protocol.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.usage == nil {
		return nil
	}
	out := protocol.Message{}
	for key, value := range t.usage {
		out[key] = value
	}
	return out
}

// translate returns 0..n ACP update payloads for one ordinary stream-json
// event. Control requests and result events are handled by the notify layer,
// not here.
func (t *Translator) translate(message protocol.Message) []protocol.Message {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch protocol.Str(message, "type") {
	case protocol.ClaudeTypeStreamEvent:
		return t.translateStream(protocol.Sub(message, "event"))

	case protocol.ClaudeTypeAssistant:
		return t.translateAssistant(protocol.Sub(message, "message"))

	case protocol.ClaudeTypeUser:
		return t.translateUser(protocol.Sub(message, "message"))

	case protocol.ClaudeTypeSystem:
		// system/status and system/thinking_tokens drive UI hints but carry no
		// transcript content; ignore them (and system/init, handled by Client).
		return nil

	case protocol.ClaudeTypeRateLimit, protocol.ClaudeTypeTaskStarted,
		protocol.ClaudeTypeTaskUpdated, protocol.ClaudeTypeTaskNotif:
		return nil
	}
	return nil
}

// translateStream handles Anthropic streaming passthrough (only present with
// --include-partial-messages). We emit text/thinking deltas live and remember
// the accumulated text so the complete assistant block is not emitted twice.
func (t *Translator) translateStream(event protocol.Message) []protocol.Message {
	if event == nil {
		return nil
	}
	switch protocol.Str(event, "type") {
	case protocol.ClaudeStreamContentBlockStart:
		block := protocol.Sub(event, "content_block")
		if protocol.Str(block, "type") == protocol.ClaudeBlockToolUse {
			toolID := protocol.Str(block, "id")
			if toolID != "" && !t.seenTools[toolID] {
				t.seenTools[toolID] = true
				name := protocol.Str(block, "name")
				t.toolTitles[toolID] = name
				return []protocol.Message{toolCallUpdate(toolID, name, nil,
					protocol.ToolStatusPending, "")}
			}
		}
		return nil

	case protocol.ClaudeStreamContentBlockDelta:
		index := int(protocol.Num(event, "index"))
		delta := protocol.Sub(event, "delta")
		switch protocol.Str(delta, "type") {
		case protocol.ClaudeDeltaText:
			text := protocol.Str(delta, "text")
			t.streamed[index] += text
			return []protocol.Message{textUpdate(protocol.UpdateAgentMessageChunk, text)}
		case protocol.ClaudeDeltaThinking:
			text := protocol.Str(delta, "thinking")
			t.streamed[index] += text
			return []protocol.Message{textUpdate(protocol.UpdateAgentThoughtChunk, text)}
		default:
			return nil
		}
	}
	return nil
}

// translateAssistant consumes a complete assistant message. Its content is an
// array with (usually) one block: text, thinking or tool_use.
func (t *Translator) translateAssistant(message protocol.Message) []protocol.Message {
	if message == nil {
		return nil
	}
	if usage := protocol.Sub(message, "usage"); usage != nil {
		t.captureUsage(usage)
	}
	out := make([]protocol.Message, 0, 2)
	for index, raw := range protocol.Raw(message, "content") {
		block := protocol.AsMessage(raw)
		if block == nil {
			continue
		}
		switch protocol.Str(block, "type") {
		case protocol.ClaudeBlockText:
			text := protocol.Str(block, "text")
			if text == "" || t.streamed[index] != "" {
				continue // already delivered via stream deltas
			}
			out = append(out, textUpdate(protocol.UpdateAgentMessageChunk, text))

		case protocol.ClaudeBlockThinking:
			text := protocol.Str(block, "thinking")
			if text == "" || t.streamed[index] != "" {
				continue
			}
			out = append(out, textUpdate(protocol.UpdateAgentThoughtChunk, text))

		case protocol.ClaudeBlockToolUse:
			toolID := protocol.Str(block, "id")
			if toolID == "" {
				continue
			}
			name := protocol.Str(block, "name")
			t.toolTitles[toolID] = name
			if t.seenTools[toolID] {
				// Input JSON deltas may have arrived earlier; send a final
				// in_progress update carrying the complete input.
				out = append(out, toolCallUpdate(toolID, name, block["input"],
					protocol.ToolStatusInProgress, ""))
				continue
			}
			t.seenTools[toolID] = true
			out = append(out, toolCallUpdate(toolID, name, block["input"],
				protocol.ToolStatusPending, ""))
		}
	}
	return out
}

// translateUser consumes a user message that carries tool_result blocks (tool
// output fed back to the model). These close the matching ACP tool calls.
func (t *Translator) translateUser(message protocol.Message) []protocol.Message {
	if message == nil {
		return nil
	}
	out := make([]protocol.Message, 0, 2)
	for _, raw := range protocol.Raw(message, "content") {
		block := protocol.AsMessage(raw)
		if block == nil || protocol.Str(block, "type") != protocol.ClaudeBlockToolRes {
			continue
		}
		toolID := protocol.Str(block, "tool_use_id")
		if toolID == "" {
			continue
		}
		status := protocol.ToolStatusCompleted
		if protocol.Bool(block, "is_error") {
			status = protocol.ToolStatusFailed
		}
		title := t.toolTitles[toolID]
		if !t.seenTools[toolID] {
			t.seenTools[toolID] = true
			out = append(out, toolCallUpdate(toolID, title, nil,
				protocol.ToolStatusInProgress, ""))
		}
		out = append(out, toolCallUpdate(toolID, title, nil, status,
			truncate(toolResultText(block["content"]), 8000)))
	}
	return out
}

func toolResultText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	for _, raw := range protocol.AsSlice(value) {
		block := protocol.AsMessage(raw)
		if block != nil && protocol.Str(block, "type") == "text" {
			return protocol.Str(block, "text")
		}
	}
	return protocol.StringValue(value)
}

// captureUsage folds Claude's usage object into an ACP usage_update. Context
// window size comes from the final result event's modelUsage when available.
func (t *Translator) captureUsage(usage protocol.Message) {
	input := protocol.Num(usage, "input_tokens")
	output := protocol.Num(usage, "output_tokens")
	cacheRead := protocol.Num(usage, "cache_read_input_tokens")
	cacheWrite := protocol.Num(usage, "cache_creation_input_tokens")
	used := input + output + cacheRead + cacheWrite
	if used > 0 {
		t.usage = protocol.Message{"used": int64(used)}
	}
}

// captureResultUsage reads the final result event, which carries the model's
// contextWindow for a meaningful size figure.
func (t *Translator) captureResultUsage(result protocol.Message) {
	if usage := protocol.Sub(result, "usage"); usage != nil {
		t.captureUsage(usage)
	}
	// modelUsage is an object keyed by model id (not an array).
	size := float64(0)
	if models := protocol.AsMessage(result["modelUsage"]); models != nil {
		for _, value := range models {
			if window := protocol.Num(protocol.AsMessage(value), "contextWindow"); window > size {
				size = window
			}
		}
	}
	if t.usage != nil && size > 0 {
		t.usage["size"] = int64(size)
	}
}

// recordError remembers a terminal error message for the transcript.
func (t *Translator) recordError(text string) {
	if text != "" {
		t.lastError = text
	}
}

// toolCallUpdate builds a tool_call (title/kind set) or tool_call_update.
func toolCallUpdate(toolID, name string, input any, status string, output string) protocol.Message {
	update := protocol.Message{
		"sessionUpdate": protocol.UpdateToolCallUpdate,
		"toolCallId":    toolID,
		"status":        status,
	}
	if title := toolTitle(name); title != "" {
		update["title"] = title
	}
	if kind := toolKind(name); kind != "" {
		update["kind"] = kind
	}
	if input != nil {
		update["rawInput"] = input
	}
	if output != "" {
		update["rawOutput"] = truncate(output, 8000)
	}
	return update
}

func toolTitle(name string) string {
	if name == "" {
		return "claude tool"
	}
	return truncate(name, 160)
}

// toolKind maps Claude's built-in tool names onto ACP ToolKind values.
func toolKind(name string) string {
	switch name {
	case "Read", "Glob", "Grep", "NotebookRead":
		return protocol.ToolKindRead
	case "Edit", "Write", "NotebookEdit":
		return protocol.ToolKindEdit
	case "Bash", "PowerShell", "KillShell":
		return protocol.ToolKindExecute
	case "WebFetch", "WebSearch":
		return protocol.ToolKindFetch
	case "TodoWrite", "Task", "TaskOutput", "Agent":
		return protocol.ToolKindOther
	default:
		return protocol.ToolKindOther
	}
}

func textUpdate(kind, text string) protocol.Message {
	return protocol.Message{
		"sessionUpdate": kind,
		"content":       protocol.Message{"type": "text", "text": text},
	}
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// firstNonEmpty is used by permission + history code in this package.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
