package e2e

import "testing"

// TestSessionNewDefaultMode 是「连接 zcode 默认 yolo」的可回归保证。
//
// session/new 不显式给 mode 时, 桥必须用缺省模式建会话, 并把该 mode 原样交给
// zcode 的 session/create: 缺省解析链为 客户端显式 mode → ZCODE_ACP_MODE
// (serve --mode 落到此) → 兼容旧 ZCODE_ACP_DEFAULT_MODE → yolo。
//
// 这条专门钉住之前埋的坑: 文档/--mode/doctor 都说 ZCODE_ACP_MODE, 但 session/new
// 只认 ZCODE_ACP_DEFAULT_MODE, 导致 ZCODE_ACP_MODE=build 静默无效、会话仍 yolo。
func TestSessionNewDefaultMode(t *testing.T) {
	const sessionID = "s-mode"
	script := fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{"session": map[string]any{"sessionId": sessionID}}},
		},
	}

	cases := []struct {
		name         string
		env          map[string]string // 注入 ZCODE_ACP_MODE / ZCODE_ACP_DEFAULT_MODE
		explicitMode string            // 客户端显式给的 mode; 空表示不给
		want         string
	}{
		{"不给任何变量 → yolo", nil, "", "yolo"},
		{"ZCODE_ACP_MODE=build → build", map[string]string{"ZCODE_ACP_MODE": "build"}, "", "build"},
		{"仅 ZCODE_ACP_DEFAULT_MODE=plan → plan", map[string]string{"ZCODE_ACP_DEFAULT_MODE": "plan"}, "", "plan"},
		{"ZCODE_ACP_MODE 优先于 DEFAULT", map[string]string{"ZCODE_ACP_MODE": "edit", "ZCODE_ACP_DEFAULT_MODE": "plan"}, "", "edit"},
		{"显式 mode=plan 覆盖缺省", nil, "plan", "plan"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := startScriptedEnv(t, script, c.env)
			f.client.request(1, "initialize", map[string]any{})

			params := map[string]any{"cwd": "/tmp"}
			if c.explicitMode != "" {
				params["mode"] = c.explicitMode
			}
			resp := f.client.request(2, "session/new", params)
			if resp["error"] != nil {
				t.Fatalf("session/new 失败: %v", resp["error"])
			}

			// 1) 桥把 mode 原样交给 zcode。
			if got := modeOfCreate(t, f); got != c.want {
				t.Errorf("zcode session/create 的 mode = %q, want %q", got, c.want)
			}
			// 2) 桥投回客户端的 currentModeId 也一致。
			res, _ := resp["result"].(map[string]any)
			modes, _ := res["modes"].(map[string]any)
			if modes["currentModeId"] != c.want {
				t.Errorf("ACP 响应 modes.currentModeId = %v, want %q", modes["currentModeId"], c.want)
			}
		})
	}
}

// modeOfCreate 从 fake 收到的请求里取出 session/create 的 mode。
func modeOfCreate(t *testing.T, f *scriptedFake) string {
	t.Helper()
	for _, r := range f.loggedRequests(t) {
		if m, _ := r["method"].(string); m == "session/create" {
			params, _ := r["params"].(map[string]any)
			mode, _ := params["mode"].(string)
			return mode
		}
	}
	t.Fatalf("fake 未收到 session/create")
	return ""
}
