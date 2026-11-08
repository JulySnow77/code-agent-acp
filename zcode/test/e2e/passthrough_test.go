package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeScript 描述一次 Go fake zcode 的行为脚本 (test/fakeserver)。
type fakeScript struct {
	Responses map[string]fakeAction `json:"responses"`
	Default   *fakeAction           `json:"default"`
}

type fakeAction struct {
	Result         map[string]any  `json:"result,omitempty"`
	Error          *fakeRPCError   `json:"error,omitempty"`
	Push           []fakePush      `json:"push,omitempty"`
	ServerRequests []fakeServerReq `json:"serverRequests,omitempty"`
	// DelayMs 写响应前先睡这么久 (fake 侧能力), 用来把时序窗口撑到确定性宽度。
	DelayMs int `json:"delayMs,omitempty"`
}

type fakeRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type fakePush struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// fakeServerReq: fake 主动发起的反向调用 (等桥应答后再推 then)。
type fakeServerReq struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	Then   []fakePush     `json:"then,omitempty"`
}

// scriptedFake 启动桥 (serve) 但把 ZCODE_BIN 指向 Go fake, 返回客户端与请求日志。
type scriptedFake struct {
	client *acpClient
	logMu  sync.Mutex
	log    string
}

func startScripted(t *testing.T, script fakeScript) *scriptedFake {
	t.Helper()
	return startScriptedEnv(t, script, nil)
}

// startScriptedEnv 在 startScripted 之上再注入环境变量 (超时等场景)。
func startScriptedEnv(t *testing.T, script fakeScript, extraEnv map[string]string) *scriptedFake {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("无 go 工具链")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	raw, _ := json.Marshal(script)
	if err := os.WriteFile(scriptPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "requests.jsonl")

	fakeBin := filepath.Join(dir, "fakeserver")
	if out, err := exec.Command("go", "build", "-o", fakeBin,
		"github.com/tizerluo/zcode-open-bridge-go/test/fakeserver").CombinedOutput(); err != nil {
		t.Fatalf("构建 fakeserver: %v\n%s", err, out)
	}

	// 复用 startBridge 的进程管理, 但注入 Go fake 与脚本。
	env := map[string]string{
		"ZCODE_BIN":         fakeBin,
		"FAKE_ZCODE_SCRIPT": scriptPath,
		"FAKE_ZCODE_LOG":    logPath,
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	fake := &scriptedFake{}
	fake.client = startBridgeEnv(t, env)
	fake.log = logPath
	t.Cleanup(func() {
		if t.Failed() {
			if raw, err := os.ReadFile(logPath); err == nil {
				t.Logf("fake 收到的请求:\n%s", raw)
			}
		}
	})
	return fake
}

// loggedRequests 读 fake 收到的全部请求。
func (f *scriptedFake) loggedRequests(t *testing.T) []map[string]any {
	t.Helper()
	f.logMu.Lock()
	defer f.logMu.Unlock()
	raw, err := os.ReadFile(f.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range splitLines(string(raw)) {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if seg := trimBlank(s[start:i]); seg != "" {
				out = append(out, seg)
			}
			start = i + 1
		}
	}
	if seg := trimBlank(s[start:]); seg != "" {
		out = append(out, seg)
	}
	return out
}

func trimBlank(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\r' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\r' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// waitForLog 等日志里出现目标方法 (fake 写日志在响应前, 但响应到达晚于写盘,
// 通常读一次即可; 保守起见带短重试)。
//
// 桥启动时的协议探测帧 (id="bridge-probe-1" 的 workspace/readState) 也会被
// 记进日志 —— 必须排除, 否则断言到的是探测帧的 cwd workspace。
func (f *scriptedFake) waitForLog(t *testing.T, method string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, req := range f.loggedRequests(t) {
			if req["method"] != method {
				continue
			}
			if id, _ := req["id"].(string); id == "bridge-probe-1" || id == "invalid-message" {
				continue
			}
			return req
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake 日志中未见 %s", method)
	return nil
}

// ---- Phase 3 透传用例 (移植上游 test_app_server_methods.py 的 M 系列) ----

// M6: workspace 选择器四形态 (dict / workspacePath / cwd / 兜底)。
func TestPassthroughWorkspaceResolve(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   map[string]any
	}{
		{"合法 dict 原样透传", map[string]any{
			"workspace": map[string]any{"workspacePath": "/p/a", "workspaceKey": "/p/a"},
		}, map[string]any{"workspacePath": "/p/a", "workspaceKey": "/p/a"}},
		{"workspacePath 构造", map[string]any{"workspacePath": "/p/b"},
			map[string]any{"workspacePath": "/p/b", "workspaceKey": "/p/b"}},
		{"cwd 构造", map[string]any{"cwd": "/p/c"},
			map[string]any{"workspacePath": "/p/c", "workspaceKey": "/p/c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := startScripted(t, fakeScript{
				Responses: map[string]fakeAction{
					"workspace/readState": {Result: map[string]any{"ok": true}},
				},
			})
			f.client.request(1, "initialize", map[string]any{})
			f.client.request(2, "workspace/readState", c.params)
			req := f.waitForLog(t, "workspace/readState")
			got, _ := req["params"].(map[string]any)["workspace"].(map[string]any)
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("workspace[%s] = %v, want %v (got=%v)", k, got[k], v, got)
				}
			}
		})
	}
}

// M1/M3/M5/M8: 必填参数缺失 → -32602, 且不触达后端。
func TestPassthroughRequiredParams(t *testing.T) {
	cases := []struct{ method string }{
		{"session/setThoughtLevel"},
		{"session/cancelBackgroundTask"},
		{"session/setMode"},
		{"workspace/generateText"},
		{"workspace/setDefaultModel"},
		{"workspace/setDefaultMode"},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			f := startScripted(t, fakeScript{}) // 无脚本: 一旦触达后端就返默认忽略, 测试会超时/空
			f.client.request(1, "initialize", map[string]any{})
			resp := f.client.request(2, c.method, map[string]any{})
			e, ok := resp["error"].(map[string]any)
			if !ok || e["code"] != float64(-32602) {
				t.Fatalf("缺参应 -32602: %v", resp)
			}
			for _, req := range f.loggedRequests(t) {
				if req["method"] == c.method {
					t.Fatalf("缺参请求不应触达后端: %v", req)
				}
			}
		})
	}
}

// 规则 passthrough-32601-removed: 后端 -32601 → 「当前版本已移除」文案, code 保持。
func TestPassthroughRemovedCapability(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/fork": {Error: &fakeRPCError{Code: -32601, Message: "method not implemented"}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "session/fork", map[string]any{
		"sessionId": "s", "target": map[string]any{"kind": "latestCheckpoint"},
	})
	e, _ := resp["error"].(map[string]any)
	if e == nil || e["code"] != float64(-32601) {
		t.Fatalf("-32601 应保持 code: %v", resp)
	}
	msg, _ := e["message"].(string)
	if msg == "" || !containsStr(msg, "已移除") {
		t.Errorf("文案应为「已移除」: %q", msg)
	}
}

// 其他后端错误 → -32603 短名形态 (session/fork → "zcode fork failed: …")。
func TestPassthroughBackendError(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/compact": {Error: &fakeRPCError{Code: -32000, Message: "prompt is running"}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "session/compact", map[string]any{"sessionId": "s"})
	e, _ := resp["error"].(map[string]any)
	if e == nil || e["code"] != float64(-32603) {
		t.Fatalf("应 -32603: %v", resp)
	}
	if msg, _ := e["message"].(string); msg != "zcode compact failed: prompt is running" {
		t.Errorf("短名文案不符: %q", msg)
	}
}

// D2 双名字: _zcode/session/fork 与 session/fork 等价。
func TestPassthroughAliasNames(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/fork": {Result: map[string]any{"sessionId": "forked-1"}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "_zcode/session/fork", map[string]any{"sessionId": "s"})
	if resp["error"] != nil {
		t.Fatalf("别名调用失败: %v", resp["error"])
	}
	if r, _ := resp["result"].(map[string]any); r["sessionId"] != "forked-1" {
		t.Errorf("别名响应不一致: %v", resp)
	}
	f.waitForLog(t, "session/fork")
}

// C4 setModel 适配器: 旧 {modelId} 升格为嵌套结构并注入缺省等级。
func TestPassthroughSetModelUpgrade(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{
				"session": map[string]any{"sessionId": "sess-upg"},
				"settings": map[string]any{"model": map[string]any{"available": []any{
					map[string]any{
						"ref": map[string]any{"providerId": "prov-1", "modelId": "glm-9"},
						"reasoning": map[string]any{"defaultLevel": "max",
							"levels": []any{map[string]any{"label": "max", "value": "max"}}},
					},
				}}},
			}},
			"session/setModel": {Result: map[string]any{"ok": true}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	f.client.request(2, "session/new", map[string]any{"cwd": "/tmp"})
	resp := f.client.request(3, "session/setModel", map[string]any{"sessionId": "sess-upg", "modelId": "glm-9"})
	if resp["error"] != nil {
		t.Fatalf("setModel 失败: %v", resp["error"])
	}
	req := f.waitForLog(t, "session/setModel")
	params, _ := req["params"].(map[string]any)
	model, _ := params["model"].(map[string]any)
	if model == nil || model["providerId"] != "prov-1" || model["modelId"] != "glm-9" {
		t.Fatalf("升格结果异常: %v", params)
	}
	if opts, _ := model["options"].(map[string]any); opts["reasoningLevel"] != "max" {
		t.Errorf("缺省等级未注入: %v", model)
	}
}

// enhance 同步: 透传并返回 {enhanced}。
func TestEnhanceSync(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"prompt/enhance": {Result: map[string]any{"enhanced": "更好的提示词"}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "prompt/enhance", map[string]any{
		"prompt": "原始提示词", "workspacePath": "/p",
	})
	if resp["error"] != nil {
		t.Fatalf("enhance 失败: %v", resp["error"])
	}
	if r, _ := resp["result"].(map[string]any); r["enhanced"] != "更好的提示词" {
		t.Errorf("enhanced 结果异常: %v", resp)
	}
	req := f.waitForLog(t, "prompt/enhance")
	ws, _ := req["params"].(map[string]any)["workspace"].(map[string]any)
	if ws["workspacePath"] != "/p" {
		t.Errorf("workspace 未注入: %v", req)
	}
}

// enhance/start 异步: start 响应后推送 prompt/enhance/result 通知 → 桥返回 enhanced。
func TestEnhanceStartResult(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{"session": map[string]any{"sessionId": "s-e"}}},
			"prompt/enhance/start": {Result: map[string]any{"accepted": true}, Push: []fakePush{
				{Method: "prompt/enhance/result", Params: map[string]any{
					"requestId": "job-1", "status": "completed", "enhanced": "增强完成",
				}},
			}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	done := make(chan map[string]any, 1)
	go func() {
		done <- f.client.request(2, "prompt/enhance/start", map[string]any{
			"prompt": "p", "requestId": "job-1", "workspacePath": "/p",
		})
	}()
	select {
	case resp := <-done:
		if resp["error"] != nil {
			t.Fatalf("enhance/start 失败: %v", resp["error"])
		}
		if r, _ := resp["result"].(map[string]any); r["enhanced"] != "增强完成" {
			t.Errorf("异步结果异常: %v", resp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("enhance/start 超时 (result 通知未路由)")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
