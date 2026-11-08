package e2e

import (
	"testing"
	"time"
)

// zcode 重发没等到应答的提问 (reannounce) 的契约。
//
// zcode 的 requestClient 在等人应答时会按 reannounceIntervalMs 重发同一个请求
// (requestUserInput 拿到 1000ms, 之后翻倍, 上限 10s), 每次都用**新的** JSON-RPC
// id 但 method/params 完全一致。人在回路要几秒才答得上来, 所以真机上几乎必然
// 触发 —— 一次提问变成两次, 人被问两遍。
//
// 更糟的是第二份答案到不了模型: resolveClientRequest 按 id 查
// pendingClientRequests, 而第一条被应答时 cleanupClientRequest 把同一个 f 上登记的
// 所有 id 一起注销了, 后到的应答查无记录、静默丢弃。也就是用户第二次的选择被吞掉,
// 模型只看见第一次。

// reannounceScript 让 fake 连发两条 requestUserInput: **id 不同、requestId 相同**,
// 问题与选项一字不差 —— 精确复刻 zcode 的重发。
func reannounceScript(requestID string) fakeScript {
	qs := []any{frameworkQuestion}
	turnDone := []fakePush{{Method: "session/event", Params: map[string]any{
		"sessionId": "s-ask", "seq": 2, "type": "turn.completed",
		"payload": map[string]any{"resultType": "success"},
	}}}
	params := map[string]any{
		"sessionId": "s-ask", "questions": qs,
		"requestId": requestID, "toolCallId": "tc-1", "toolName": "AskUserQuestion",
	}
	return fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{"session": map[string]any{"sessionId": "s-ask"}}},
			"session/subscribe": {Result: map[string]any{
				"eventSeq": 0, "snapshot": map[string]any{"status": "idle"},
			}},
			"session/messages": {Result: map[string]any{"messages": []any{}}},
			"session/send": {
				Result: map[string]any{"accepted": true},
				ServerRequests: []fakeServerReq{
					// 第一次: 客户端会被问一次。
					{Method: "interaction/requestUserInput", Params: params},
					// 重发: 同一个 requestId, 不同的 id。桥必须**不再问**客户端。
					{Method: "interaction/requestUserInput", Params: params, Then: turnDone},
				},
			},
		},
	}
}

// TestAskReannounceAsksClientOnce: 重发**不能再问客户端第二次**。
//
// 修复前这条会失败: 桥按 id 判重, 两个不同 id 被当成两次提问, 人被问两遍。
func TestAskReannounceAsksClientOnce(t *testing.T) {
	f, c, done := startAskCaps(t, reannounceScript("req-1"), elicitationFormCaps())

	elID, params := waitElicitation(t, c, 10*time.Second)
	if params["sessionId"] != "s-ask" {
		t.Errorf("sessionId = %v, want s-ask", params["sessionId"])
	}
	replyElicitation(c, elID, "accept", map[string]any{"Which framework should we use?": "vue"})

	// 关键断言: 第二条 (重发) 不该再冒出一次 elicitation。
	assertNoAgentRequest(t, c, "elicitation/create", 2*time.Second)
	waitPromptOK(t, done)

	// 两条 zcode 请求都要被应答 —— 桥不能因为去重就漏掉重发那条, 否则 zcode 那条
	// request 会一直悬着 (真机上表现为 pending 泄漏 + 超时)。
	replies := askReplies(t, f, "interaction/requestUserInput", 2)
	for i, inner := range replies {
		if action, _ := inner["action"].(string); action != "accept" {
			t.Fatalf("第 %d 条应答 action = %v, want accept (重发要重放同一份答案)", i+1, action)
		}
		got := askAnswers(t, inner)["Which framework should we use?"]
		if got != "vue" {
			t.Errorf("第 %d 条 answers = %v, want vue (重放第一次的答案, 不重新问人)", i+1, got)
		}
	}
}

// TestAskReannounceFallsBackDedup: 降级路径 (session/request_permission) 同样要去重。
//
// 两条路径共用 handleUserInput, 但投放给客户端的东西完全不同 (permission 逐题一轮),
// 所以要单独钉住 —— 免得只验了 elicitation 就以为降级路径也覆盖到了。
func TestAskReannounceFallsBackDedup(t *testing.T) {
	f, c, done := startAsk(t, reannounceScript("req-2"))

	permID, _ := waitPermission(t, c, 10*time.Second)
	replyPermission(c, permID, "selected", "vue")

	// 重发不该再产生第二条 request_permission。
	assertNoAgentRequest(t, c, "session/request_permission", 2*time.Second)
	waitPromptOK(t, done)

	replies := askReplies(t, f, "interaction/requestUserInput", 2)
	for i, inner := range replies {
		got := askAnswers(t, inner)["Which framework should we use?"]
		if got != "vue" {
			t.Errorf("第 %d 条 answers = %v, want vue (重放第一次的答案)", i+1, got)
		}
	}
}

// TestAskDistinctRequestIDsBothAsked: 去重键必须是 requestId, 不能只看「有没有问过」。
//
// 真实的一轮里模型可以合法地连问**两个不同**的 requestId (demo-ask.sh 的 two
// 模式就是这个场景)。去重要按 requestId 精确匹配 —— 拿 coarser 的键 (比如会话级
// 「本 turn 已问过」标志) 会把第二次真实提问也吞掉, 那比重复提问更糟: 人根本
// 没被问, 模型却拿到一个凭空来的答案。
func TestAskDistinctRequestIDsBothAsked(t *testing.T) {
	qs := []any{frameworkQuestion, platformsQuestion}
	turnDone := []fakePush{{Method: "session/event", Params: map[string]any{
		"sessionId": "s-ask", "seq": 2, "type": "turn.completed",
		"payload": map[string]any{"resultType": "success"},
	}}}
	script := fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{"session": map[string]any{"sessionId": "s-ask"}}},
			"session/subscribe": {Result: map[string]any{
				"eventSeq": 0, "snapshot": map[string]any{"status": "idle"},
			}},
			"session/messages": {Result: map[string]any{"messages": []any{}}},
			"session/send": {
				Result: map[string]any{"accepted": true},
				ServerRequests: []fakeServerReq{
					{Method: "interaction/requestUserInput", Params: map[string]any{
						"sessionId": "s-ask", "questions": qs,
						"requestId": "req-a", "toolCallId": "tc-a",
					}},
					{Method: "interaction/requestUserInput", Params: map[string]any{
						"sessionId": "s-ask", "questions": qs,
						"requestId": "req-b", "toolCallId": "tc-b",
					}, Then: turnDone},
				},
			},
		},
	}
	f, c, done := startAskCaps(t, script, elicitationFormCaps())

	// 两个**不同** requestId → 两次 elicitation, 两次都得真问。
	// 多选题的应答是「一个选项一个 boolean」(见 internal/bridge/elicitation.go
	// 的 properties), 不是数组 —— 用错形状会被 readAnswer 判为非法而整组 cancel。
	first, _ := waitElicitation(t, c, 10*time.Second)
	replyElicitation(c, first, "accept", map[string]any{
		"Which framework should we use?":              "vue",
		"Which platforms must be supported?[ios]":     true,
		"Which platforms must be supported?[android]": false,
	})
	second, _ := waitElicitation(t, c, 10*time.Second)
	replyElicitation(c, second, "accept", map[string]any{
		"Which framework should we use?":              "react",
		"Which platforms must be supported?[ios]":     false,
		"Which platforms must be supported?[android]": true,
	})
	waitPromptOK(t, done)

	replies := askReplies(t, f, "interaction/requestUserInput", 2)
	if len(replies) != 2 {
		t.Fatalf("应答条数 = %d, want 2 (两个不同 requestId 都得被独立回答)", len(replies))
	}
	// 各自回各自的答案 —— 第二条不能被第一条的答案覆盖。
	if got := askAnswers(t, replies[0])["Which framework should we use?"]; got != "vue" {
		t.Errorf("第一条 answers = %v, want vue", got)
	}
	if got := askAnswers(t, replies[1])["Which framework should we use?"]; got != "react" {
		t.Errorf("第二条 answers = %v, want react (不能被第一条覆盖)", got)
	}
}

// askReplies 等齐 want 条对 method 的应答, 按发出顺序返回各自的 result。
//
// waitForReply 只给第一条 —— 这里要验「重发那条也被应答了」, 必须看全部。
func askReplies(t *testing.T, f *scriptedFake, method string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out []map[string]any
		for _, entry := range f.loggedRequests(t) {
			if entry["method"] != method {
				continue
			}
			reply, ok := entry["reply"].(map[string]any)
			if !ok {
				continue
			}
			inner, _ := reply["result"].(map[string]any)
			if inner == nil {
				continue
			}
			out = append(out, inner)
		}
		if len(out) >= want {
			return out[:want]
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("对 %s 的应答不足 %d 条 (桥漏答了重发那条?)", method, want)
	return nil
}
