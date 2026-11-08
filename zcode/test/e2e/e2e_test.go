// Package e2e 对完整二进制做端到端验证: ACP client (测试) → zcode-acp-bridge
// serve → fake zcode app-server (test/fakepy)。这是 PORTING_PLAN §7 的
// 「假服务 e2e」层, Phase 2 的验收测试。
package e2e

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

var bridgeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zbridge-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: 临时目录失败:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	bridgeBin = filepath.Join(dir, "zcode-acp-bridge")
	buildArgs := []string{"build"}
	if os.Getenv("ZCODE_E2E_RACE") == "1" {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, "-o", bridgeBin, "github.com/tizerluo/zcode-open-bridge-go/cmd/zcode-acp-bridge")
	out, err := exec.Command("go", buildArgs...).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: 构建失败: %v\n%s\n", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// acpClient 驱动 serve 进程: 写 ACP 帧, 读响应与通知。
type acpClient struct {
	t         *testing.T
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Scanner
	frames    chan map[string]any
	pending   map[string]chan map[string]any
	pendingMu sync.Mutex
}

func startBridge(t *testing.T) *acpClient {
	t.Helper()
	return startBridgeEnv(t, nil)
}

// startBridgeEnv 启动 serve 并注入额外环境变量 (Go fake 场景传脚本与日志路径)。
func startBridgeEnv(t *testing.T, extra map[string]string) *acpClient {
	t.Helper()
	fake := fakeBin(t)
	// provider 配置: serve 的启动自检要求能定位它 (C1); fake 不读, 造个最小的。
	providerDir := t.TempDir()
	providerFile := filepath.Join(providerDir, "zcode-builtin.json")
	if err := os.WriteFile(providerFile, []byte(`{"providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(),
		"ZCODE_BIN="+fake,
		"ZCODE_BUILTIN_PROVIDER_CONFIG_FILE="+providerFile,
	)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command(bridgeBin, "serve")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// stderr: ZCODE_E2E_VERBOSE=1 时透传到测试输出 (诊断用), 否则丢弃。
	var stderrSink io.Writer = io.Discard
	if os.Getenv("ZCODE_E2E_VERBOSE") == "1" {
		stderrSink = os.Stderr
	}
	go func() { _, _ = io.Copy(stderrSink, stderr) }()

	c := &acpClient{
		t:       t,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewScanner(stdout),
		frames:  make(chan map[string]any, 256),
		pending: map[string]chan map[string]any{},
	}
	c.stdout.Buffer(make([]byte, 0, 64<<10), 16<<20)
	go c.readLoop()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return c
}

// request 发一条请求并等待对应 id 的响应 (通知顺带流入 frames)。
//
// 超时/写失败都返回 error 帧而不是 t.Fatalf: prompt 常在独立 goroutine 里发,
// 从非测试 goroutine 调 Fatalf 会 Goexit 掉整个测试二进制, 把其余用例的失败
// 一起吞掉。调用方本来就检查 resp["error"], 失败信息一样能到测试输出。
func (c *acpClient) request(id any, method string, params any) map[string]any {
	c.t.Helper()
	ch := make(chan map[string]any, 1)
	key := fmt.Sprint(id)
	c.pendingMu.Lock()
	c.pending[key] = ch
	c.pendingMu.Unlock()

	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := c.write(frame); err != nil {
		return requestFailed("写 %s 帧失败: %v", method, err)
	}
	select {
	case resp := <-ch:
		return resp
	case <-time.After(30 * time.Second):
		return requestFailed("等待 %s 响应超时", method)
	}
}

func requestFailed(format string, args ...any) map[string]any {
	return map[string]any{"error": map[string]any{
		"code": float64(-1), "message": fmt.Sprintf(format, args...),
	}}
}

func (c *acpClient) write(frame map[string]any) error {
	bts, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(bts, '\n'))
	return err
}

func (c *acpClient) readLoop() {
	for c.stdout.Scan() {
		line := strings.TrimSpace(c.stdout.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if id, ok := m["id"]; ok && m["method"] == nil {
			key := fmt.Sprint(id)
			c.pendingMu.Lock()
			ch := c.pending[key]
			delete(c.pending, key)
			c.pendingMu.Unlock()
			if ch != nil {
				ch <- m
				continue
			}
		}
		c.frames <- m
	}
	close(c.frames)
}

// waitUpdate 等待一条满足条件的 session/update 通知。
func (c *acpClient) waitUpdate(t *testing.T, timeout time.Duration,
	match func(update map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatal("serve stdout 已关闭")
			}
			if f["method"] == "session/update" {
				if params, _ := f["params"].(map[string]any); params != nil {
					if update, _ := params["update"].(map[string]any); update != nil {
						if match == nil || match(update) {
							return update
						}
					}
				}
			}
		case <-deadline:
			t.Fatal("等待 session/update 超时")
		}
	}
}

// ---- 测试 ----

// TestE2ETextTurn 全链路: initialize → session/new → session/prompt →
// 流式 chunk + end_turn (Phase 2 验收: 建会话、流式看到文本)。
func TestE2ETextTurn(t *testing.T) {
	c := startBridge(t)

	resp := c.request(1, "initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]any{"name": "e2e-test"}})
	if resp["error"] != nil {
		t.Fatalf("initialize 失败: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != float64(1) {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}

	resp = c.request(2, "session/new", map[string]any{"cwd": "/tmp", "mode": "yolo"})
	if resp["error"] != nil {
		t.Fatalf("session/new 失败: %v", resp["error"])
	}
	sid := resp["result"].(map[string]any)["sessionId"].(string)

	promptDone := make(chan map[string]any, 1)
	go func() {
		promptDone <- c.request(3, "session/prompt", map[string]any{
			"sessionId": sid,
			"prompt":    []map[string]any{{"type": "text", "text": "只回复两个字: 收到"}},
		})
	}()

	// 流式: fake 会推 turn.started → model.streaming(text_delta) → turn.completed。
	var chunks []string
	for {
		update := c.waitUpdate(t, 20*time.Second, func(u map[string]any) bool {
			return u["sessionUpdate"] == "agent_message_chunk"
		})
		text := update["content"].(map[string]any)["text"].(string)
		chunks = append(chunks, text)
		// fake 只发一段; 收到后等 prompt 结束。
		break
	}
	select {
	case resp := <-promptDone:
		if resp["error"] != nil {
			t.Fatalf("session/prompt 失败: %v", resp["error"])
		}
		if sr := resp["result"].(map[string]any)["stopReason"]; sr != "end_turn" {
			t.Errorf("stopReason = %v, want end_turn", sr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("session/prompt 超时")
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "hello from fake zcode") {
		t.Errorf("流式文本不含 fake 回复: %q", joined)
	}
}

// TestE2EAskPythonFake 是上游 test_ask_user_question_e2e.py 的 Go 等价物:
// Python fake 发 interaction/requestUserInput, 桥转成 session/request_permission,
// 客户端选 React → zcode (fake) 收到 accept + answers → turn 正常结束。
// fake 的 FAKE_ZCODE_STATE_FILE 记录 zcode 侧收到的字面应答, 供断言。
func TestE2EAskPythonFake(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	c := startBridgeEnv(t, map[string]string{"FAKE_ZCODE_STATE_FILE": stateFile})

	c.request(1, "initialize", map[string]any{})
	resp := c.request(2, "session/new", map[string]any{"cwd": "/tmp"})
	sid := resp["result"].(map[string]any)["sessionId"].(string)

	promptDone := make(chan map[string]any, 1)
	go func() {
		promptDone <- c.request(3, "session/prompt", map[string]any{
			"sessionId": sid,
			"prompt":    "ASK_USER_QUESTION",
		})
	}()

	// 等桥转来的权限请求并选 React。
	permID := ""
	deadline := time.After(15 * time.Second)
	for permID == "" {
		select {
		case fr := <-c.frames:
			if fr["method"] == "session/request_permission" {
				permID, _ = fr["id"].(string)
			}
		case <-deadline:
			t.Fatal("未收到 session/request_permission")
		}
	}
	c.write(map[string]any{
		"jsonrpc": "2.0", "id": permID,
		"result": map[string]any{"outcome": map[string]any{
			"outcome": "selected", "optionId": "React",
		}},
	})

	select {
	case resp := <-promptDone:
		if resp["error"] != nil {
			t.Fatalf("prompt 失败: %v", resp["error"])
		}
	case <-time.After(20 * time.Second):
		// fake 的 ANSWER_TIMEOUT_S=30s: 答案没送达时 fake 改推 turn.failed,
		// 桥按 D4 返回错误 —— 也是 20s 内的可见失败; 真挂死才会走到这里。
		t.Fatal("答案未送达 zcode (turn 挂死)")
	}

	// zcode 侧字面应答: {"action":"accept","content":{"answers":{…:"React"}}}。
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("fake 未写 state 文件: %v", err)
	}
	var state map[string]any
	if json.Unmarshal(raw, &state) != nil {
		t.Fatalf("state 文件非 JSON: %s", raw)
	}
	// fake 写的是桥回的完整应答帧: {id, jsonrpc, result:{action, content:{answers}}}。
	result, _ := state["result"].(map[string]any)
	if result == nil {
		t.Fatalf("state 缺 result: %s", raw)
	}
	content, _ := result["content"].(map[string]any)
	answers, _ := content["answers"].(map[string]any)
	if result["action"] != "accept" || answers["Which framework should we use?"] != "React" {
		t.Errorf("zcode 收到的应答异常: %s", raw)
	}
}

// TestE2EUnknownSession 验证 prompt 未知会话的失败面。
func TestE2EUnknownSession(t *testing.T) {
	c := startBridge(t)
	c.request(1, "initialize", map[string]any{})
	resp := c.request(2, "session/prompt", map[string]any{
		"sessionId": "no-such-session", "prompt": "hi",
	})
	e, ok := resp["error"].(map[string]any)
	if !ok || e["code"] != float64(-32603) {
		t.Errorf("未知会话应 -32603: %v", resp)
	}
}

// fakeBin 与 backend 测试同源: 上游 Python fake (Phase 2~3 复用)。
func fakeBin(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("..", "..", "test", "fakepy", "zcode_app_server"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("缺少 fake app-server: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("系统无 python3")
	}
	return bin
}
