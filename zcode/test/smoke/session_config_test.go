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

// TestSmokeRealSessionConfig 在真实 ZCode 上验证 T04 的标准面补齐:
// session/new 投放 modes + configOptions (模型目录), session/set_mode 与
// session/set_config_option 往返生效。
//
// fake 契约测试 (test/e2e/session_config_test.go) 证明桥的投影与应答正确;
// 这里证明**真实后端的 settings 形状与假设一致** (settings.mode.current /
// settings.model.current), 以及 zcode 接受这两个标准方法背后的 zcode 调用。
//
// 前置同 TestSmokeRealBuildTask:
//
//	python3 scripts/make-smoke-provider-config.py    # 打印 providerId 与模型
//	export ZCODE_BIN=/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs
//	export ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json
//	export ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=/tmp/zcode-smoke-provider.json
//	export ZCODE_ACP_MODEL=<providerId>/GLM-5.3-Flash
//
// 可选 ZCODE_ACP_BRIDGE_BIN 直接驱动已安装的二进制 (默认从源码构建)。
//
//	go test -count=1 -tags=real ./test/smoke -run TestSmokeRealSessionConfig -v -timeout 10m
//
// **-count=1 不能省** (见 TestSmokeRealPermission 的说明)。
func TestSmokeRealSessionConfig(t *testing.T) {
	if os.Getenv("ZCODE_BIN") == "" {
		t.Skip("ZCODE_BIN 未设置 (真机会话配置验证需要)")
	}
	bridge := bridgeBinOverride()
	if bridge == "" {
		bridge = filepath.Join(t.TempDir(), "zcode-acp-bridge")
		if out, err := exec.Command("go", "build", "-o", bridge,
			"github.com/tizerluo/zcode-open-bridge-go/cmd/zcode-acp-bridge").CombinedOutput(); err != nil {
			t.Fatalf("构建失败: %v\n%s", err, out)
		}
	}

	cmd := exec.Command(bridge, "serve", "-v")
	cmd.Env = os.Environ()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
	updates := make(chan map[string]any, 16)

	write := func(frame map[string]any) {
		bts, _ := json.Marshal(frame)
		if _, err := stdin.Write(append(bts, '\n')); err != nil {
			t.Errorf("写帧失败: %v", err)
		}
	}
	request := func(id, method string, params any) map[string]any {
		t.Helper()
		ch := make(chan map[string]any, 1)
		mu.Lock()
		pending[id] = ch
		mu.Unlock()
		write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		select {
		case m := <-ch:
			return m
		case <-time.After(8 * time.Minute):
			t.Fatalf("等待 %s 超时", method)
			return nil
		}
	}
	waitUpdate := func(t *testing.T, match func(map[string]any) bool) map[string]any {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for {
			select {
			case u := <-updates:
				if match(u) {
					return u
				}
			case <-deadline:
				t.Fatal("等待 session/update 超时")
				return nil
			}
		}
	}

	go func() {
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if m["method"] == "session/update" {
				params, _ := m["params"].(map[string]any)
				if u, ok := params["update"].(map[string]any); ok {
					select {
					case updates <- u:
					default:
					}
				}
				continue
			}
			if m["method"] != nil {
				continue // 其它通知
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

	if m := request("1", "initialize", map[string]any{"protocolVersion": 1}); m["error"] != nil {
		t.Fatalf("initialize 失败: %v", m["error"])
	}
	m := request("2", "session/new", map[string]any{
		"cwd": t.TempDir(), "mode": "build",
	})
	if m["error"] != nil {
		t.Fatalf("session/new 失败 (doctor 可诊断): %v", m["error"])
	}
	result, _ := m["result"].(map[string]any)
	sid, _ := result["sessionId"].(string)
	t.Logf("✓ session/new → %s", sid)

	// modes: 后端 settings.mode.current 或请求值兜底, 两者都必须是合法模式。
	modes, _ := result["modes"].(map[string]any)
	if modes == nil {
		t.Fatalf("session/new 未投放 modes: %v", result)
	}
	current, _ := modes["currentModeId"].(string)
	switch current {
	case "build", "edit", "plan", "yolo":
		t.Logf("✓ modes.currentModeId = %s", current)
	default:
		t.Errorf("modes.currentModeId = %q, 不是 zcode 的合法模式", current)
	}
	avail, _ := modes["availableModes"].([]any)
	if len(avail) != 4 {
		t.Errorf("availableModes = %d 项, want 4 (build/edit/plan/yolo)", len(avail))
	}

	// configOptions: 真机要证的核心事实 —— 后端 settings.model.current 确实
	// 存在且能对上目录。目录为空或缺 current 时桥省略该字段, 那也是要报的偏差。
	co, _ := result["configOptions"].([]any)
	if len(co) != 1 {
		stop()
		t.Fatalf("configOptions 未投放 (后端 settings.model 缺 current?): %v", result)
	}
	cfg, _ := co[0].(map[string]any)
	currentModel, _ := cfg["currentValue"].(string)
	options, _ := cfg["options"].([]any)
	t.Logf("✓ configOptions: %d 个模型, 当前 %s", len(options), currentModel)
	if currentModel == "" || len(options) == 0 {
		stop()
		t.Fatalf("configOptions 形状异常: %v", cfg)
	}
	// ZCODE_ACP_MODEL 生效时, 投放的「当前模型」必须是它 —— configOptions
	// 从 setModel 之后的 settings 派生, 不是 create 时的旧值 (真机踩过)。
	if env := os.Getenv("ZCODE_ACP_MODEL"); env != "" {
		if i := strings.Index(env, "/"); i >= 0 {
			for _, raw := range options {
				o, _ := raw.(map[string]any)
				if o["id"] == env {
					if currentModel != env {
						stop()
						t.Errorf("currentValue = %s, want %s (ZCODE_ACP_MODEL 已生效, 展示却还是旧值)", currentModel, env)
					}
					break
				}
			}
		}
	}

	// set_mode: 标准 {sessionId, modeId} → zcode setMode → current_mode_update。
	if m := request("3", "session/set_mode", map[string]any{
		"sessionId": sid, "modeId": "plan",
	}); m["error"] != nil {
		t.Fatalf("session/set_mode 失败: %v", m["error"])
	}
	upd := waitUpdate(t, func(u map[string]any) bool {
		return u["sessionUpdate"] == "current_mode_update"
	})
	if upd["currentModeId"] != "plan" {
		t.Errorf("current_mode_update = %v, want plan", upd["currentModeId"])
	}
	t.Logf("✓ set_mode plan → current_mode_update (%v)", upd["currentModeId"])

	// set_config_option: 换到目录里另一个模型 (只有一个模型时跳过, 核心断言
	// 已在上面)。zcode 侧拒绝 (模型不可用等) 会以 -32603 浮出, 同样算证据。
	var target string
	for _, raw := range options {
		o, _ := raw.(map[string]any)
		if id, _ := o["id"].(string); id != "" && id != currentModel {
			target = id
			break
		}
	}
	if target == "" {
		t.Log("ℹ 目录只有一个模型, 跳过 set_config_option 切换")
		stop()
		return
	}
	m = request("4", "session/set_config_option", map[string]any{
		"sessionId": sid, "configId": "model", "value": target,
	})
	if m["error"] != nil {
		t.Fatalf("set_config_option(%s) 失败: %v", target, m["error"])
	}
	res, _ := m["result"].(map[string]any)
	if co, _ := res["configOptions"].([]any); len(co) == 1 {
		if got, _ := co[0].(map[string]any)["currentValue"].(string); got != target {
			t.Errorf("切换后 currentValue = %s, want %s", got, target)
		}
	}
	upd = waitUpdate(t, func(u map[string]any) bool {
		if u["sessionUpdate"] != "config_option_update" {
			return false
		}
		co, _ := u["configOptions"].([]any)
		return len(co) == 1 && co[0].(map[string]any)["currentValue"] == target
	})
	t.Logf("✓ set_config_option → %s (config_option_update 已推)", updCo(upd))
	stop()
}

// updCo 从 config_option_update 里取当前值, 只为日志。
func updCo(u map[string]any) string {
	co, _ := u["configOptions"].([]any)
	if len(co) == 1 {
		s, _ := co[0].(map[string]any)["currentValue"].(string)
		return s
	}
	return "(none)"
}
