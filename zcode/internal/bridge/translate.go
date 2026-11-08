// Package bridge 是 ACP 协议面: 会话映射、turn 状态机、事件翻译、扩展透传。
//
// 对应上游 ACPBridge (基线 3069013)。与上游的关键差异:
//   - 不移植 ProjectionDiffer 轮询降级 (PORTING_PLAN §1.2): 0.16+ 只走事件订阅。
//   - turn.failed 错误可见 (§8 D4): 推错误文本 + prompt 返回错误, 不再伪装 end_turn。
package bridge

import (
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// toolKindMap: zcode 工具名 → ACP ToolCallKind (上游 EventTranslator.TOOL_KIND_MAP)。
var toolKindMap = map[string]string{
	"Bash": "execute", "bash": "execute",
	"Edit": "edit", "Write": "edit", "edit": "edit", "write": "edit",
	"Read": "read", "read": "read",
	"Grep": "search", "grep": "search", "Glob": "search",
	"WebFetch": "fetch", "WebSearch": "fetch", "web_search": "fetch",
}

// Translator 把 zcode session/event 翻译成 ACP session/update 的 update 载荷。
//
// 状态化: 记录 turn 生命周期标记 (供 turn 循环判定结束) 与已见 toolCallId
// (scheduled 去重; result 帧不带 toolName)。一个 turn 一个实例。
type Translator struct {
	// TurnStarted: 见过 turn.started。
	TurnStarted bool
	// TurnDone: 见过 turn.completed 或 turn.failed (终止帧)。
	TurnDone bool
	// TurnFailed: 终止帧是 turn.failed。
	TurnFailed bool
	// TurnError: turn.failed 提取的错误文案 (message/type/code 兜底)。
	TurnError string
	// ResultType: turn.completed 的 resultType (success/cancelled/error_*)。
	ResultType string
	// Attribution: turn.failed payload.error.attribution 原样保留 (D4 的 error data)。
	Attribution map[string]any
	// ContextWindow: 订阅快照缓存的 projection.contextWindow, 兜底 usage size
	// (0.16.1 turn.completed.usage 无 contextWindow)。
	ContextWindow int

	seenTools  map[string]bool
	toolNames  map[string]string
	unknownCnt int // 未知 type 计数 (§2.2 C7: 忽略并计数)
}

// NewTranslator 构造翻译器。
func NewTranslator() *Translator {
	return &Translator{seenTools: map[string]bool{}, toolNames: map[string]string{}}
}

// UnknownCount 返回被忽略的未知事件类型数。
func (t *Translator) UnknownCount() int { return t.unknownCnt }

// Translate 翻译一条事件, 产出 0..n 条 update 载荷 (session/update 的 update 字段)。
func (t *Translator) Translate(ev zcode.Event) []map[string]any {
	var out []map[string]any
	payload := ev.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	switch ev.Kind() {
	case "turn.started":
		t.TurnStarted = true

	case "model.streaming":
		out = append(out, t.translateStreaming(payload)...)

	case "tool.updated":
		out = append(out, t.translateTool(payload)...)

	case "turn.completed":
		t.TurnDone = true
		t.ResultType = zcode.Str(payload, "resultType")
		if t.ResultType == "" {
			t.ResultType = "success"
		}
		out = append(out, t.translateUsage(payload)...)

	case "turn.failed":
		// 0.16+ 终止帧: 之后无 turn.completed, payload 无 resultType,
		// 无条件判失败 (上游 bb7ada5)。只取 message/type/code —— 不碰
		// detail/stack (可能含敏感回显)。
		t.TurnDone = true
		t.TurnFailed = true
		err := zcode.Sub(payload, "error")
		if err == nil {
			// 防御: error 为字符串等非对象形态时兜底。
			if s := zcode.Str(payload, "error"); s != "" && s != "{}" {
				err = map[string]any{"message": s}
			}
		}
		t.TurnError = firstNonEmpty(
			zcode.Str(err, "message"),
			zcode.Str(err, "type"),
			zcode.Str(err, "code"),
			"unknown error",
		)
		if attr := zcode.Sub(err, "attribution"); attr != nil {
			t.Attribution = attr
		}

	case "session.titleUpdated":
		// 标题更新不产出 ACP 事件 (上游 §4)。
	}
	// 其他 (session.updated / rewind.triggered / turn.steerQueued …): 忽略并计数。
	if ev.Kind() != "" && !t.knownKind(ev.Kind()) {
		t.unknownCnt++
	}
	return out
}

func (t *Translator) knownKind(kind string) bool {
	switch kind {
	case "turn.started", "model.streaming", "tool.updated", "turn.completed",
		"turn.failed", "session.titleUpdated", "session.updated":
		return true
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// translateStreaming: model.streaming → agent_message_chunk / agent_thought_chunk。
//
// payload: {kind: text_delta|reasoning_delta, delta}。0.16.1 还会出现
// tool_input_start/delta/end 与 tool_call (工具输入流): 刻意忽略 —— 工具生命
// 周期由 tool.updated 承载, 不重复映射。
func (t *Translator) translateStreaming(payload map[string]any) []map[string]any {
	kind := zcode.Str(payload, "kind")
	delta := zcode.Str(payload, "delta")
	// 兼容 fake (上游本地基线) 用 text 字段携带增量。
	if delta == "" {
		delta = zcode.Str(payload, "text")
	}
	if delta == "" {
		return nil
	}
	switch kind {
	case "text_delta":
		return []map[string]any{{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": delta},
		}}
	case "reasoning_delta":
		return []map[string]any{{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": delta},
		}}
	}
	return nil
}

// translateTool: tool.updated → tool_call / tool_call_update。
//
// payload.kind: scheduled|started|progress|result|error|batch。
func (t *Translator) translateTool(payload map[string]any) []map[string]any {
	kind := zcode.Str(payload, "kind")
	callID := zcode.Str(payload, "toolCallId")
	toolName := zcode.Str(payload, "toolName")

	switch kind {
	case "scheduled":
		if callID == "" || t.seenTools[callID] {
			return nil
		}
		t.seenTools[callID] = true
		t.toolNames[callID] = toolName
		acpKind := toolKindMap[toolName]
		if acpKind == "" {
			acpKind = "execute"
		}
		name := toolName
		if name == "" {
			name = "unknown"
		}
		return []map[string]any{{
			"sessionUpdate": "tool_call",
			"toolCallId":    callID,
			"title":         name,
			"kind":          acpKind,
			"status":        "pending",
		}}

	case "started":
		if callID == "" {
			return nil
		}
		return []map[string]any{{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    callID,
			"status":        "in_progress",
		}}

	case "progress":
		if callID == "" {
			return nil
		}
		// progress 帧含实时输出 (stdoutTail/stderrTail)。
		output := firstNonEmpty(
			zcode.Str(payload, "stdoutTail"),
			zcode.Str(payload, "stderrTail"),
		)
		return []map[string]any{{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    callID,
			"status":        "in_progress",
			"rawOutput":     output,
		}}

	case "result":
		if callID == "" {
			return nil
		}
		// 0.16.1 result 为对象 {success, content, …}; 0.15 为字符串。
		status := "completed"
		output := ""
		if raw := payload["result"]; raw != nil {
			if m, ok := raw.(map[string]any); ok {
				output = firstNonEmpty(zcode.Str(m, "content"), zcode.Str(m, "error"))
				if ok, _ := m["success"].(bool); !ok {
					status = "failed"
				}
			} else {
				output = zcode.Str(payload, "result")
			}
		}
		if len(output) > 2000 {
			output = output[:2000]
		}
		return []map[string]any{{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    callID,
			"status":        status,
			"rawOutput":     output,
		}}

	case "error":
		if callID == "" {
			return nil
		}
		return []map[string]any{{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    callID,
			"status":        "failed",
			"rawOutput":     zcode.Str(payload, "error"),
		}}

	case "batch":
		// 多个工具一起完成。zcode 通常也逐个发 result, 这里只补发 scheduled
		// 过但未收到 result 的, 防客户端永远卡 in_progress。
		var out []map[string]any
		errCount := int(zcode.Num(payload, "errorCount"))
		for _, rawID := range zcode.Slice(payload, "toolCallIds") {
			bid, _ := rawID.(string)
			if bid == "" || !t.seenTools[bid] {
				continue
			}
			status := "completed"
			if errCount > 0 {
				status = "failed"
			}
			out = append(out, map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    bid,
				"status":        status,
			})
		}
		return out
	}
	return nil
}

// translateUsage: turn.completed.usage → usage_update。
func (t *Translator) translateUsage(payload map[string]any) []map[string]any {
	usage := zcode.Sub(payload, "usage")
	used := 0.0
	size := 0.0
	if usage != nil {
		used = zcode.Num(usage, "totalTokens")
		size = zcode.Num(usage, "contextWindow")
	}
	if used == 0 {
		used = zcode.Num(payload, "tokenCount")
	}
	if size == 0 {
		size = float64(t.ContextWindow)
	}
	if size <= 0 && used <= 0 {
		return nil
	}
	return []map[string]any{{
		"sessionUpdate": "usage_update",
		"used":          int64(used),
		"size":          int64(size),
	}}
}
