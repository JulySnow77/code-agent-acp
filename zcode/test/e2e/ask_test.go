package e2e

import (
	"testing"
	"time"
)

// 降级路径 (客户端未声明 elicitation.form) 的问答契约。
//
// **fixture 里 label ≠ value 是刻意的。** 旧 fixture 全部 value == label, 于是
// 「桥到底回传的是 label 还是 value」这件事在测试里根本不可见 —— T03 的 plan 审批
// bug（用户点「Approve」、zcode 收到 "Approve" 而不是 "approve"）就是这么藏下来的。
// 真实 zcode 的 plan 审批选项正是 {label:"Approve", value:"approve"}
// (createExitPlanModeApprovalQuestion, interaction-broker.ts:326)。

var frameworkQuestion = map[string]any{
	"question":    "Which framework should we use?",
	"header":      "Framework",
	"multiSelect": false,
	"options": []any{
		map[string]any{"label": "React", "value": "react"},
		map[string]any{"label": "Vue", "value": "vue"},
	},
}

var platformsQuestion = map[string]any{
	"question":    "Which platforms must be supported?",
	"header":      "Platforms",
	"multiSelect": true,
	"options": []any{
		map[string]any{"label": "iOS", "value": "ios"},
		map[string]any{"label": "Android", "value": "android"},
	},
}

// planApprovalQuestion 复刻 zcode 真实的 plan 审批问题
// (createExitPlanModeApprovalQuestion, interaction-broker.ts:326,
//
//	常量 EXIT_PLAN_MODE_APPROVAL_QUESTION / _APPROVE 见同文件 :39-40)。
var planApprovalQuestion = map[string]any{
	"question":    "Review this implementation plan.",
	"header":      "Plan",
	"multiSelect": false,
	"options": []any{
		map[string]any{
			"label":       "Approve",
			"value":       "approve",
			"description": "Exit plan mode and start implementation.",
		},
	},
}

// askScript 构造: create → subscribe → send (响应后反问一组问题,
// 应答到达再推 turn.completed)。
func askScript(questions ...map[string]any) fakeScript {
	qs := make([]any, len(questions))
	for i, q := range questions {
		qs[i] = q
	}
	return askScriptPrompt(qs)
}

// askScriptPrompt 与 askScript 相同但允许直接给 questions 数组。
func askScriptPrompt(qs []any) fakeScript {
	return fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{"session": map[string]any{"sessionId": "s-ask"}}},
			"session/subscribe": {Result: map[string]any{
				"eventSeq": 0, "snapshot": map[string]any{"status": "idle"},
			}},
			// ask turn 无流式文本 → 桥会兜底取最后回复 (session/messages)。
			"session/messages": {Result: map[string]any{"messages": []any{}}},
			"session/send": {
				Result: map[string]any{"accepted": true},
				ServerRequests: []fakeServerReq{{
					Method: "interaction/requestUserInput",
					Params: map[string]any{"sessionId": "s-ask", "questions": qs},
					Then: []fakePush{
						{Method: "session/event", Params: map[string]any{
							"sessionId": "s-ask", "seq": 2, "type": "turn.completed",
							"payload": map[string]any{"resultType": "success"},
						}},
					},
				}},
			},
		},
	}
}

// elicitationFormCaps 是声明了 elicitation.form 的 clientCapabilities。
func elicitationFormCaps() map[string]any {
	return map[string]any{
		"clientCapabilities": map[string]any{
			"elicitation": map[string]any{"form": map[string]any{}},
		},
	}
}

// startAsk 启动脚本桥并走到 prompt 在飞, 返回 (fake, 客户端, prompt 完成通道)。
// 不声明 elicitation 能力 → 走降级路径。
func startAsk(t *testing.T, script fakeScript) (*scriptedFake, *acpClient, chan map[string]any) {
	t.Helper()
	return startAskCaps(t, script, map[string]any{})
}

// startAskCaps 与 startAsk 相同, 但 initialize 带上给定的 clientCapabilities。
func startAskCaps(t *testing.T, script fakeScript, caps map[string]any) (*scriptedFake, *acpClient, chan map[string]any) {
	t.Helper()
	f := startScripted(t, script)
	f.client.request(1, "initialize", caps)
	f.client.request(2, "session/new", map[string]any{"cwd": "/tmp"})
	done := make(chan map[string]any, 1)
	go func() {
		done <- f.client.request(3, "session/prompt", map[string]any{
			"sessionId": "s-ask", "prompt": "ASK_USER_QUESTION",
		})
	}()
	return f, f.client, done
}

// waitPermission 等桥发来的 session/request_permission 请求 (agent→client)。
func waitPermission(t *testing.T, c *acpClient, timeout time.Duration) (string, map[string]any) {
	t.Helper()
	return waitAgentRequest(t, c, "session/request_permission", timeout)
}

// waitElicitation 等桥发来的 elicitation/create 请求。
func waitElicitation(t *testing.T, c *acpClient, timeout time.Duration) (string, map[string]any) {
	t.Helper()
	return waitAgentRequest(t, c, "elicitation/create", timeout)
}

// waitAgentRequest 等一条指定方法的 agent→client 请求。
func waitAgentRequest(t *testing.T, c *acpClient, method string, timeout time.Duration) (string, map[string]any) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				t.Fatal("stdout 已关闭")
			}
			if fr["method"] == method {
				id, _ := fr["id"].(string)
				params, _ := fr["params"].(map[string]any)
				return id, params
			}
		case <-deadline:
			t.Fatalf("等待 %s 超时", method)
		}
	}
}

// assertNoAgentRequest 断言某类请求在超时窗口内**没有**发出来。
func assertNoAgentRequest(t *testing.T, c *acpClient, method string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				t.Fatal("stdout 已关闭")
			}
			if fr["method"] == method {
				t.Fatalf("不该发出 %s, 却收到了 (params=%v)", method, fr["params"])
			}
		case <-deadline:
			return
		}
	}
}

// replyPermission 应答 session/request_permission。
func replyPermission(c *acpClient, id string, outcome, optionID string) {
	c.write(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{"outcome": map[string]any{"outcome": outcome, "optionId": optionID}},
	})
}

// replyElicitation 应答 elicitation/create。content 为 nil 时不带 content 字段。
func replyElicitation(c *acpClient, id, action string, content map[string]any) {
	result := map[string]any{"action": action}
	if content != nil {
		result["content"] = content
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// waitAskReply 等桥对 interaction/requestUserInput 的应答并返回其 result。
func waitAskReply(t *testing.T, f *scriptedFake) map[string]any {
	t.Helper()
	reply := f.waitForReply(t, "interaction/requestUserInput")
	inner, _ := reply["result"].(map[string]any)
	if inner == nil {
		t.Fatalf("对 requestUserInput 的应答缺 result: %v", reply)
	}
	return inner
}

// askAnswers 取应答里的 answers (accept 时才有)。
func askAnswers(t *testing.T, inner map[string]any) map[string]any {
	t.Helper()
	content, _ := inner["content"].(map[string]any)
	answers, _ := content["answers"].(map[string]any)
	if answers == nil {
		t.Fatalf("accept 应答缺 answers: %v", inner)
	}
	return answers
}

// waitPromptOK 等 prompt 正常收尾。
func waitPromptOK(t *testing.T, done chan map[string]any) {
	t.Helper()
	select {
	case resp := <-done:
		if resp["error"] != nil {
			t.Fatalf("prompt 失败: %v", resp["error"])
		}
	case <-time.After(20 * time.Second):
		t.Fatal("prompt 超时")
	}
}

// TestAskFallbackRoundTrip: 单问题, 用户选 vue → zcode 收到 accept + answers["vue"]。
//
// optionId 必须是 **value** 而不是 label —— 命中之后原样回 value。
func TestAskFallbackRoundTrip(t *testing.T) {
	f, c, done := startAsk(t, askScript(frameworkQuestion))

	permID, params := waitPermission(t, c, 10*time.Second)
	// 选项 = 2 个问题选项 + 取消项。
	opts, _ := params["options"].([]any)
	if len(opts) != 3 {
		t.Fatalf("选项数 = %d, want 3 (2 选项 + 取消项)", len(opts))
	}
	first, _ := opts[0].(map[string]any)
	if first["optionId"] != "react" {
		t.Errorf("optionId = %v, want react (= zcode 的 value, 不是 label)", first["optionId"])
	}
	if first["name"] != "React" {
		t.Errorf("name = %v, want React (label 是给人看的)", first["name"])
	}
	toolCall, _ := params["toolCall"].(map[string]any)
	if toolCall["title"] != "AskUserQuestion" {
		t.Errorf("toolCall.title = %v", toolCall["title"])
	}

	replyPermission(c, permID, "selected", "vue")
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "accept" {
		t.Fatalf("zcode 收到 action=%v, want accept", inner["action"])
	}
	answers := askAnswers(t, inner)
	if got := answers["Which framework should we use?"]; got != "vue" {
		t.Errorf("答案 = %v, want vue (必须是 value)", got)
	}
}

// TestAskFallbackReturnsValue: 回 label 而不是 value 时**不算**选中。
//
// 旧实现 optionId 就是 label, 所以这么回是合法的; 新契约下 label 不是 optionId,
// 客户端这么答等于回了一个没投放过的 id, 必须被拒绝 —— 否则桥等于允许客户端用
// label 之外的任意字符串表达用户的选择。
func TestAskFallbackReturnsValue(t *testing.T) {
	f, c, done := startAsk(t, askScript(frameworkQuestion))

	permID, _ := waitPermission(t, c, 10*time.Second)
	replyPermission(c, permID, "selected", "Vue") // label, 不是 value
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "cancel" {
		t.Fatalf("回了未投放的 optionId, zcode 应收到 cancel, 实际: %v", inner)
	}
}

// TestAskFallbackRejectsFreeText: 选项外的 optionId 原先是当自由文本透传的。
//
// 那是授权协议上的漏洞: 客户端回什么都能变成「用户说了这句话」。新契约只认投放过
// 的 optionId, 其余一律 cancel。
func TestAskFallbackRejectsFreeText(t *testing.T) {
	f, c, done := startAsk(t, askScript(frameworkQuestion))

	permID, _ := waitPermission(t, c, 10*time.Second)
	replyPermission(c, permID, "selected", "Svelte 吧")
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "cancel" {
		t.Fatalf("自由文本不得被当答案透传, zcode 应收到 cancel, 实际: %v", inner)
	}
	if content, bad := inner["content"]; bad {
		t.Errorf("cancel 不应带 content, got %v", content)
	}
	if reason, _ := inner["reason"].(string); reason == "" {
		t.Errorf("cancel 应带 reason 说明为什么没答成")
	}
}

// TestAskFallbackRejectsArray: 塞进 optionId 的 JSON 数组 (omnigent 私有编码)。
//
// 旧实现靠 strings.HasPrefix(optionId, "[") 把它解析回 []string。这层编码已移除:
// zcode 的 normalizeAnswerValue (interaction-broker.ts:617) 本来就接受数组, 正规
// 客户端直接回数组即可, 而把 JSON 塞进 optionId 是协议违规 (optionId 必须是
// 投放过的选项之一)。
func TestAskFallbackRejectsArray(t *testing.T) {
	f, c, done := startAsk(t, askScript(frameworkQuestion))

	permID, _ := waitPermission(t, c, 10*time.Second)
	replyPermission(c, permID, "selected", `["react","vue"]`)
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "cancel" {
		t.Fatalf("JSON 数组塞进 optionId 应被拒绝, zcode 应收到 cancel, 实际: %v", inner)
	}
}

// TestAskFallbackRejectCancels: 用户点取消项 → zcode 收到 cancel, turn 正常结束。
func TestAskFallbackRejectCancels(t *testing.T) {
	f, c, done := startAsk(t, askScript(frameworkQuestion))

	permID, params := waitPermission(t, c, 10*time.Second)
	// 取消项的 optionId 是保留哨兵。测试从**实际投放的选项**里取, 不写死字面量 ——
	// 写死的话哨兵改名就会静默通过, 而哨兵改名正是要防的事 (它必须与任何 zcode
	// 的 value 都不撞车)。
	opts, _ := params["options"].([]any)
	cancelID := ""
	for _, raw := range opts {
		o, _ := raw.(map[string]any)
		if o["kind"] == "reject_once" {
			cancelID, _ = o["optionId"].(string)
		}
	}
	if cancelID == "" {
		t.Fatalf("选项里应有唯一的 reject_once 取消项: %v", opts)
	}
	for _, raw := range opts {
		o, _ := raw.(map[string]any)
		if o["optionId"] == cancelID && o["kind"] != "reject_once" {
			t.Fatalf("取消哨兵 %q 撞上了真实选项 %v", cancelID, o)
		}
	}

	replyPermission(c, permID, "selected", cancelID)
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "cancel" {
		t.Fatalf("用户点取消后 zcode 应收到 cancel, 实际: %v", inner)
	}
}

// TestAskFallbackMultiSelectUnsupported: 降级路径表达不了多选 → 明确 cancel,
// 且**一题都不问**。
//
// 「一题都不问」是关键: 问完三题才发现第四题是多选, 用户白答三次, 而且那三个
// 答案会被整组丢弃。
func TestAskFallbackMultiSelectUnsupported(t *testing.T) {
	f, c, done := startAsk(t, askScript(frameworkQuestion, platformsQuestion))

	assertNoAgentRequest(t, c, "session/request_permission", 3*time.Second)
	waitPromptOK(t, done)

	inner := waitAskReply(t, f)
	if inner["action"] != "cancel" {
		t.Fatalf("多选在降级路径上无法表达, zcode 应收到 cancel, 实际: %v", inner)
	}
	reason, _ := inner["reason"].(string)
	if reason == "" {
		t.Errorf("cancel 应带 reason 说明是能力不足")
	}
}

// 工具权限契约的用例见 permission_test.go (T01: 选项与应答对齐真实 zcode schema)。

// waitForReply 在 fake 日志中找对某方法反向调用的应答, 返回**应答帧**本身
// (日志条目形如 {"id":"srv-N","method":…,"reply":{"id":…,"result":…}})。
func (f *scriptedFake) waitForReply(t *testing.T, method string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, entry := range f.loggedRequests(t) {
			if entry["method"] != method {
				continue
			}
			if reply, ok := entry["reply"].(map[string]any); ok {
				return reply
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("fake 日志中未见对 %s 的应答", method)
	return nil
}
