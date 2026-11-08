package e2e

import (
	"strings"
	"testing"
	"time"
)

// T01 工具权限契约: interaction/requestPermission ↔ session/request_permission。
//
// fixture 复刻真实 ZCode 形状 (本地源码基线 29628c9 / 3.14.3):
//   - 选项 zcodePermissionOptionSchema: {optionId, kind, name, description?, response}
//   - 应答 zcodePermissionResponseSchema: {decision, reason?, modifiedInput?,
//     permissionUpdates?}, decision ∈ {allow, deny, escalate, modify}
//   - 选项列表由 buildProtocolPermissionOptions 合成 (permission-options.ts)
// 桥的回包必须是选中项 response 原文; 旧实现的 {action, optionId} 既不是合法
// PermissionResponse, 也丢掉了持久规则。

// zcodeDeniedByUser 是 zcode PERMISSION_DENIED_BY_USER_CONTENT 的逐字副本
// (apps/zcode-cli/packages/bootstrap/src/permission-options.ts)。
const zcodeDeniedByUser = "The user doesn't want to proceed with this tool use. " +
	"The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). " +
	"STOP what you are doing and wait for the user to tell you how to proceed."

// zcodePermissionOptions 复刻 buildProtocolPermissionOptions 的默认三项输出。
func zcodePermissionOptions() []any {
	return []any{
		map[string]any{
			"optionId": "allow_once",
			"kind":     "allow_once",
			"name":     "Allow once",
			"response": map[string]any{
				"decision": "allow",
				"reason":   "Approved once",
			},
		},
		map[string]any{
			"optionId":    "allow_project",
			"kind":        "allow_always",
			"name":        "Always allow in this project",
			"description": "Do not ask again for matching requests in this project",
			"response": map[string]any{
				"decision": "allow",
				"reason":   "Approved for this project",
				"permissionUpdates": []any{map[string]any{
					"type":     "addRules",
					"behavior": "allow",
					"rules": []any{map[string]any{
						"toolName":    "Bash",
						"ruleContent": "ls -la",
					}},
				}},
			},
		},
		map[string]any{
			"optionId": "deny",
			"kind":     "deny",
			"name":     "Deny",
			"response": map[string]any{
				"decision": "deny",
				"reason":   zcodeDeniedByUser,
			},
		},
	}
}

// zcodeSessionAllowOption 复刻 optionsPolicy=session-always-allow 时的选项。
// 它的 response 不带 permissionUpdates —— 会话语义由 zcode broker 在应答侧合成。
func zcodeSessionAllowOption() map[string]any {
	return map[string]any{
		"optionId":    "allowSession",
		"kind":        "allow_session",
		"name":        "Always allow in this session",
		"description": "Do not ask again for this tool in this session",
		"response": map[string]any{
			"decision": "allow",
			"reason":   "Approved for this session",
		},
	}
}

// permScript: session/send 反向调用 interaction/requestPermission, 应答后推
// turn.completed (照 zcodePermissionRequestParamsSchema 的真实字段构造)。
func permScript(options []any) fakeScript {
	return fakeScript{
		Responses: map[string]fakeAction{
			"session/create":    {Result: map[string]any{"session": map[string]any{"sessionId": "s-perm"}}},
			"session/subscribe": {Result: map[string]any{"eventSeq": 0}},
			"session/messages":  {Result: map[string]any{"messages": []any{}}},
			"session/send": {
				Result: map[string]any{"accepted": true},
				ServerRequests: []fakeServerReq{{
					Method: "interaction/requestPermission",
					Params: map[string]any{
						"requestId":  "req-1",
						"sessionId":  "s-perm",
						"toolCallId": "tc-1",
						"toolName":   "Bash",
						"reason":     "run a shell command",
						"riskLevel":  "medium",
						"input":      map[string]any{"command": "ls -la"},
						"options":    options,
					},
					Then: []fakePush{{
						Method: "session/event",
						Params: map[string]any{
							"sessionId": "s-perm", "seq": 1, "type": "turn.completed",
							"payload": map[string]any{"resultType": "success"},
						},
					}},
				}},
			},
		},
	}
}

// startPerm 走完 initialize → session/new → session/prompt, 等权限请求在飞。
func startPerm(t *testing.T, script fakeScript) (*scriptedFake, *acpClient, chan map[string]any) {
	t.Helper()
	return startPermEnv(t, script, nil)
}

func startPermEnv(t *testing.T, script fakeScript, env map[string]string) (*scriptedFake, *acpClient, chan map[string]any) {
	t.Helper()
	f := startScriptedEnv(t, script, env)
	f.client.request(1, "initialize", map[string]any{})
	f.client.request(2, "session/new", map[string]any{"cwd": "/tmp"})
	done := make(chan map[string]any, 1)
	go func() {
		done <- f.client.request(3, "session/prompt", map[string]any{
			"sessionId": "s-perm", "prompt": "run something",
		})
	}()
	return f, f.client, done
}

// waitPromptEnd 等 prompt 返回 (应答已送达 zcode, turn 正常收尾)。
func waitPromptEnd(t *testing.T, done chan map[string]any) {
	t.Helper()
	select {
	case resp := <-done:
		if resp["error"] != nil {
			t.Fatalf("prompt 失败: %v", resp["error"])
		}
	case <-time.After(20 * time.Second):
		t.Fatal("prompt 超时 (zcode 侧挂死)")
	}
}

// zcodePermissionResponse 取 fake 日志里桥对 interaction/requestPermission 的
// 应答 result —— 即 zcode 真正拿到的 PermissionResponse。
func zcodePermissionResponse(t *testing.T, f *scriptedFake) map[string]any {
	t.Helper()
	reply := f.waitForReply(t, "interaction/requestPermission")
	inner, _ := reply["result"].(map[string]any)
	if inner == nil {
		t.Fatalf("桥未应答 interaction/requestPermission: %v", reply)
	}
	return inner
}

// assertDeny 断言应答是合法 deny 且带 zcode 认得的拒绝文案。
func assertDeny(t *testing.T, resp map[string]any) {
	t.Helper()
	if resp["decision"] != "deny" {
		t.Fatalf("decision = %v, want deny (整包: %v)", resp["decision"], resp)
	}
	reason, _ := resp["reason"].(string)
	if !strings.Contains(reason, "The tool use was rejected") {
		t.Errorf("deny reason 需明确告知模型工具未执行, got %q", reason)
	}
	if _, bad := resp["action"]; bad {
		t.Errorf("不应回旧的 {action, optionId} 形状: %v", resp)
	}
	if _, bad := resp["optionId"]; bad {
		t.Errorf("不应回旧的 {action, optionId} 形状: %v", resp)
	}
}

// acpOptionsOf 取桥发来的 ACP 选项列表。
func acpOptionsOf(t *testing.T, params map[string]any) []map[string]any {
	t.Helper()
	raw, _ := params["options"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		o, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("选项不是对象: %v", item)
		}
		out = append(out, o)
	}
	return out
}

// acpOptionByID 按 optionId 取 ACP 选项。
func acpOptionByID(t *testing.T, params map[string]any, optionID string) map[string]any {
	t.Helper()
	for _, o := range acpOptionsOf(t, params) {
		if o["optionId"] == optionID {
			return o
		}
	}
	t.Fatalf("ACP 选项里没有 %q: %v", optionID, params["options"])
	return nil
}

// assertNoPermissionRequest 断言桥没有向客户端发权限请求。
func assertNoPermissionRequest(t *testing.T, c *acpClient) {
	t.Helper()
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case fr, ok := <-c.frames:
			if !ok {
				return
			}
			if fr["method"] == "session/request_permission" {
				t.Fatalf("无有效选项时不应向客户端发权限请求: %v", fr["params"])
			}
		case <-timer.C:
			return
		}
	}
}

// ---- 场景 ----

// TestPermissionAllowOnce: 允许一次 → zcode 收到该选项 response 原文。
func TestPermissionAllowOnce(t *testing.T) {
	f, c, done := startPerm(t, permScript(zcodePermissionOptions()))

	permID, params := waitPermission(t, c, 15*time.Second)
	opts := acpOptionsOf(t, params)
	if len(opts) != 3 {
		t.Fatalf("选项数 = %d, want 3 (zcode 三项原样透传): %v", len(opts), opts)
	}
	// ACP kind 必须在协议闭集内, 且与授权范围一致。
	for _, want := range []struct{ id, kind string }{
		{"allow_once", "allow_once"},
		{"allow_project", "allow_always"},
		{"deny", "reject_once"},
	} {
		if got := acpOptionByID(t, params, want.id)["kind"]; got != want.kind {
			t.Errorf("%s 的 ACP kind = %v, want %v", want.id, got, want.kind)
		}
	}
	// optionId 来自 zcode, 不是旧实现的 id/label 兜底。
	allowOnce := acpOptionByID(t, params, "allow_once")
	if allowOnce["name"] != "Allow once" {
		t.Errorf("name = %v, want zcode 的 \"Allow once\"", allowOnce["name"])
	}
	replyPermission(c, permID, "selected", "allow_once")
	waitPromptEnd(t, done)

	resp := zcodePermissionResponse(t, f)
	if resp["decision"] != "allow" || resp["reason"] != "Approved once" {
		t.Fatalf("zcode 应收到 allow_once 的 response 原文: %v", resp)
	}
	if _, has := resp["permissionUpdates"]; has {
		t.Errorf("允许一次不得带持久规则: %v", resp)
	}
}

// TestPermissionAllowProjectKeepsPermissionUpdates: 持久允许 → 权限更新字段
// 完整保留, 且 ACP 展示的 kind 与授权范围一致 (allow_always)。
func TestPermissionAllowProjectKeepsPermissionUpdates(t *testing.T) {
	f, c, done := startPerm(t, permScript(zcodePermissionOptions()))

	permID, params := waitPermission(t, c, 15*time.Second)
	if kind := acpOptionByID(t, params, "allow_project")["kind"]; kind != "allow_always" {
		t.Errorf("持久允许的 kind = %v, want allow_always", kind)
	}
	// zcode 的说明文字不能丢 —— 用户要靠它判断这条持久规则覆盖什么。
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("缺 _meta: %v", params)
	}
	zmeta, _ := meta["zcode"].(map[string]any)
	optionMeta, _ := zmeta["optionMeta"].(map[string]any)
	entry, _ := optionMeta["allow_project"].(map[string]any)
	if entry["description"] != "Do not ask again for matching requests in this project" {
		t.Errorf("_meta 里的选项说明异常: %v", optionMeta)
	}
	if zmeta["riskLevel"] != "medium" || zmeta["reason"] != "run a shell command" {
		t.Errorf("_meta 应保留 riskLevel/reason: %v", zmeta)
	}

	replyPermission(c, permID, "selected", "allow_project")
	waitPromptEnd(t, done)

	resp := zcodePermissionResponse(t, f)
	if resp["decision"] != "allow" {
		t.Fatalf("decision = %v, want allow", resp["decision"])
	}
	updates, _ := resp["permissionUpdates"].([]any)
	if len(updates) != 1 {
		t.Fatalf("permissionUpdates 丢失: %v", resp)
	}
	update, _ := updates[0].(map[string]any)
	if update["type"] != "addRules" || update["behavior"] != "allow" {
		t.Errorf("permissionUpdate 字段异常: %v", update)
	}
	rules, _ := update["rules"].([]any)
	rule, _ := rules[0].(map[string]any)
	if rule["toolName"] != "Bash" || rule["ruleContent"] != "ls -la" {
		t.Errorf("持久规则内容丢失: %v", rules)
	}
}

// TestPermissionAllowSessionKind: 会话免确认在 ACP 闭集里没有槽位, 按 zcode 自己
// v4 投影的既有做法归入 allow_always; 真实作用域靠 name/description 呈现, 桥不改
// response (会话语义由 zcode broker 在应答侧合成, wire 上不能带 permissionUpdates)。
func TestPermissionAllowSessionKind(t *testing.T) {
	options := append(zcodePermissionOptions(), zcodeSessionAllowOption())
	f, c, done := startPerm(t, permScript(options))

	permID, params := waitPermission(t, c, 15*time.Second)
	opt := acpOptionByID(t, params, "allowSession")
	if opt["kind"] != "allow_always" {
		t.Errorf("allow_session 的 ACP kind = %v, want allow_always", opt["kind"])
	}
	if opt["name"] != "Always allow in this session" {
		t.Errorf("name = %v, want zcode 的会话作用域名称", opt["name"])
	}
	replyPermission(c, permID, "selected", "allowSession")
	waitPromptEnd(t, done)

	resp := zcodePermissionResponse(t, f)
	if resp["decision"] != "allow" || resp["reason"] != "Approved for this session" {
		t.Fatalf("zcode 应收到会话选项的 response 原文: %v", resp)
	}
	if _, has := resp["permissionUpdates"]; has {
		t.Errorf("会话语义不得写成项目持久规则: %v", resp)
	}
}

// TestPermissionDenyOptionRelayed: 选择拒绝项 → 回传拒绝项响应, 不把选项一律标成允许。
func TestPermissionDenyOptionRelayed(t *testing.T) {
	f, c, done := startPerm(t, permScript(zcodePermissionOptions()))

	permID, params := waitPermission(t, c, 15*time.Second)
	if kind := acpOptionByID(t, params, "deny")["kind"]; kind != "reject_once" {
		t.Errorf("deny 选项的 ACP kind = %v, want reject_once", kind)
	}
	replyPermission(c, permID, "selected", "deny")
	waitPromptEnd(t, done)

	resp := zcodePermissionResponse(t, f)
	assertDeny(t, resp)
	if resp["reason"] != zcodeDeniedByUser {
		t.Errorf("应回传 deny 选项的 response 原文, got %v", resp["reason"])
	}
}

// TestPermissionClientCancelled: 客户端取消 → 合法 deny, 等待正常结束。
func TestPermissionClientCancelled(t *testing.T) {
	f, c, done := startPerm(t, permScript(zcodePermissionOptions()))

	permID, _ := waitPermission(t, c, 15*time.Second)
	c.write(map[string]any{
		"jsonrpc": "2.0", "id": permID,
		"result": map[string]any{"outcome": map[string]any{"outcome": "cancelled"}},
	})
	waitPromptEnd(t, done)

	assertDeny(t, zcodePermissionResponse(t, f))
}

// TestPermissionTimeout: 客户端不应答 → 超时后合法 deny, turn 收尾。
func TestPermissionTimeout(t *testing.T) {
	f, c, done := startPermEnv(t, permScript(zcodePermissionOptions()),
		map[string]string{"ZCODE_ACP_QUESTION_TIMEOUT": "2s"})

	// 请求到达后故意不应答, 等 ZCODE_ACP_QUESTION_TIMEOUT 走完。
	waitPermission(t, c, 15*time.Second)
	waitPromptEnd(t, done)

	assertDeny(t, zcodePermissionResponse(t, f))
}

// TestPermissionUnknownOptionID: 客户端返回未知 optionId → 不授予权限, 合法 deny。
func TestPermissionUnknownOptionID(t *testing.T) {
	f, c, done := startPerm(t, permScript(zcodePermissionOptions()))

	permID, _ := waitPermission(t, c, 15*time.Second)
	replyPermission(c, permID, "selected", "allow_everything")
	waitPromptEnd(t, done)

	assertDeny(t, zcodePermissionResponse(t, f))
}

// TestPermissionEmptyOptionID: selected 但没有 optionId → 同样不授予权限。
func TestPermissionEmptyOptionID(t *testing.T) {
	f, c, done := startPerm(t, permScript(zcodePermissionOptions()))

	permID, _ := waitPermission(t, c, 15*time.Second)
	replyPermission(c, permID, "selected", "")
	waitPromptEnd(t, done)

	assertDeny(t, zcodePermissionResponse(t, f))
}

// TestPermissionNoValidOptions: 没有有效选项 → 不合成允许决策, 不问客户端, 直接 deny。
func TestPermissionNoValidOptions(t *testing.T) {
	cases := []struct {
		name    string
		options []any
	}{
		{"空列表", []any{}},
		{"全部结构非法", []any{
			map[string]any{"optionId": "a"},                      // 缺 response
			map[string]any{"optionId": "b", "response": "allow"}, // response 非对象
			map[string]any{"optionId": "", "response": map[string]any{"decision": "allow"}},
			"not-an-object",
		}},
		{"decision 非法", []any{
			map[string]any{"optionId": "x", "kind": "allow_once",
				"response": map[string]any{"decision": "permit"}},
		}},
		{"permissionUpdates 结构非法", []any{
			map[string]any{"optionId": "y", "kind": "allow_always",
				"response": map[string]any{"decision": "allow", "permissionUpdates": []any{}}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c, done := startPerm(t, permScript(tc.options))
			waitPromptEnd(t, done)
			assertNoPermissionRequest(t, c)

			resp := zcodePermissionResponse(t, f)
			assertDeny(t, resp)
			if resp["decision"] == "allow" {
				t.Fatalf("不得合成允许决策: %v", resp)
			}
		})
	}
}

// TestPermissionMalformedOptionSkipped: 混入非法项时, 合法项照常投放并可用。
func TestPermissionMalformedOptionSkipped(t *testing.T) {
	options := []any{
		map[string]any{"optionId": "broken", "kind": "allow_once"}, // 缺 response
		zcodePermissionOptions()[0],                                // allow_once
		map[string]any{"optionId": "allow_once", "kind": "allow_once",
			"response": map[string]any{"decision": "allow"}}, // optionId 重复
	}
	f, c, done := startPerm(t, permScript(options))

	permID, params := waitPermission(t, c, 15*time.Second)
	opts := acpOptionsOf(t, params)
	if len(opts) != 1 || opts[0]["optionId"] != "allow_once" {
		t.Fatalf("只应投放合法项且去重: %v", opts)
	}
	replyPermission(c, permID, "selected", "allow_once")
	waitPromptEnd(t, done)

	resp := zcodePermissionResponse(t, f)
	if resp["decision"] != "allow" || resp["reason"] != "Approved once" {
		t.Fatalf("合法项应可用: %v", resp)
	}
}

// TestPermissionUnrepresentableDecisionNotOffered: escalate/modify 在 ACP kind 闭集
// 里没有对应槽位, 投放出来会让用户以为点了「拒绝」而实际是别的决策 —— 不投放;
// 全都不可表达时直接 deny。
func TestPermissionUnrepresentableDecisionNotOffered(t *testing.T) {
	t.Run("与合法项并存时只丢不可表达项", func(t *testing.T) {
		options := []any{
			map[string]any{"optionId": "esc", "kind": "custom",
				"response": map[string]any{"decision": "escalate", "reason": "need admin"}},
			zcodePermissionOptions()[2], // deny
		}
		f, c, done := startPerm(t, permScript(options))
		permID, params := waitPermission(t, c, 15*time.Second)
		opts := acpOptionsOf(t, params)
		if len(opts) != 1 || opts[0]["optionId"] != "deny" {
			t.Fatalf("escalate 不应投放: %v", opts)
		}
		replyPermission(c, permID, "selected", "deny")
		waitPromptEnd(t, done)
		assertDeny(t, zcodePermissionResponse(t, f))
	})

	t.Run("只有不可表达项时直接 deny", func(t *testing.T) {
		options := []any{
			map[string]any{"optionId": "mod", "kind": "custom",
				"response": map[string]any{"decision": "modify", "modifiedInput": map[string]any{"command": "ls"}}},
		}
		f, c, done := startPerm(t, permScript(options))
		waitPromptEnd(t, done)
		assertNoPermissionRequest(t, c)
		assertDeny(t, zcodePermissionResponse(t, f))
	})
}
