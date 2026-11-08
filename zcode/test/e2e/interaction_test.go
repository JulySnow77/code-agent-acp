package e2e

import (
	"testing"
	"time"
)

// T02 交互请求的生命周期: 取消 / 超时 / 断连下都能干净收尾, 且**客户端不留悬空
// 请求**。
//
// T01 只证明了「zcode 侧收到合法应答」。它没覆盖另一半: 桥在放弃等待时是
// 单方面 dropPending —— 客户端那条 session/request_permission 永远收不到应答,
// UI 停在「等待授权」。整个 turn 取消之后弹窗还挂着, 这正是 T02 要修的。
//
// 防回归挂死: 取消/超时用例都注入 ZCODE_ACP_QUESTION_TIMEOUT。没修好的话这里
// 是「断言失败」, 而不是整个测试挂满 10 分钟。

// interactionTimeoutEnv 是取消/超时用例的交互超时上限。取得足够小以便快速失败,
// 又足够大以免和「取消应当立即生效」这条断言抢跑。
var interactionTimeoutEnv = map[string]string{"ZCODE_ACP_QUESTION_TIMEOUT": "30s"}

// waitPermRequest 等桥发来的 session/request_permission, 记住它的 id。
func waitPermRequest(t *testing.T, c *acpClient, timeout time.Duration) (string, map[string]any) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				t.Fatal("stdout 已关闭")
			}
			if fr["method"] == "session/request_permission" {
				id, _ := fr["id"].(string)
				params, _ := fr["params"].(map[string]any)
				return id, params
			}
		case <-deadline:
			t.Fatal("等待 session/request_permission 超时")
		}
	}
}

// waitAbandonedOutcome 等桥给那条**已放弃**的请求补的结果帧。
//
// acpClient.readLoop 把「有 id 无 method」的帧路由给 c.pending; 桥补的 brq-N
// 从没被测试注册过 pending, 所以会落到 c.frames 上, 这里能收到。
//
// 返回 result.outcome 的原始 map: {outcome:"selected", optionId} 或
// {outcome:"cancelled"}。
func waitAbandonedOutcome(t *testing.T, c *acpClient, reqID string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				t.Fatal("stdout 已关闭")
			}
			if fr["method"] != nil {
				continue // 通知
			}
			if id, _ := fr["id"].(string); id != reqID {
				continue
			}
			if errObj, bad := fr["error"]; bad {
				t.Fatalf("桥给已放弃的请求回了 error 而非结果: %v", errObj)
			}
			result, _ := fr["result"].(map[string]any)
			outcome, _ := result["outcome"].(map[string]any)
			if outcome == nil {
				t.Fatalf("结果帧缺 outcome: %v", fr)
			}
			return outcome
		case <-deadline:
			t.Fatalf("等待桥给 %s 补结果帧超时 —— 客户端这条请求会一直悬着", reqID)
		}
	}
}

// assertCancelledOutcome 断言是 ACP 规定的「什么都没批」说法。
func assertCancelledOutcome(t *testing.T, outcome map[string]any) {
	t.Helper()
	if got := outcome["outcome"]; got != "cancelled" {
		t.Fatalf("outcome = %v, want cancelled (悬空的授权请求会让客户端 UI 卡住)", got)
	}
	if id, bad := outcome["optionId"]; bad && id != "" {
		t.Errorf("cancelled 不应带 optionId, got %v", id)
	}
}

// cancelSession 发 session/cancel 通知 (无 id)。
func cancelSession(c *acpClient, sessionID string) {
	_ = c.write(map[string]any{
		"jsonrpc": "2.0", "method": "session/cancel",
		"params": map[string]any{"sessionId": sessionID},
	})
}

// TestInteractionCancelDuringPermission: 权限请求在飞时取消整个 turn。
//
// 旧行为: turn 循环同步停在 handlePermission 的 case body 里等答案,
// session/cancel 虽然执行了 (关掉 turn.cancel) 却没人听 —— 最坏要等
// compat.Question (默认 10 分钟) 才醒, 且客户端那条请求始终悬空。
func TestInteractionCancelDuringPermission(t *testing.T) {
	f, c, done := startPermEnv(t, permScript(zcodePermissionOptions()), interactionTimeoutEnv)

	permID, _ := waitPermRequest(t, c, 10*time.Second)
	if permID == "" {
		t.Fatal("桥发的 session/request_permission 没有 id")
	}
	cancelSession(c, "s-perm")

	// ① 客户端那条请求必须收到 cancelled, 而不是永远等下去。
	start := time.Now()
	assertCancelledOutcome(t, waitAbandonedOutcome(t, c, permID, 10*time.Second))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("取消后 %s 才收尾, 应立即生效 (旧实现要等 Question 超时)", elapsed)
	}

	// ② zcode 侧仍要收到合法 deny —— 绝不能挂死。
	assertDeny(t, zcodePermissionResponse(t, f))

	// ③ prompt 以 cancelled 收尾。
	select {
	case resp := <-done:
		if resp["error"] != nil {
			t.Fatalf("取消后 prompt 应返回 stopReason=cancelled, 实际错误: %v", resp["error"])
		}
		result, _ := resp["result"].(map[string]any)
		if result["stopReason"] != "cancelled" {
			t.Errorf("stopReason = %v, want cancelled", result["stopReason"])
		}
	case <-time.After(20 * time.Second):
		t.Fatal("取消后 prompt 未收尾 (zcode 侧挂死)")
	}
}

// TestInteractionCancelDuringAsk: 同上, 走 interaction/requestUserInput 路径。
// 问答是逐题同步发的, 取消必须掐断的是**当前那一题**。
func TestInteractionCancelDuringAsk(t *testing.T) {
	f := startScriptedEnv(t, askScript(frameworkQuestion), interactionTimeoutEnv)
	c := f.client
	c.request(1, "initialize", map[string]any{})
	c.request(2, "session/new", map[string]any{"cwd": "/tmp"})
	done := make(chan map[string]any, 1)
	go func() {
		done <- c.request(3, "session/prompt", map[string]any{
			"sessionId": "s-ask", "prompt": "ASK_USER_QUESTION",
		})
	}()

	permID, _ := waitPermRequest(t, c, 10*time.Second)
	cancelSession(c, "s-ask")

	assertCancelledOutcome(t, waitAbandonedOutcome(t, c, permID, 10*time.Second))

	// 问答放弃后向 zcode 回 cancel, 而不是继续问下一题。
	reply := f.waitForReply(t, "interaction/requestUserInput")
	inner, _ := reply["result"].(map[string]any)
	if inner == nil || inner["action"] != "cancel" {
		t.Errorf("取消后 zcode 应收到 {action: cancel}, 实际: %v", reply)
	}

	select {
	case resp := <-done:
		if resp["error"] != nil {
			t.Fatalf("取消后 prompt 应返回 stopReason=cancelled, 实际错误: %v", resp["error"])
		}
		result, _ := resp["result"].(map[string]any)
		if result["stopReason"] != "cancelled" {
			t.Errorf("stopReason = %v, want cancelled", result["stopReason"])
		}
	case <-time.After(20 * time.Second):
		t.Fatal("取消后 prompt 未收尾 (zcode 侧挂死)")
	}
}

// TestPermissionClientAnsweredOnTimeout: 交互超时时客户端也必须被收尾。
//
// 现有 TestPermissionTimeout 只断言了 zcode 侧收到 deny, 漏掉的正是这一半:
// 超时后客户端那条 id 永远不会有第二帧。
func TestPermissionClientAnsweredOnTimeout(t *testing.T) {
	// 2s: 明显短于任何正常应答, 但够走完 fake 的应答回路。
	f, c, done := startPermEnv(t, permScript(zcodePermissionOptions()),
		map[string]string{"ZCODE_ACP_QUESTION_TIMEOUT": "2s"})

	permID, _ := waitPermRequest(t, c, 10*time.Second)
	// 故意不应答 —— 逼桥走超时分支。
	assertCancelledOutcome(t, waitAbandonedOutcome(t, c, permID, 20*time.Second))
	assertDeny(t, zcodePermissionResponse(t, f))
	waitPromptEnd(t, done)
}

// TestConcurrentPromptQueued: P3 起同会话并发 prompt 不再 -32600, 而是在闸口
// 排队 —— 第一个 turn 结束(或取消)后自动轮到第二个。
//
// 行为沿革: 旧实现 runTurn 在 subscribe+send **之后**才登记 br.turns, 第二个
// prompt 能从「已进 runTurn 但还没登记」的窗口插进来, RegisterListener 对同一
// sid 直接覆盖, 第一个 turn 的权限请求被当成「无在飞 turn」自动 cancel。
// T02 改成硬拒绝 (-32600); P3 拍板改为排队 (闸口串行 + 深度上限 8), 排队期间
// 客户端取消会让它退出队列。
//
// 用例靠 fake 的 delayMs 把 session/send 撑到 1.5s, 让第二个 prompt 确定落在
// 第一个 turn 的窗口里。第二个 prompt 也会排到自己的 send (同样 1.5s 延迟),
// 各自触发一条权限请求。
func TestConcurrentPromptQueued(t *testing.T) {
	script := permScript(zcodePermissionOptions())
	send := script.Responses["session/send"]
	send.DelayMs = 1500
	script.Responses["session/send"] = send

	f := startScriptedEnv(t, script, interactionTimeoutEnv)
	c := f.client
	c.request(1, "initialize", map[string]any{})
	c.request(2, "session/new", map[string]any{"cwd": "/tmp"})

	// 第一个 prompt 出发, 随后卡在慢 session/send 里。
	done1 := make(chan map[string]any, 1)
	go func() {
		done1 <- c.request(3, "session/prompt", map[string]any{
			"sessionId": "s-perm", "prompt": "run something",
		})
	}()
	time.Sleep(300 * time.Millisecond) // 落进第一个 turn 的 send 窗口

	// 第二个 prompt 打同一个会话 —— 应排队, 而不是立刻返回错误。
	done2 := make(chan map[string]any, 1)
	go func() {
		done2 <- c.request(4, "session/prompt", map[string]any{
			"sessionId": "s-perm", "prompt": "run something else",
		})
	}()
	time.Sleep(500 * time.Millisecond)
	select {
	case resp := <-done2:
		t.Fatalf("第二个 prompt 应还在排队, 实际已返回: %v", resp)
	default:
	}

	// 第一个 turn 走完整个人在回路, 正常结束。
	permID1, _ := waitPermRequest(t, c, 15*time.Second)
	replyPermission(c, permID1, "selected", "allow_once")
	waitPromptEnd(t, done1)

	// 轮到第二个: 它自己的 send 完成后再来一次权限请求, 答完即结束
	// (waitPromptEnd 内部已断言无 error)。
	permID2, _ := waitPermRequest(t, c, 15*time.Second)
	replyPermission(c, permID2, "selected", "allow_once")
	waitPromptEnd(t, done2)

	// 两个 prompt 都必须真的下发过后端 (排队不是丢弃)。
	sends := 0
	for _, r := range f.loggedRequests(t) {
		if r["method"] == "session/send" {
			sends++
		}
	}
	if sends != 2 {
		t.Errorf("session/send 次数 = %d, want 2 (排队的 prompt 轮到后必须下发)", sends)
	}
}
