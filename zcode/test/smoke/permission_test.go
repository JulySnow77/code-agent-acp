//go:build real

package smoke

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSmokeRealPermission 在真实 ZCode 上走通工具权限应答 (T01 真机层)。
//
// 契约测试 (test/e2e/permission_test.go) 用 fake 证明桥的转换正确; 这里证明
// 真实后端接受这个形状: build 模式下让模型真的调一次工具, 客户端按
// ZCODE_ACP_SMOKE_PERMISSION_OPTION 选一个选项, 再从线材日志里读出**桥回给
// zcode 的那一包**并断言它是合法 PermissionResponse。
//
// 前置: CLI 认的 provider 配置和 App 登录写进 config.json 的凭证不是一回事
// (CLI 不读 config.json 的 provider map)。先生成一份 personal provider 配置:
//
//	python3 scripts/make-smoke-provider-config.py    # 打印 providerId 与模型
//
// 然后 (三个分支各跑一次, 换 ZCODE_ACP_SMOKE_PERMISSION_OPTION):
//
//	export ZCODE_BIN=/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs
//	export ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json
//	export ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=/tmp/zcode-smoke-provider.json
//	export ZCODE_ACP_MODEL=<providerId>/GLM-5.3-Flash
//	export ZCODE_ACP_SMOKE_PERMISSION_OPTION=allow_once   # 或 allow_project / deny
//	go test -count=1 -tags=real ./test/smoke -run TestSmokeRealPermission -v -timeout 10m
//
// **-count=1 不能省**: 换环境变量重跑时 go test 有时会命中缓存直接回上次结果
// (输出里出现 `(cached)`), 看着像跑过了其实后端根本没启。
//
// 可选: ZCODE_ACP_SMOKE_PERMISSION_PROMPT 覆盖默认提示词,
// ZCODE_ACP_SMOKE_PERMISSION_WORKSPACE 指定工作区 (默认临时目录 —— 持久允许
// 分支会把规则写进 ~/.zcode/cli/db 的 project 作用域, 用独立工作区便于事后清理)。
func TestSmokeRealPermission(t *testing.T) {
	bin := os.Getenv("ZCODE_BIN")
	if bin == "" {
		t.Skip("ZCODE_BIN 未设置 (真机权限验证需要)")
	}
	want := strings.TrimSpace(permissionOption())
	if want == "" {
		t.Skip("ZCODE_ACP_SMOKE_PERMISSION_OPTION 未设置 (要选哪个权限选项)")
	}
	workspace := strings.TrimSpace(permissionWorkspace())
	if workspace == "" {
		workspace = t.TempDir()
	}
	// 命令必须**不是**只读的。ZCode 对只读 bash 命令降级能力
	// (handlers/bash.ts resolveBashPermissionCapability →
	// isRuntimeReadOnlyBashCommand)，build 模式直接 allow("mode.build.readOnly")，
	// 根本不会发起权限请求。`echo` 在只读白名单里
	// (bash-readonly-policy-simple-commands.ts 的 allowAnyArgs)，
	// 用它当探针会永远等不到 requestPermission。`touch` 有副作用，必然被 gate。
	probeFile := filepath.Join(workspace, "t01-permission-probe.txt")
	prompt := strings.TrimSpace(permissionPrompt())
	if prompt == "" {
		prompt = "请用 Bash 工具运行这条命令并把它的输出原样告诉我: touch " + probeFile +
			"。必须真的调用工具，不要只用文字回答。"
	}

	bridge := filepath.Join(t.TempDir(), "zcode-acp-bridge")
	if out, err := exec.Command("go", "build", "-o", bridge,
		"github.com/tizerluo/zcode-open-bridge-go/cmd/zcode-acp-bridge").CombinedOutput(); err != nil {
		t.Fatalf("构建失败: %v\n%s", err, out)
	}
	wirePath := filepath.Join(t.TempDir(), "wire.jsonl")

	cmd := exec.Command(bridge, "serve", "-v")
	cmd.Env = append(os.Environ(), "ZCODE_ACP_WIRE_LOG="+wirePath)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// 线材日志要等进程退出才刷完, 所以断言之前先停 serve (defer 兜底重复调用)。
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	defer stop()
	go func() { _, _ = io.Copy(os.Stderr, stderr) }()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)

	var mu sync.Mutex
	pending := map[string]chan map[string]any{}
	permCh := make(chan map[string]any, 1)

	write := func(frame map[string]any) {
		t.Helper()
		bts, _ := json.Marshal(frame)
		if _, err := stdin.Write(append(bts, '\n')); err != nil {
			t.Errorf("写帧失败: %v", err)
		}
	}
	// request 注册通道后才发帧; 通道由 reader 在投递时摘除 —— await 侧
	// 不能提前 delete, 否则响应到达时已经没有投递目标 (真实后端冷启动
	// 就有 20s+, 这个竞态必然触发)。
	request := func(id, method string, params any) {
		t.Helper()
		ch := make(chan map[string]any, 1)
		mu.Lock()
		pending[id] = ch
		mu.Unlock()
		write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	await := func(id, what string) map[string]any {
		t.Helper()
		mu.Lock()
		ch := pending[id]
		mu.Unlock()
		if ch == nil {
			t.Fatalf("%s: 没有为 id=%s 注册的通道", what, id)
		}
		// 真机建会话可达 20s+, turn 更长。
		select {
		case m := <-ch:
			return m
		case <-time.After(8 * time.Minute):
			t.Fatalf("等待 %s 超时", what)
			return nil
		}
	}

	go func() {
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if m["method"] == "session/request_permission" {
				params, _ := m["params"].(map[string]any)
				select {
				case permCh <- map[string]any{"id": m["id"], "params": params}:
				default:
				}
				continue
			}
			if m["method"] != nil {
				continue // 通知
			}
			key := fmt.Sprint(m["id"])
			mu.Lock()
			ch := pending[key]
			delete(pending, key)
			mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}()

	request("1", "initialize", map[string]any{"protocolVersion": 1})
	if m := await("1", "initialize"); m["error"] != nil {
		t.Fatalf("initialize 失败: %v", m["error"])
	}
	request("2", "session/new", map[string]any{"cwd": workspace, "mode": "build"})
	m := await("2", "session/new")
	if m["error"] != nil {
		t.Fatalf("session/new 失败 (doctor 可诊断): %v", m["error"])
	}
	sid, _ := m["result"].(map[string]any)["sessionId"].(string)
	t.Logf("✓ session/new (mode=build, workspace=%s) → %s", workspace, sid)

	// prompt 在飞, 等真实后端发起权限请求。
	go request("3", "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	})

	var perm map[string]any
	select {
	case perm = <-permCh:
	case <-time.After(8 * time.Minute):
		stop()
		t.Fatal("后端在 build 模式下没有发起权限请求 (是否根本没调工具?)")
	}
	params, _ := perm["params"].(map[string]any)
	options, _ := params["options"].([]any)
	if len(options) == 0 {
		stop()
		t.Fatalf("后端没给任何权限选项: %v", params)
	}
	// 桥把 zcode 的 toolName 投到 toolCall.title/name, riskLevel/reason 投到
	// _meta.zcode —— 不在 ACP params 顶层 (顶层是 ACP 自己的形状)。
	toolCall, _ := params["toolCall"].(map[string]any)
	rawMeta, _ := params["_meta"].(map[string]any)
	zmeta, _ := rawMeta["zcode"].(map[string]any)
	if toolCall == nil || zmeta == nil {
		t.Errorf("桥没投 toolCall/_meta.zcode: %v", params)
	}
	t.Logf("  后端权限请求: toolCallId=%v name=%v title=%v",
		toolCall["toolCallId"], toolCall["name"], toolCall["title"])
	t.Logf("  _meta.zcode: toolName=%v riskLevel=%v reason=%v",
		zmeta["toolName"], zmeta["riskLevel"], zmeta["reason"])
	if zmeta["toolName"] == "" {
		t.Errorf("_meta.zcode.toolName 为空, 桥没把 zcode 的 toolName 带过来")
	}
	found := false
	for _, raw := range options {
		o, _ := raw.(map[string]any)
		t.Logf("  选项 optionId=%v kind=%v name=%v", o["optionId"], o["kind"], o["name"])
		if o["optionId"] == want {
			found = true
		}
	}
	if !found {
		stop()
		t.Fatalf("后端没有 optionId=%q 的选项 (只有上面这些)", want)
	}
	write(map[string]any{
		"jsonrpc": "2.0", "id": perm["id"],
		"result": map[string]any{"outcome": map[string]any{
			"outcome": "selected", "optionId": want,
		}},
	})
	t.Logf("✓ 客户端选择 %s", want)

	final := await("3", "session/prompt")
	if final["error"] != nil {
		// D4 错误可见是预期行为; 拒绝分支下模型可能因此报错, 不影响契约断言。
		t.Logf("℠ session/prompt 返回错误 (D4 错误可见是预期): %v", final["error"])
	} else {
		t.Logf("✓ session/prompt → %v", final["result"].(map[string]any)["stopReason"])
	}

	// 桥回给 zcode 的那一包 —— 真机上真正要验证的东西。
	stop()
	assertRealPermissionReply(t, wirePath, want, workspace)
}

// assertRealPermissionReply 从线材日志里取出桥对 interaction/requestPermission
// 的应答并断言其形状。旧实现的 {action, optionId} 会被 zcode 的 strict schema 拒掉。
func assertRealPermissionReply(t *testing.T, wirePath, want, workspace string) {
	t.Helper()
	raw, err := os.ReadFile(wirePath)
	if err != nil {
		t.Fatalf("读线材日志失败: %v", err)
	}
	var reqID string
	var reply map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Dir  string         `json:"dir"`
			Kind string         `json:"kind"`
			Msg  map[string]any `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch {
		case rec.Dir == "zcode→bridge" && rec.Kind == "interaction/requestPermission":
			reqID = fmt.Sprint(rec.Msg["ID"])
		case rec.Dir == "bridge→zcode" && rec.Kind == "reply" && fmt.Sprint(rec.Msg["id"]) == reqID:
			reply, _ = rec.Msg["result"].(map[string]any)
		}
	}
	if reply == nil {
		t.Fatalf("线材日志里没有对 %s 的应答", reqID)
	}
	bts, _ := json.Marshal(reply)
	t.Logf("zcode 实际收到的 PermissionResponse: %s", bts)

	decision, _ := reply["decision"].(string)
	switch want {
	case "allow_project":
		if decision != "allow" {
			t.Errorf("持久允许应回 allow, got %v", decision)
		}
		updates, _ := reply["permissionUpdates"].([]any)
		if len(updates) == 0 {
			t.Errorf("持久允许必须带 permissionUpdates, 实际: %s", bts)
		}
	case "allow_once", "allowSession":
		if decision != "allow" {
			t.Errorf("一次/会话允许应回 allow, got %v", decision)
		}
		if _, has := reply["permissionUpdates"]; has {
			t.Errorf("一次/会话允许不应带 permissionUpdates: %s", bts)
		}
	case "deny":
		if decision != "deny" {
			t.Errorf("拒绝应回 deny, got %v", decision)
		}
	default:
		t.Logf("ℹ 未识别的选项 %q, 只记录不断言", want)
	}
	if _, bad := reply["action"]; bad {
		t.Errorf("不应回旧的 {action, optionId} 形状: %s", bts)
	}
	t.Logf("ℹ 工作区 %s (持久规则落在 ~/.zcode/cli/db 的 project 作用域)", workspace)
}
