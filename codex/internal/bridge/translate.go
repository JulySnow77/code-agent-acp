package bridge

import (
	"strings"
	"sync"

	"codex-acp-bridge/internal/protocol"
)

// Translator turns one Codex turn's notification stream into ACP
// session/update payloads. It is stateful: tool items are keyed by itemId and
// plan text is accumulated until the plan item completes.
type Translator struct {
	mu         sync.Mutex
	seenTools  map[string]bool
	toolTitles map[string]string
	plan       strings.Builder
	lastError  string
	unknown    int
}

func newTranslator() *Translator {
	return &Translator{
		seenTools:  make(map[string]bool),
		toolTitles: make(map[string]string),
	}
}

// lastErrorText returns the most recent non-retryable app-server error.
func (t *Translator) lastErrorText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastError
}

// translate returns 0..n ACP update payloads for one notification.
func (t *Translator) translate(method string, params protocol.Message) []protocol.Message {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch method {
	case "item/agentMessage/delta":
		return []protocol.Message{textUpdate(protocol.UpdateAgentMessageChunk, protocol.Str(params, "delta"))}

	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		return []protocol.Message{textUpdate(protocol.UpdateAgentThoughtChunk, protocol.Str(params, "delta"))}

	case "item/plan/delta":
		t.plan.WriteString(protocol.Str(params, "delta"))
		return nil

	case "item/commandExecution/outputDelta", "command/exec/outputDelta",
		"item/fileChange/outputDelta", "item/mcpToolCall/progress":
		itemID := protocol.Str(params, "itemId")
		delta := protocol.Str(params, "delta")
		if delta == "" {
			delta = protocol.StringValue(params["progress"])
		}
		if itemID == "" || delta == "" {
			return nil
		}
		out := []protocol.Message{}
		out = append(out, t.ensureTool(itemID)...)
		out = append(out, protocol.Message{
			"sessionUpdate": protocol.UpdateToolCallUpdate,
			"toolCallId":    itemID,
			"status":        protocol.ToolStatusInProgress,
			"rawOutput":     truncate(delta, 4000),
		})
		return out

	case "item/started":
		return t.startItem(protocol.Sub(params, "item"))

	case "item/completed":
		return t.completeItem(protocol.Sub(params, "item"))

	case "turn/plan/updated":
		return []protocol.Message{t.planUpdate(planFromTurnParams(params))}

	case "thread/tokenUsage/updated":
		return usageUpdate(params)

	case "error":
		errObject := protocol.Sub(params, "error")
		text := protocol.Str(errObject, "message")
		if text == "" {
			text = protocol.Str(params, "message")
		}
		if !protocol.Bool(params, "willRetry") && text != "" {
			t.lastError = text
			return []protocol.Message{textUpdate(protocol.UpdateAgentMessageChunk, "Codex error: "+text)}
		}
		return nil

	case "warning":
		return nil

	case "turn/started", "thread/started", "turn/diff/updated",
		"item/reasoning/summaryPartAdded", "item/autoApprovalReview/started",
		"item/autoApprovalReview/completed", "thread/status/changed",
		"thread/readState/changed", "server/request/resolved":
		return nil
	}
	t.unknown++
	return nil
}

// startItem maps a ThreadItem from item/started to an ACP tool_call.
// Agent messages / reasoning / plans are streamed via deltas, not item frames.
func (t *Translator) startItem(item protocol.Message) []protocol.Message {
	itemType := protocol.Str(item, "type")
	itemID := protocol.Str(item, "id")
	switch itemType {
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
	default:
		return nil
	}
	if itemID == "" || t.seenTools[itemID] {
		return nil
	}
	t.seenTools[itemID] = true
	out := toolCallForItem(item)
	t.toolTitles[itemID] = protocol.Str(out, "title")
	return []protocol.Message{out}
}

// completeItem closes a tool call (or emits an accumulated plan).
func (t *Translator) completeItem(item protocol.Message) []protocol.Message {
	itemID := protocol.Str(item, "id")
	switch protocol.Str(item, "type") {
	case "plan":
		if t.plan.Len() == 0 {
			t.plan.WriteString(protocol.Str(item, "text"))
		}
		update := t.planUpdate(t.plan.String())
		t.plan.Reset()
		if update == nil {
			return nil
		}
		return []protocol.Message{update}
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
		if itemID == "" {
			return nil
		}
		out := []protocol.Message{}
		if !t.seenTools[itemID] {
			t.seenTools[itemID] = true
			out = append(out, toolCallForItem(item))
		}
		status, output := itemOutcome(item)
		out = append(out, protocol.Message{
			"sessionUpdate": protocol.UpdateToolCallUpdate,
			"toolCallId":    itemID,
			"status":        status,
			"rawOutput":     truncate(output, 8000),
		})
		return out
	}
	return nil
}

// ensureTool creates a synthetic tool_call if an output delta arrives without
// a preceding item/started (older app-server builds do this).
func (t *Translator) ensureTool(itemID string) []protocol.Message {
	if t.seenTools[itemID] {
		return nil
	}
	t.seenTools[itemID] = true
	t.toolTitles[itemID] = "codex tool"
	return []protocol.Message{{
		"sessionUpdate": protocol.UpdateToolCall,
		"toolCallId":    itemID,
		"title":         t.toolTitles[itemID],
		"kind":          protocol.ToolKindOther,
		"status":        protocol.ToolStatusInProgress,
	}}
}

func (t *Translator) planUpdate(text string) protocol.Message {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	entries := make([]protocol.Message, 0, 8)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entries = append(entries, protocol.Message{
			"content":  strings.TrimLeft(line, "-*0123456789. "),
			"priority": "medium",
			"status":   "pending",
		})
	}
	if len(entries) == 0 {
		return nil
	}
	return protocol.Message{
		"sessionUpdate": protocol.UpdatePlan,
		"entries":       entries,
	}
}

func planFromTurnParams(params protocol.Message) string {
	if plan := protocol.Sub(params, "plan"); plan != nil {
		if text := protocol.Str(plan, "text"); text != "" {
			return text
		}
		if explanation := protocol.Str(params, "explanation"); explanation != "" {
			return explanation
		}
	}
	return protocol.Str(params, "text")
}

func usageUpdate(params protocol.Message) []protocol.Message {
	usage := protocol.Sub(params, "tokenUsage")
	if usage == nil {
		return nil
	}
	total := protocol.Sub(usage, "total")
	used := protocol.Num(total, "totalTokens")
	size := protocol.Num(usage, "modelContextWindow")
	if used <= 0 && size <= 0 {
		return nil
	}
	return []protocol.Message{{
		"sessionUpdate": protocol.UpdateUsage,
		"used":          int64(used),
		"size":          int64(size),
	}}
}

func toolCallForItem(item protocol.Message) protocol.Message {
	itemType := protocol.Str(item, "type")
	call := protocol.Message{
		"sessionUpdate": protocol.UpdateToolCall,
		"toolCallId":    protocol.Str(item, "id"),
		"status":        protocol.ToolStatusPending,
	}
	switch itemType {
	case "commandExecution":
		call["title"] = truncate(firstNonEmpty(protocol.Str(item, "command"), "command"), 160)
		call["kind"] = protocol.ToolKindExecute
		call["rawInput"] = protocol.Message{
			"command": protocol.Str(item, "command"),
			"cwd":     protocol.StringValue(item["cwd"]),
		}
	case "fileChange":
		call["title"] = "apply_patch"
		call["kind"] = protocol.ToolKindEdit
		call["rawInput"] = protocol.Message{"changes": protocol.Raw(item, "changes")}
	case "mcpToolCall":
		call["title"] = truncate(protocol.Str(item, "server")+"/"+protocol.Str(item, "tool"), 160)
		call["kind"] = protocol.ToolKindOther
		call["rawInput"] = item["arguments"]
	case "dynamicToolCall":
		call["title"] = protocol.Str(item, "tool")
		call["kind"] = protocol.ToolKindOther
		call["rawInput"] = item["arguments"]
	default:
		call["title"] = itemType
		call["kind"] = protocol.ToolKindOther
	}
	return call
}

func itemOutcome(item protocol.Message) (string, string) {
	status := protocol.ToolStatusCompleted
	output := ""
	switch protocol.Str(item, "type") {
	case "commandExecution":
		output = protocol.Str(item, "aggregatedOutput")
		if code := protocol.Num(item, "exitCode"); code != 0 {
			status = protocol.ToolStatusFailed
		}
		if itemStatus := protocol.StringValue(item["status"]); strings.Contains(itemStatus, "failed") {
			status = protocol.ToolStatusFailed
		}
	case "fileChange":
		output = protocol.StringValue(item["changes"])
		if itemStatus := protocol.StringValue(item["status"]); strings.Contains(itemStatus, "failed") {
			status = protocol.ToolStatusFailed
		}
	case "mcpToolCall", "dynamicToolCall":
		output = protocol.StringValue(item["result"])
		if errObject := item["error"]; errObject != nil {
			status = protocol.ToolStatusFailed
			output = protocol.StringValue(errObject)
		}
	}
	return status, output
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
