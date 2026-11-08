package e2e

import (
	"testing"
	"time"
)

// flow-questions v1 (T003) 的桥端到端契约: 声明了 flowQuestions 的客户端**不再**
// 收到 elicitation/create, 改为收到 _multica/flow/question 通知 —— 题型
// (single/multi/text/自定义文本)、边界 (min/max) 与稳定问题 id 都在通知里无损
// 投影; 答案经 _multica/flow/answer 回填 zcode 的原生调用。样本与 JulyBoard 仓
// docs/julyboard/flow-question-protocol.md 一致。

// flowQuestionsCaps: 声明 elicitation.form + _meta.multica.flowQuestions=v1。
func flowQuestionsCaps() map[string]any {
	return map[string]any{
		"clientCapabilities": map[string]any{
			"elicitation": map[string]any{"form": map[string]any{}},
			"_meta":       map[string]any{"multica": map[string]any{"flowQuestions": "v1"}},
		},
	}
}

var flowStorageQuestion = map[string]any{
	"question":        "使用哪种存储？",
	"header":          "Storage",
	"allowCustomText": true,
	"options": []any{
		map[string]any{"label": "数据库", "value": "db"},
		map[string]any{"label": "任务目录", "value": "files"},
	},
}

var flowConstraintQuestion = map[string]any{
	"question": "需要保留哪些约束？",
	"header":   "Constraints",
}

var flowPlatformsQuestion = map[string]any{
	"question":        "支持哪些平台？",
	"header":          "Platforms",
	"multiSelect":     true,
	"minSelections":   1,
	"maxSelections":   2,
	"allowCustomText": true,
	"options": []any{
		map[string]any{"label": "iOS", "value": "ios"},
		map[string]any{"label": "Android", "value": "android"},
		map[string]any{"label": "Web", "value": "web"},
	},
}

// flowQuestionEntry 从 _multica/flow/question 通知里取指定 question_id 的题目,
// 取不到直接 Fatal —— 缺题属于契约破裂, 不该静默继续。
func flowQuestionEntry(t *testing.T, question map[string]any, want int) []map[string]any {
	t.Helper()
	raw, _ := question["questions"].([]any)
	if len(raw) != want {
		t.Fatalf("questions = %d, want %d (%v)", len(raw), want, question["questions"])
	}
	out := make([]map[string]any, 0, want)
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		if entry == nil {
			t.Fatalf("question 条目不是对象: %v", item)
		}
		out = append(out, entry)
	}
	return out
}

// answerFlow 发一条 _multica/flow/answer 并断言被受理 (accepted=true)。
func answerFlow(t *testing.T, c *acpClient, interactionID string, content map[string]any) {
	t.Helper()
	resp := c.request(4, "_multica/flow/answer", map[string]any{
		"interaction_id":  interactionID,
		"dispatch_id":     "d-e2e",
		"consumption_id":  "c-e2e",
		"answer_revision": "1",
		"content":         content,
	})
	if resp["error"] != nil {
		t.Fatalf("_multica/flow/answer 报错: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	if result["accepted"] != true {
		t.Fatalf("_multica/flow/answer 结果 = %v, want accepted=true", result)
	}
}

// TestFlowQuestionSameTurnSingleAndText: 同轮 single+text 通过 _multica/flow/question
// 无损投影 (题型、选项、稳定 id、allow_custom_text 齐备), 选项答案与自由文本答案都
// 按 zcode 原文回键; 整个问答期间不出现 elicitation/create。
func TestFlowQuestionSameTurnSingleAndText(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(flowStorageQuestion, flowConstraintQuestion), flowQuestionsCaps())

	question := waitAgentNotification(t, c, "_multica/flow/question", 10*time.Second)
	if id, _ := question["interaction_id"].(string); id == "" {
		t.Fatalf("interaction_id 缺失: %v", question)
	}
	qs := flowQuestionEntry(t, question, 2)

	single := qs[0]
	if single["question_id"] == "" || single["kind"] != "single" {
		t.Errorf("第一题 = %v, want question_id 非空 kind=single", single)
	}
	if single["allow_custom_text"] != true {
		t.Errorf("第一题 allow_custom_text = %v, want true", single["allow_custom_text"])
	}
	opts, _ := single["options"].([]any)
	if len(opts) != 2 {
		t.Errorf("第一题 options = %v, want 2", opts)
	}
	text := qs[1]
	if text["kind"] != "text" {
		t.Errorf("第二题 kind = %v, want text", text["kind"])
	}
	if _, has := text["options"]; has {
		t.Errorf("文本题不应带 options: %v", text)
	}

	// flow 协商取代了 elicitation: 全程不得再出现 elicitation/create。
	assertNoAgentRequest(t, c, "elicitation/create", 2*time.Second)

	answerFlow(t, c, question["interaction_id"].(string), map[string]any{
		"使用哪种存储？":   "db",
		"需要保留哪些约束？": "保留现有执行队列",
	})
	waitPromptOK(t, done)

	replies := askReplies(t, f, "interaction/requestUserInput", 1)
	answers := askAnswers(t, replies[0])
	if answers["使用哪种存储？"] != "db" {
		t.Errorf("answers[使用哪种存储？] = %v, want db", answers["使用哪种存储？"])
	}
	if answers["需要保留哪些约束？"] != "保留现有执行队列" {
		t.Errorf("answers[需要保留哪些约束？] = %v, want 保留现有执行队列", answers["需要保留哪些约束？"])
	}
}

// TestFlowQuestionMultiBoundsAndCustom: multi 的 min/max 由通知携带 (form schema
// 表达不了的边界), 双选按 boolean 集合回收后按数组回传 zcode。
func TestFlowQuestionMultiBoundsAndCustom(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(flowPlatformsQuestion), flowQuestionsCaps())

	question := waitAgentNotification(t, c, "_multica/flow/question", 10*time.Second)
	qs := flowQuestionEntry(t, question, 1)
	entry := qs[0]
	if entry["kind"] != "multi" || entry["min_selections"] != float64(1) || entry["max_selections"] != float64(2) {
		t.Errorf("multi 题 = %v, want kind=multi min=1 max=2", entry)
	}
	opts, _ := entry["options"].([]any)
	if len(opts) != 3 {
		t.Errorf("multi options = %d, want 3", len(opts))
	}

	answerFlow(t, c, question["interaction_id"].(string), map[string]any{
		"支持哪些平台？[ios]":     true,
		"支持哪些平台？[android]": true,
		"支持哪些平台？[web]":     false,
	})
	waitPromptOK(t, done)

	replies := askReplies(t, f, "interaction/requestUserInput", 1)
	got, _ := askAnswers(t, replies[0])["支持哪些平台？"].([]any)
	if len(got) != 2 || got[0] != "ios" || got[1] != "android" {
		t.Errorf("answers[支持哪些平台？] = %v, want [ios android]", got)
	}
}

// TestFlowQuestionCustomTextSingle: 单选自定义文本按原文回传给 zcode —— 选项题在
// 声明 allow_custom_text 后接受枚举外的非空文本。
func TestFlowQuestionCustomTextSingle(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(flowStorageQuestion), flowQuestionsCaps())

	question := waitAgentNotification(t, c, "_multica/flow/question", 10*time.Second)
	answerFlow(t, c, question["interaction_id"].(string), map[string]any{
		"使用哪种存储？": "对象存储",
	})
	waitPromptOK(t, done)

	replies := askReplies(t, f, "interaction/requestUserInput", 1)
	answers := askAnswers(t, replies[0])
	if answers["使用哪种存储？"] != "对象存储" {
		t.Errorf("answers[使用哪种存储？] = %v, want 对象存储 (自定义文本原文)", answers["使用哪种存储？"])
	}
}

// TestFlowQuestionIDStableAcrossReasks: 两个不同 requestId 复问同一道题, 两次通知的
// question_id 必须相同 —— daemon 侧据此把重报对齐到同一平台问题身份, 不产生第二份
// 待答问题。interaction_id 随 requestId 变, question_id 不变。
func TestFlowQuestionIDStableAcrossReasks(t *testing.T) {
	turnDone := []fakePush{{Method: "session/event", Params: map[string]any{
		"sessionId": "s-ask", "seq": 2, "type": "turn.completed",
		"payload": map[string]any{"resultType": "success"},
	}}}
	params := func(rid string) map[string]any {
		return map[string]any{
			"sessionId": "s-ask", "questions": []any{flowStorageQuestion},
			"requestId": rid, "toolCallId": "tc-" + rid,
		}
	}
	script := fakeScript{
		Responses: map[string]fakeAction{
			"session/create":    {Result: map[string]any{"session": map[string]any{"sessionId": "s-ask"}}},
			"session/subscribe": {Result: map[string]any{"eventSeq": 0, "snapshot": map[string]any{"status": "idle"}}},
			"session/messages":  {Result: map[string]any{"messages": []any{}}},
			"session/send": {
				Result: map[string]any{"accepted": true},
				ServerRequests: []fakeServerReq{
					{Method: "interaction/requestUserInput", Params: params("req-a")},
					{Method: "interaction/requestUserInput", Params: params("req-b"), Then: turnDone},
				},
			},
		},
	}
	_, c, done := startAskCaps(t, script, flowQuestionsCaps())

	first := waitAgentNotification(t, c, "_multica/flow/question", 10*time.Second)
	firstEntry := flowQuestionEntry(t, first, 1)[0]
	answerFlow(t, c, first["interaction_id"].(string), map[string]any{"使用哪种存储？": "db"})

	second := waitAgentNotification(t, c, "_multica/flow/question", 10*time.Second)
	secondEntry := flowQuestionEntry(t, second, 1)[0]
	answerFlow(t, c, second["interaction_id"].(string), map[string]any{"使用哪种存储？": "db"})

	if first["interaction_id"] == second["interaction_id"] {
		t.Errorf("不同 requestId 应产生不同 interaction_id: %v", first["interaction_id"])
	}
	if firstEntry["question_id"] == "" || firstEntry["question_id"] != secondEntry["question_id"] {
		t.Errorf("同题复问 question_id 不稳定: %v vs %v",
			firstEntry["question_id"], secondEntry["question_id"])
	}
	waitPromptOK(t, done)
}

// TestFlowQuestionNonFlowClientUnchanged: 未声明 flowQuestions 的客户端对 text 题
// 仍走 elicitation 路径, 投影不出合法 enum 时放弃 (不发明一片空白表), 直接回 cancel
// —— flow 能力不是隐式默认, 非 flow 路径逐字不变。
func TestFlowQuestionNonFlowClientUnchanged(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(flowConstraintQuestion), elicitationFormCaps())

	assertNoAgentRequest(t, c, "elicitation/create", 3*time.Second)
	assertNoAgentRequest(t, c, "_multica/flow/question", 1*time.Second)
	waitPromptOK(t, done)

	replies := askReplies(t, f, "interaction/requestUserInput", 1)
	if action := replies[0]["action"]; action != "cancel" {
		t.Errorf("非 flow 客户端的 text 题 action = %v, want cancel (无法投影)", action)
	}
}
