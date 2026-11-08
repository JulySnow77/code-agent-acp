//go:build real

// Package smoke 对真实 ZCode CLI 的冒烟测试 (PORTING_PLAN §7 真机层)。
//
// 只在显式要求时运行 (需要本机 ZCode 与可用模型, 冷启动 20s 量级):
//
//	ZCODE_BIN=/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs \
//	  go test -tags=real ./test/smoke -v -timeout 10m
//
// 可选 ZCODE_ACP_MODEL=providerId/modelId 指定模型。
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
	"testing"
	"time"
)

// TestSmokeRealZcode: initialize → session/new → prompt → 期望流式 chunk 与
// end_turn (turn.failed 时错误可见, 测试同样通过 —— 断言的是「失败面被呈现」
// 而非「模型必然成功」; 想验成功请配 ZCODE_ACP_MODEL 为已知可用模型)。
func TestSmokeRealZcode(t *testing.T) {
	bin := os.Getenv("ZCODE_BIN")
	if bin == "" {
		t.Skip("ZCODE_BIN 未设置 (真机冒烟需要)")
	}

	bridge := filepath.Join(t.TempDir(), "zcode-acp-bridge")
	out, err := exec.Command("go", "build", "-o", bridge,
		"github.com/tizerluo/zcode-open-bridge-go/cmd/zcode-acp-bridge").CombinedOutput()
	if err != nil {
		t.Fatalf("构建失败: %v\n%s", err, out)
	}

	cmd := exec.Command(bridge, "serve")
	cmd.Env = os.Environ()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	go func() { _, _ = io.Copy(os.Stderr, stderr) }() // 诊断直接透传

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)

	send := func(id int, method string, params any) {
		t.Helper()
		bts, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := stdin.Write(append(bts, '\n')); err != nil {
			t.Fatalf("写 %s: %v", method, err)
		}
	}
	awaitResp := func(id int) map[string]any {
		t.Helper()
		deadline := time.Now().Add(5 * time.Minute) // 真机建会话可达 20s+, turn 更长
		for {
			if !sc.Scan() {
				t.Fatal("stdout 关闭")
			}
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if fmt.Sprint(m["id"]) == fmt.Sprint(id) && m["method"] == nil {
				return m
			}
			// 通知 (session/update 等) 打日志后继续。
			t.Logf("  ⇐ %v", strings.TrimSpace(fmt.Sprintf("%.200v", m)))
			if time.Now().After(deadline) {
				t.Fatal("等待响应超时")
			}
		}
	}

	send(1, "initialize", map[string]any{"protocolVersion": 1})
	if m := awaitResp(1); m["error"] != nil {
		t.Fatalf("initialize 失败: %v", m["error"])
	}
	t.Log("✓ initialize")

	send(2, "session/new", map[string]any{"cwd": "/tmp", "mode": "yolo"})
	m := awaitResp(2)
	if m["error"] != nil {
		t.Fatalf("session/new 失败 (doctor 可诊断): %v", m["error"])
	}
	sid := m["result"].(map[string]any)["sessionId"].(string)
	t.Logf("✓ session/new → %s", sid)

	send(3, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []map[string]any{{"type": "text", "text": "只回复两个字: 收到"}},
	})
	m = awaitResp(3)
	if m["error"] != nil {
		// D4: 错误可见是预期行为 (模型未配/限流时), 打印并结束。
		t.Logf("⚠ session/prompt 返回错误 (错误可见是 D4 预期): %v", m["error"])
		return
	}
	stop := m["result"].(map[string]any)["stopReason"]
	if stop != "end_turn" {
		t.Errorf("stopReason = %v, want end_turn", stop)
	}
	t.Logf("✓ session/prompt → %v", stop)
}
