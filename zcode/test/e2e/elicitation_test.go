package e2e

import (
	"testing"
	"time"
)

// 协商路径: 客户端声明 `clientCapabilities.elicitation.form` → 桥用
// elicitation/create 一次问完整组问题, 普通问答不再伪装成工具授权。
//
// 能力声明的解析本身 (哪些形状算声明了 form) 是纯函数, 表驱动测试放在
// internal/bridge/elicitation_test.go; 这里只验「解析结果如何改变实际走哪条路」。

// TestElicitationNotAdvertisedUsesFallback: 不声明能力 → 走 permission, 且
// **绝不发** elicitation/create。ACP: 未声明的 capability 一律当不支持,
// 「Agents MUST NOT request a mode the Client has not advertised」。
func TestElicitationNotAdvertisedUsesFallback(t *testing.T) {
	for _, caps := range []map[string]any{
		{},                                       // 完全没有 clientCapabilities
		{"clientCapabilities": map[string]any{}}, // 有 clientCapabilities, 但没有 elicitation
		{"clientCapabilities": map[string]any{"elicitation": map[string]any{}}},            // 空对象 = 零模式 (与 MCP 不同!)
		{"clientCapabilities": map[string]any{"elicitation": map[string]any{"form": nil}}}, // form: null = 不支持
	} {
		f, c, done := startAskCaps(t, askScript(frameworkQuestion), caps)
		permID, _ := waitPermission(t, c, 10*time.Second)
		assertNoAgentRequest(t, c, "elicitation/create", 300*time.Millisecond)
		replyPermission(c, permID, "selected", "vue")
		waitPromptOK(t, done)
		if got := askAnswers(t, waitAskReply(t, f))["Which framework should we use?"]; got != "vue" {
			t.Errorf("caps=%v 走降级路径, 答案 = %v, want vue", caps, got)
		}
	}
}

// TestElicitationFormSchema: 声明 form 后, requestedSchema 的形状必须符合
// ElicitationSchema (扁平对象 + 原始类型 + enum), 且 **enum 取 value**。
func TestElicitationFormSchema(t *testing.T) {
	_, c, _ := startAskCaps(t, askScript(frameworkQuestion), elicitationFormCaps())

	elID, params := waitElicitation(t, c, 10*time.Second)
	if params["sessionId"] != "s-ask" {
		t.Errorf("sessionId = %v, want s-ask", params["sessionId"])
	}
	if params["mode"] != "form" {
		t.Errorf("mode = %v, want form", params["mode"])
	}
	if msg, _ := params["message"].(string); msg == "" {
		t.Errorf("message 不应为空")
	}
	schema, _ := params["requestedSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("requestedSchema.type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	prop, _ := props["Which framework should we use?"].(map[string]any)
	if prop == nil {
		t.Fatalf("property 名应为问题原文 (zcode 按原文取值), 实际 keys=%v", keysOf(props))
	}
	if prop["type"] != "string" {
		t.Errorf("单选 property.type = %v, want string", prop["type"])
	}
	if prop["title"] != "Framework" {
		t.Errorf("property.title = %v, want Framework (= header)", prop["title"])
	}
	if prop["description"] != "Which framework should we use?" {
		t.Errorf("property.description = %v, want 问题原文", prop["description"])
	}
	// 关键: enum 是 value, 不是 label。
	assertEnum(t, prop, []any{"react", "vue"})
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "Which framework should we use?" {
		t.Errorf("required = %v, want 全部 property 名", req)
	}
	// 展示名 (label) 走 _meta, 不塞进 property。
	meta, _ := params["_meta"].(map[string]any)
	zmeta, _ := meta["zcode"].(map[string]any)
	qs, _ := zmeta["questions"].([]any)
	if len(qs) != 1 {
		t.Fatalf("_meta.zcode.questions 长度 = %d, want 1", len(qs))
	}
	q0, _ := qs[0].(map[string]any)
	choices, _ := q0["options"].([]any)
	ch0, _ := choices[0].(map[string]any)
	if ch0["value"] != "react" || ch0["label"] != "React" {
		t.Errorf("_meta 里应同时给出 value 与 label, 实际 %v", ch0)
	}

	// 客户端按 value 应答。
	replyElicitation(c, elID, "accept", map[string]any{"Which framework should we use?": "vue"})
}

// TestElicitationPlanApprovalValue: **T03 的核心回归测试**。
//
// 复刻 zcode 真实的 plan 审批问题 (createExitPlanModeApprovalQuestion,
// interaction-broker.ts:326): 选项 label "Approve" / value "approve", 而
// planApprovalResponseToBrokerResult (:399) 拿答案与常量 "approve" 逐字比对。
//
// 旧实现 optionId=label 且回传 label → 用户点「批准」, zcode 收到 "Approve",
// 判为不是批准, 并把字面量当用户反馈喂回模型。旧 fixture 里 value==label,
// 测试根本看不出区别。
func TestElicitationPlanApprovalValue(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(planApprovalQuestion), elicitationFormCaps())

	elID, params := waitElicitation(t, c, 10*time.Second)
	schema, _ := params["requestedSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	prop, _ := props["Review this implementation plan."].(map[string]any)
	if prop == nil {
		t.Fatalf("property 名应为 EXIT_PLAN_MODE_APPROVAL_QUESTION 原文, keys=%v", keysOf(props))
	}
	// enum 必须是 "approve" —— 这是 zcode 拿去逐字比对的那个常量。
	assertEnum(t, prop, []any{"approve"})

	replyElicitation(c, elID, "accept", map[string]any{"Review this implementation plan.": "approve"})
	waitPromptOK(t, done)

	answers := askAnswers(t, waitAskReply(t, f))
	got := answers["Review this implementation plan."]
	if got != "approve" {
		t.Errorf("zcode 收到 %q, want \"approve\" (收到 \"Approve\" 会被判为未批准, "+
			"并把该字面量当用户反馈喂回模型)", got)
	}
}

// TestElicitationPlanApprovalRejectsLabel: 客户端回 label "Approve" (不在 enum 内)
// → 整组 cancel。**绝不能**把 "Approve" 当答案交给 zcode。
func TestElicitationPlanApprovalRejectsLabel(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(planApprovalQuestion), elicitationFormCaps())

	elID, _ := waitElicitation(t, c, 10*time.Second)
	replyElicitation(c, elID, "accept", map[string]any{"Review this implementation plan.": "Approve"})
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "cancel" {
		t.Fatalf("enum 外的取值必须被拒绝, zcode 应收到 cancel, 实际: %v", inner)
	}
	if _, bad := inner["content"]; bad {
		t.Errorf("cancel 不应带 content (半个答案比没有答案更危险), got %v", inner["content"])
	}
}

// TestElicitationMultiSelectArray: 多选 → 每选项一个 boolean property, 勾选的合成
// 数组交给 zcode。
//
// **不能**用 `type: "array"` + `items.enum`: ACP form 模式的 property 只允许原始
// 类型与 enum (elicitation.md:101, schema.md 的 ElicitationSchema "must be
// primitive types"), 数组两者都不是 —— 严格客户端会按 -32602 拒掉整个请求, 于是
// 每一个多选题都被悄悄降级。
func TestElicitationMultiSelectArray(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(frameworkQuestion, platformsQuestion), elicitationFormCaps())

	elID, params := waitElicitation(t, c, 10*time.Second)
	schema, _ := params["requestedSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	ios, _ := props["Which platforms must be supported?[ios]"].(map[string]any)
	android, _ := props["Which platforms must be supported?[android]"].(map[string]any)
	if ios == nil || android == nil {
		t.Fatalf("多选应展开为每选项一个 boolean property, 实际 keys=%v", keysOf(props))
	}
	if ios["type"] != "boolean" || android["type"] != "boolean" {
		t.Errorf("多选 property 应是 boolean, got %v / %v", ios["type"], android["type"])
	}
	if ios["title"] != "iOS" {
		t.Errorf("property.title = %v, want iOS (= label)", ios["title"])
	}
	if _, bad := props["Which platforms must be supported?"]; bad {
		t.Errorf("多选题不该再有聚合 property (那是 array 投影, 非法)")
	}
	// required 只列单选题 —— 强制客户端为未勾选项回 false 是不合理的要求。
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "Which framework should we use?" {
		t.Errorf("required = %v, want 只含单选题", req)
	}

	// **一次** elicitation 问完两问 —— 不再逐题一轮。
	replyElicitation(c, elID, "accept", map[string]any{
		"Which framework should we use?":              "react",
		"Which platforms must be supported?[ios]":     true,
		"Which platforms must be supported?[android]": true,
	})
	assertNoAgentRequest(t, c, "session/request_permission", 300*time.Millisecond)
	waitPromptOK(t, done)

	answers := askAnswers(t, waitAskReply(t, f))
	if got := answers["Which framework should we use?"]; got != "react" {
		t.Errorf("第一题答案 = %v, want react", got)
	}
	platforms, ok := answers["Which platforms must be supported?"].([]any)
	if !ok || len(platforms) != 2 || platforms[0] != "ios" || platforms[1] != "android" {
		t.Errorf("多选答案应原样是数组 [ios android], 实际: %v", answers)
	}
}

// TestElicitationMultiSelectPartialTicks: 只勾一部分 → zcode 只收到勾中的那些。
func TestElicitationMultiSelectPartialTicks(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(platformsQuestion), elicitationFormCaps())

	elID, _ := waitElicitation(t, c, 10*time.Second)
	replyElicitation(c, elID, "accept", map[string]any{
		"Which platforms must be supported?[android]": true,
		// ios 那个 property 省略 = 不勾 (checkbox 语义)。
	})
	waitPromptOK(t, done)

	answers := askAnswers(t, waitAskReply(t, f))
	platforms, ok := answers["Which platforms must be supported?"].([]any)
	if !ok || len(platforms) != 1 || platforms[0] != "android" {
		t.Errorf("只勾了 android, 答案应是 [android], 实际: %v", answers)
	}
}

// TestElicitationPartialAnswerCancels: 客户端只答了一问 → 整组 cancel。
//
// 丢弃缺的那题、其余照收会产生「不完整但标着 accept」的 answers, 模型会当成用户
// 确实只答了这些, 然后基于缺的那题往下编。
func TestElicitationPartialAnswerCancels(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(frameworkQuestion, platformsQuestion), elicitationFormCaps())

	elID, _ := waitElicitation(t, c, 10*time.Second)
	replyElicitation(c, elID, "accept", map[string]any{
		"Which framework should we use?": "react",
		// 多选题的 boolean 一个都没给。
	})
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "cancel" {
		t.Fatalf("缺答案应整组 cancel, 实际: %v", inner)
	}
	if _, bad := inner["content"]; bad {
		t.Errorf("cancel 不应带 content, got %v", inner["content"])
	}
}

// TestElicitationDeclineAndCancel: decline / cancel → zcode 收到对应 action。
//
// decline 的语义是「不答这题」, 但 zcode 的应答闭集里 decline 并不携带内容, 无法
// 表达「哪一题 decline 了」, 所以整组 cancel; 区别只体现在日志与 _meta 上。
func TestElicitationDeclineAndCancel(t *testing.T) {
	for _, action := range []string{"decline", "cancel"} {
		f, c, done := startAskCaps(t, askScript(frameworkQuestion), elicitationFormCaps())
		elID, _ := waitElicitation(t, c, 10*time.Second)
		replyElicitation(c, elID, action, nil)
		waitPromptOK(t, done)

		inner := waitAskReply(t, f)
		if inner["action"] != "cancel" {
			t.Errorf("客户端 action=%s 时 zcode 应收到 cancel, 实际: %v", action, inner)
		}
	}
}

// TestElicitationUnknownActionRejected: 规范里 action 是闭集 (外加 `_` 前缀的实现
// 私有 `other`)。收到不认识的值不能猜。
func TestElicitationUnknownActionRejected(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(frameworkQuestion), elicitationFormCaps())

	elID, _ := waitElicitation(t, c, 10*time.Second)
	replyElicitation(c, elID, "_someVendorAction", map[string]any{"Which framework should we use?": "react"})
	waitPromptOK(t, done)

	if inner := waitAskReply(t, f); inner["action"] != "cancel" {
		t.Fatalf("未知 action 必须 cancel, 实际: %v", inner)
	}
}

// TestElicitationDroppedEmptyOptions: 选项缺 value 的题投影不出合法 enum,
// 明确失败而不是摆一张用户答不了的表。
func TestElicitationDroppedEmptyOptions(t *testing.T) {
	bad := map[string]any{
		"question": "Pick one.",
		"header":   "Pick",
		"options":  []any{map[string]any{"label": "只有 label 没有 value"}},
	}
	f, c, done := startAskCaps(t, askScript(bad), elicitationFormCaps())

	assertNoAgentRequest(t, c, "elicitation/create", 2*time.Second)
	waitPromptOK(t, done)
	if inner := waitAskReply(t, f); inner["action"] != "cancel" {
		t.Fatalf("无合法选项时应 cancel, 实际: %v", inner)
	}
}

// TestElicitationDuplicateQuestionsFallBackToIndexedKeys: 两道题原文相同 →
// property 名退化为 answer_<i> (zcode 的 normalizeAskUserQuestionAnswers 同样按
// content["answer_<i>"] 取值), 保证 requestedSchema 合法 (JSON 对象键唯一)。
func TestElicitationDuplicateQuestionsFallBackToIndexedKeys(t *testing.T) {
	dup := map[string]any{
		"question": "Same?",
		"header":   "Same",
		"options":  []any{map[string]any{"label": "Yes", "value": "yes"}},
	}
	_, c, _ := startAskCaps(t, askScript(dup, dup), elicitationFormCaps())

	_, params := waitElicitation(t, c, 10*time.Second)
	schema, _ := params["requestedSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if len(props) != 2 {
		t.Fatalf("property 数 = %d, want 2 (重复的题要有各自的键)", len(props))
	}
	if _, ok := props["answer_1"]; !ok {
		t.Errorf("第二道重复题应退化为 answer_1, 实际 keys=%v", keysOf(props))
	}
}

// assertEnum 断言一个 schema 节点的 enum 顺序与内容。
func assertEnum(t *testing.T, node map[string]any, want []any) {
	t.Helper()
	got, _ := node["enum"].([]any)
	if len(got) != len(want) {
		t.Fatalf("enum = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("enum = %v, want %v (enum 必须取 option.value)", got, want)
		}
	}
}

// keysOf 取 map 的键, 供失败信息用。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
