package bridge

import (
	"testing"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

func ev(kind string, payload map[string]any) zcode.Event {
	return zcode.Event{Type: kind, Payload: payload, Seq: 1}
}

// TestTranslateStreaming 覆盖 model.streaming → chunk 的映射与字段兼容
// (0.16 真机用 delta; 上游 fake 用 text)。
func TestTranslateStreaming(t *testing.T) {
	tr := NewTranslator()

	got := tr.Translate(ev("model.streaming", map[string]any{"kind": "text_delta", "delta": "你好"}))
	if len(got) != 1 || got[0]["sessionUpdate"] != "agent_message_chunk" {
		t.Fatalf("text_delta 翻译异常: %v", got)
	}
	if content := got[0]["content"].(map[string]any); content["text"] != "你好" {
		t.Errorf("chunk 文本 = %v", content)
	}

	got = tr.Translate(ev("model.streaming", map[string]any{"kind": "reasoning_delta", "delta": "思考"}))
	if len(got) != 1 || got[0]["sessionUpdate"] != "agent_thought_chunk" {
		t.Fatalf("reasoning_delta 翻译异常: %v", got)
	}

	// fake 兼容: text 字段。
	got = tr.Translate(ev("model.streaming", map[string]any{"kind": "text_delta", "text": "hello"}))
	if len(got) != 1 {
		t.Fatalf("text 字段兼容失效: %v", got)
	}

	// 工具输入流 kind: 刻意忽略 (工具生命周期由 tool.updated 承载)。
	if got := tr.Translate(ev("model.streaming", map[string]any{"kind": "tool_input_delta", "delta": "x"})); len(got) != 0 {
		t.Errorf("tool_input_* 应忽略: %v", got)
	}
	// 空增量不发帧。
	if got := tr.Translate(ev("model.streaming", map[string]any{"kind": "text_delta", "delta": ""})); len(got) != 0 {
		t.Errorf("空增量应不发帧: %v", got)
	}
}

// TestTranslateTool 覆盖 tool.updated 六种 kind。
func TestTranslateTool(t *testing.T) {
	tr := NewTranslator()

	got := tr.Translate(ev("tool.updated", map[string]any{"kind": "scheduled", "toolCallId": "c1", "toolName": "Bash"}))
	if len(got) != 1 || got[0]["sessionUpdate"] != "tool_call" || got[0]["kind"] != "execute" || got[0]["status"] != "pending" {
		t.Fatalf("scheduled 翻译异常: %v", got)
	}
	// 重复 scheduled 去重。
	if got := tr.Translate(ev("tool.updated", map[string]any{"kind": "scheduled", "toolCallId": "c1", "toolName": "Bash"})); len(got) != 0 {
		t.Errorf("重复 scheduled 应去重: %v", got)
	}

	got = tr.Translate(ev("tool.updated", map[string]any{"kind": "started", "toolCallId": "c1"}))
	if len(got) != 1 || got[0]["status"] != "in_progress" {
		t.Fatalf("started 翻译异常: %v", got)
	}

	got = tr.Translate(ev("tool.updated", map[string]any{"kind": "progress", "toolCallId": "c1", "stdoutTail": "running…"}))
	if len(got) != 1 || got[0]["rawOutput"] != "running…" {
		t.Fatalf("progress 翻译异常: %v", got)
	}

	// result 对象形态 (0.16.1)。
	got = tr.Translate(ev("tool.updated", map[string]any{"kind": "result", "toolCallId": "c1",
		"result": map[string]any{"success": true, "content": "done"}}))
	if len(got) != 1 || got[0]["status"] != "completed" || got[0]["rawOutput"] != "done" {
		t.Fatalf("result 翻译异常: %v", got)
	}
	// result 失败形态。
	got = tr.Translate(ev("tool.updated", map[string]any{"kind": "result", "toolCallId": "c2",
		"result": map[string]any{"success": false, "error": "boom"}}))
	if len(got) != 1 || got[0]["status"] != "failed" || got[0]["rawOutput"] != "boom" {
		t.Fatalf("result 失败形态翻译异常: %v", got)
	}

	got = tr.Translate(ev("tool.updated", map[string]any{"kind": "error", "toolCallId": "c1", "error": "工具炸了"}))
	if len(got) != 1 || got[0]["status"] != "failed" {
		t.Fatalf("error 翻译异常: %v", got)
	}

	// batch: 只补发 scheduled 过的。
	tr2 := NewTranslator()
	tr2.Translate(ev("tool.updated", map[string]any{"kind": "scheduled", "toolCallId": "b1", "toolName": "Read"}))
	got = tr2.Translate(ev("tool.updated", map[string]any{"kind": "batch",
		"toolCallIds": []any{"b1", "ghost"}, "errorCount": 0}))
	if len(got) != 1 || got[0]["toolCallId"] != "b1" || got[0]["status"] != "completed" {
		t.Fatalf("batch 翻译异常: %v", got)
	}
}

// TestTranslateTurnLifecycle 覆盖终止帧语义 (上游 bb7ada5: turn.failed 无条件
// 判失败, 不编造 resultType)。
func TestTranslateTurnLifecycle(t *testing.T) {
	tr := NewTranslator()
	tr.ContextWindow = 200000

	if got := tr.Translate(ev("turn.started", map[string]any{})); len(got) != 0 {
		t.Errorf("turn.started 不应产出 update: %v", got)
	}
	if !tr.TurnStarted {
		t.Error("TurnStarted 未置位")
	}

	got := tr.Translate(ev("turn.completed", map[string]any{
		"resultType": "success",
		"usage":      map[string]any{"totalTokens": 12},
	}))
	if !tr.TurnDone || tr.ResultType != "success" {
		t.Fatalf("turn.completed 状态异常: %+v", tr)
	}
	if len(got) != 1 || got[0]["sessionUpdate"] != "usage_update" {
		t.Fatalf("usage 翻译异常: %v", got)
	}
	// usage 无 contextWindow → 用订阅快照兜底 (上游 bb7ada5)。
	if got[0]["size"] != int64(200000) || got[0]["used"] != int64(12) {
		t.Errorf("usage 数值异常: %v", got[0])
	}
}

func TestTranslateTurnFailed(t *testing.T) {
	tr := NewTranslator()
	got := tr.Translate(ev("turn.failed", map[string]any{
		"error": map[string]any{
			"code":        "model_request_failed",
			"message":     "Model request failed.",
			"attribution": map[string]any{"statusCode": 429.0, "retryable": true, "providerId": "p", "modelId": "m"},
		},
	}))
	if len(got) != 0 {
		t.Errorf("turn.failed 本身不产出 update (错误由 failTurn 推): %v", got)
	}
	if !tr.TurnDone || !tr.TurnFailed {
		t.Fatal("turn.failed 未标记终止")
	}
	if tr.TurnError != "Model request failed." {
		t.Errorf("TurnError = %q", tr.TurnError)
	}
	if tr.Attribution == nil || tr.Attribution["providerId"] != "p" {
		t.Errorf("attribution 未保留: %v", tr.Attribution)
	}

	// error 为字符串形态的防御。
	tr2 := NewTranslator()
	tr2.Translate(ev("turn.failed", map[string]any{"error": "plain failure"}))
	if tr2.TurnError != "plain failure" {
		t.Errorf("字符串 error 兜底失效: %q", tr2.TurnError)
	}
}

// TestTranslateUnknownIgnored 验证未知 type 忽略并计数 (§2.2 C7)。
func TestTranslateUnknownIgnored(t *testing.T) {
	tr := NewTranslator()
	tr.Translate(ev("state.revisionBumped", map[string]any{"x": 1}))
	tr.Translate(ev("rewind.triggered", map[string]any{}))
	if tr.UnknownCount() != 2 {
		t.Errorf("UnknownCount = %d, want 2", tr.UnknownCount())
	}
	if tr.TurnDone {
		t.Error("未知事件不应终结 turn")
	}
}
