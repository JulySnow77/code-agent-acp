package e2e

import "testing"

// TestResumeKeepsSessionWorkspace: resume 省略 cwd 时, 桥必须沿用会话创建时的
// 工作区。
//
// 为什么这条值得单独盯着: 旧实现在没有 cwd 时填字面量 ".", 而 workspacePath 是
// 交给 zcode 当会话工作区根的 —— "." 会被 zcode 按**它自己的进程 cwd** 解析,
// 那是 exec.Command 没设 Dir 时继承来的桥启动目录。于是客户端 resume 时不传
// cwd, 会话的工作区就悄无声息地漂到了别的目录: 不报错, 模型照样干活, 只是干的
// 不是那个目录。
func TestResumeKeepsSessionWorkspace(t *testing.T) {
	const (
		origCwd   = "/proj/original"
		sessionID = "s-cwd"
	)
	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{
			"省略 cwd → 沿用会话创建时的目录",
			map[string]any{"sessionId": sessionID},
			origCwd,
		},
		{
			"显式给了别的目录 → 尊重这一次给的值",
			map[string]any{"sessionId": sessionID, "cwd": "/proj/moved"},
			"/proj/moved",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := startScripted(t, fakeScript{
				Responses: map[string]fakeAction{
					"session/create": {Result: map[string]any{"session": map[string]any{"sessionId": sessionID}}},
					"session/resume": {Result: map[string]any{"session": map[string]any{"sessionId": sessionID}}},
				},
			})
			f.client.request(1, "initialize", map[string]any{})
			if r := f.client.request(2, "session/new", map[string]any{
				"cwd": origCwd, "mode": "build",
			}); r["error"] != nil {
				t.Fatalf("session/new 失败: %v", r["error"])
			}
			if r := f.client.request(3, "session/resume", c.params); r["error"] != nil {
				t.Fatalf("session/resume 失败: %v", r["error"])
			}

			req := f.waitForLog(t, "session/resume")
			ws, _ := req["params"].(map[string]any)["workspace"].(map[string]any)
			if ws == nil {
				t.Fatalf("resume 请求里没有 workspace: %v", req["params"])
			}
			if got := ws["workspacePath"]; got != c.want {
				t.Errorf("resume 的 workspacePath = %v, want %v", got, c.want)
			}
			if got := ws["workspaceKey"]; got != c.want {
				t.Errorf("resume 的 workspaceKey = %v, want %v", got, c.want)
			}
		})
	}
}
