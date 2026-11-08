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

// TestSmokeRealPlanApproval 在真实 ZCode 上验证 T03 的核心修复: plan 审批
// (ExitPlanMode) 的答案必须是 value "approve", 不是 label "Approve"。
//
// 为什么这条值得单独上真机: ZCode 的 plan 审批走 interaction/requestUserInput
// (requestExitPlanModeApproval, interaction-broker.ts:269), 选项由
// createExitPlanModeApprovalQuestion (:326) 生成为
// {label:"Approve", value:"approve"} —— **label 与 value 故意不同**。
// planApprovalResponseToBrokerResult (:399) 经 normalizePlanApprovalAnswer 拿答案
// 与常量 EXIT_PLAN_MODE_APPROVAL_APPROVE ("approve", :40) **逐字**比对。
// 旧实现 optionId=label 且把 label 回传, 于是用户在界面上点「批准」, zcode 收到
// "Approve" ≠ "approve" → 判为未批准 →
// withPlanExitDeniedTurnStop (core/src/tool/executor/turn-control.ts:44) 把字面量
// "Approve" 当成用户反馈喂回模型。**批准计划会让 ZCode 告诉模型用户说了「Approve」。**
//
// fake fixture 里 value==label, 所以这条只有真机能证。契约层由
// test/e2e/elicitation_test.go 的 TestElicitationPlanApprovalValue 复刻同样的形状。
//
// 前置同 TestSmokeRealPermission:
//
//	python3 scripts/make-smoke-provider-config.py    # 打印 providerId 与模型
//	export ZCODE_BIN=/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs
//	export ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json
//	export ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=/tmp/zcode-smoke-provider.json
//	export ZCODE_ACP_MODEL=<providerId>/GLM-5.3-Flash
//	go test -count=1 -tags=real ./test/smoke -run TestSmokeRealPlanApproval -v -timeout 40m
//
// **-count=1 不能省** (见 TestSmokeRealPermission 的说明)。
//
// 两条问答路径各跑一次: 客户端不声明能力 (降级) 与声明 elicitation.form (协商)。
// 两条都要验, 因为 value 映射在两处各实现了一次。
//
// 批准后的**实施阶段**会触发工具授权 (真机证据: build 模式下 Write/Bash 都会
// 发 requestPermission)。首轮真机验证 (2026-09-26) 没有应答这些请求 —— 每条
// 都悬到桥的 10 分钟问答超时才被拒, 模型再重试其它命令, turn 就拖过了用例
// 的 10 分钟收尾上限。当时记的「批准后 turn 未返回」是**测试装置的洞**, 不是
// 桥的行为。现在读帧 goroutine 对非 plan 审批的问答自动应答 (权限回
// 「允许一次」, 问答表单回 cancel), 本用例只关心 plan 审批那一包。
func TestSmokeRealPlanApproval(t *testing.T) {
	if os.Getenv("ZCODE_BIN") == "" {
		t.Skip("ZCODE_BIN 未设置 (真机 plan 审批验证需要)")
	}
	for _, path := range []string{"fallback", "elicitation"} {
		t.Run(path, func(t *testing.T) {
			runPlanApproval(t, path)
		})
	}
}

// planApprovalQuestionText 是 zcode 用来做 plan 审批答案键的问题原文
// (常量 EXIT_PLAN_MODE_APPROVAL_QUESTION, interaction-broker.ts:39)。
const planApprovalQuestionText = "Review this implementation plan."

// planApprovalApproveValue 是 zcode 拿去逐字比对的批准值
// (常量 EXIT_PLAN_MODE_APPROVAL_APPROVE, interaction-broker.ts:40)。
const planApprovalApproveValue = "approve"

// planApprovalApproveLabel 是同一个选项给人看的名字。**与 value 故意不同** ——
// 这正是 T03 的 bug 藏身之处。
const planApprovalApproveLabel = "Approve"

// promptReqID 是 session/prompt 的请求 id。读帧的 goroutine 靠它把应答分流到
// promptDone (那个通道要在等审批时就 ready, 不能等到批准之后再注册)。
const promptReqID = "3"

func runPlanApproval(t *testing.T, path string) {
	t.Helper()
	workspace := t.TempDir()

	prompt := strings.TrimSpace(planApprovalPrompt())
	if prompt == "" {
		prompt = "请为「给 /tmp 写一个 hello world 脚本」这个任务写一份实现计划（3 步以内），" +
			"写完后**必须调用 ExitPlanMode 工具**把计划提交给我审批。必须真的调用工具，不要只用文字回答。"
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
	// promptDone 单独一个通道: 等审批时必须同时盯着它 —— turn 一旦失败
	// (模型不可用、网络错误), 再等下去只是白等满 8 分钟, 而且报的还是
	// 「模型没调 ExitPlanMode」这种误导性的错。
	// **读帧的 goroutine 必须真的往这里写** (见下面的 promptReqID 分支), 否则
	// 收审批时它永远不会触发, 批准后 `<-promptDone` 也会一直挂着。
	promptDone := make(chan map[string]any, 1)
	// askCh 收桥发来的**问答**请求 (降级是 session/request_permission,
	// 协商是 elicitation/create); 只收 plan 审批那一包 (含旧实现特征包),
	// 其它在读帧 goroutine 里自动应答 —— 见文件头说明。
	askCh := make(chan map[string]any, 4)
	permAnswered := 0

	write := func(frame map[string]any) {
		t.Helper()
		bts, _ := json.Marshal(frame)
		if _, err := stdin.Write(append(bts, '\n')); err != nil {
			t.Errorf("写帧失败: %v", err)
		}
	}
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
			switch m["method"] {
			case "session/request_permission", "elicitation/create":
				params, _ := m["params"].(map[string]any)
				method, _ := m["method"].(string)
				kind := planApprovalAskKind(method, params)
				if kind != "other" {
					select {
					case askCh <- map[string]any{"id": m["id"], "method": method, "params": params, "kind": kind}:
					default:
						t.Log("  (askCh 满, 丢弃一条问答请求)")
					}
					continue
				}
				// 非 plan 审批的问答: 权限回「允许一次」, 问答表单回 cancel,
				// 别让实施阶段的每条授权都悬满 10 分钟超时。
				id := fmt.Sprint(m["id"])
				if method != "session/request_permission" {
					t.Log("  ⇐ 实施阶段 elicitation 请求, 回 cancel")
					write(map[string]any{"jsonrpc": "2.0", "id": id,
						"result": map[string]any{"action": "cancel"}})
					continue
				}
				toolCall, _ := params["toolCall"].(map[string]any)
				title, _ := toolCall["title"].(string)
				if title == "AskUserQuestion" { // ask.go askOne 的降级问答走这个 title
					t.Log("  ⇐ 实施阶段问答请求, 回 cancel")
					write(map[string]any{"jsonrpc": "2.0", "id": id,
						"result": map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}})
					continue
				}
				optionID := pickAllowOnce(params)
				mu.Lock()
				permAnswered++
				n := permAnswered
				mu.Unlock()
				outcome := map[string]any{"outcome": "selected", "optionId": optionID}
				if optionID == "" {
					outcome = map[string]any{"outcome": "cancelled"}
				}
				t.Logf("  ⇐ 实施阶段权限请求 (%v), 回 allow_once (第 %d 次)", title, n)
				write(map[string]any{"jsonrpc": "2.0", "id": id,
					"result": map[string]any{"outcome": outcome}})
				continue
			case nil:
				// 落到下面按应答处理
			default:
				continue // 通知
			}
			key := fmt.Sprint(m["id"])
			// session/prompt 的应答单独走 promptDone: 等审批与等收尾都要盯它,
			// 走 pending 的话 channel 没人读, 应答会被丢在 buffered channel 里。
			if key == promptReqID {
				select {
				case promptDone <- m:
				default:
				}
				continue
			}
			mu.Lock()
			ch := pending[key]
			delete(pending, key)
			mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}()

	// 能力声明决定桥走哪条路。桥的判定是「elicitation.form 显式存在且非 null」。
	initParams := map[string]any{"protocolVersion": 1}
	if path == "elicitation" {
		initParams["clientCapabilities"] = map[string]any{
			"elicitation": map[string]any{"form": map[string]any{}},
		}
	}
	request("1", "initialize", initParams)
	if m := await("1", "initialize"); m["error"] != nil {
		t.Fatalf("initialize 失败: %v", m["error"])
	}
	request("2", "session/new", map[string]any{"cwd": workspace, "mode": "plan"})
	m := await("2", "session/new")
	if m["error"] != nil {
		t.Fatalf("session/new 失败: %v", m["error"])
	}
	sid, _ := m["result"].(map[string]any)["sessionId"].(string)
	t.Logf("✓ session/new (mode=plan, workspace=%s) → %s", workspace, sid)

	go request(promptReqID, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	})

	// 等 plan 审批请求。认它靠的是 zcode 自己的问题原文/取值, 不是方法名 ——
	// 换条路径不该让断言失守。
	ask := waitPlanApprovalAsk(t, askCh, promptDone, path)
	replyPlanApproval(t, write, ask, path)

	// 批准后 turn 会继续把计划实施完 (写文件、跑命令)。上限从 10 分钟放宽到
	// 20 分钟: 实施阶段的权限请求现在会被自动应答, turn 不应再被悬空超时拖住;
	// 若 20 分钟仍不回来, 值得当真问题查, 而不是像首轮那样轻描淡写。
	select {
	case final := <-promptDone:
		if final["error"] != nil {
			t.Errorf("批准后 session/prompt 返回错误: %v", final["error"])
		} else {
			res, _ := final["result"].(map[string]any)
			mu.Lock()
			n := permAnswered
			mu.Unlock()
			t.Logf("✓ 批准后 session/prompt → %v (实施阶段权限自动应答 %d 次)", res["stopReason"], n)
		}
	case <-time.After(20 * time.Minute):
		// turn 没回来**不影响**本用例要证的东西 —— 要证的是 zcode 收到了
		// "approve"。所以只记一条, 让线材断言照跑, 别在这里就把结果吞掉。
		t.Logf("! 批准后 20 分钟 turn 仍未结束 (权限已自动应答的情况下这不正常), " +
			"但线材断言照跑 —— 本用例要证的是 zcode 收到的答案")
	}

	stop()
	assertRealPlanApprovalReply(t, wirePath)
}

// planApprovalAskKind 分类一条问答请求: "plan" (本用例要等的 plan 审批)、
// "oldbug" (fallback 路径看到了 label 当 optionId —— 旧实现特征)、"other"
// (实施阶段的工具授权或其它问答, 读帧 goroutine 自动应答)。
//
// 不带 *testing.T: 读帧 goroutine 里不能 Fatalf (Goexit 只杀当前 goroutine,
// 整个测试会挂到超时)。oldbug 的失败断言由 waitPlanApprovalAsk 在测试
// goroutine 里做。
func planApprovalAskKind(method string, params map[string]any) string {
	if params == nil {
		return "other"
	}
	if method == "elicitation/create" {
		schema, _ := params["requestedSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		prop, ok := props[planApprovalQuestionText].(map[string]any)
		if !ok {
			return "other"
		}
		enum, _ := prop["enum"].([]any)
		if len(enum) == 1 && enum[0] == planApprovalApproveValue {
			return "plan"
		}
		return "other"
	}
	// 降级路径: 选项里应有 value "approve"。**若看到的是 label "Approve" 则是旧实现**
	// —— 报 oldbug 让测试 goroutine 当场判失败, 别让它落进「模型没调 ExitPlanMode」
	// 那个误导性的超时。
	options, _ := params["options"].([]any)
	for _, raw := range options {
		o, _ := raw.(map[string]any)
		switch o["optionId"] {
		case planApprovalApproveValue:
			return "plan"
		case planApprovalApproveLabel:
			return "oldbug"
		}
	}
	return "other"
}

// waitPlanApprovalAsk 等到 plan 审批请求, 并断言桥走的确实是本用例期望的那条路。
func waitPlanApprovalAsk(t *testing.T, askCh, promptDone chan map[string]any, path string) map[string]any {
	t.Helper()
	deadline := time.After(8 * time.Minute)
	for {
		select {
		case final := <-promptDone:
			// turn 先结束了。绝大多数情况是模型请求失败 (ZCODE_ACP_MODEL 指向的
			// 模型不在目录里是最常见的一个) —— 直接把真实原因报出来, 别伪装成
			// 「模型没调 ExitPlanMode」。
			if final["error"] != nil {
				t.Fatalf("turn 在发起 plan 审批之前就结束了: %v\n"+
					"(若报的是模型请求失败, 先看 serve 日志里 ZCODE_ACP_MODEL 设定失败那行 —— "+
					"模型目录变了, 用当前可用的模型重跑)", final["error"])
			}
			t.Fatalf("turn 结束但没有发起 plan 审批请求: %v", final["result"])
		case ask := <-askCh:
			if ask["kind"] == "oldbug" {
				t.Fatalf("桥把 label %q 当成了 optionId —— 正是 T03 修的 value 映射 bug。"+
					"zcode 拿到的答案会是 %q 而不是 %q, 与常量比对失败 → 判为未批准",
					planApprovalApproveLabel, planApprovalApproveLabel, planApprovalApproveValue)
			}
			method, _ := ask["method"].(string)
			wantMethod := "session/request_permission"
			if path == "elicitation" {
				wantMethod = "elicitation/create"
			}
			if method != wantMethod {
				t.Fatalf("客户端声明=%s 时桥应走 %s, 实际发了 %s (能力协商没生效?)",
					path, wantMethod, method)
			}
			t.Logf("✓ 收到 plan 审批请求 (%s)", method)
			return ask
		case <-deadline:
			t.Fatalf("8 分钟内后端没有发起 plan 审批请求 —— 模型没调 ExitPlanMode。" +
				"这条路径本轮**未验证**, 不要当成已通过 (换 ZCODE_ACP_SMOKE_PLAN_PROMPT 再试)")
		}
	}
}

// replyPlanApproval 按路径回「批准」。
func replyPlanApproval(t *testing.T, write func(map[string]any), ask map[string]any, path string) {
	t.Helper()
	id := fmt.Sprint(ask["id"])
	method, _ := ask["method"].(string)
	if method == "elicitation/create" {
		write(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"action":  "accept",
				"content": map[string]any{planApprovalQuestionText: planApprovalApproveValue},
			},
		})
	} else {
		write(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{"outcome": map[string]any{
				"outcome": "selected", "optionId": planApprovalApproveValue,
			}},
		})
	}
	t.Logf("✓ 客户端批准 (path=%s, 回 %q)", path, planApprovalApproveValue)
}

// assertRealPlanApprovalReply 从线材日志里取出桥对 interaction/requestUserInput
// 的应答, 断言 zcode 收到的答案是 "approve"。
//
// 这是整条链路唯一真正要证的东西: 桥与客户端之间怎么传都对, 只要**最终交给
// zcode 的那个字符串**是 "approve", 它的逐字比对才会通过。收到 "Approve" 的话
// zcode 判为未批准, 并把 "Approve" 当用户反馈喂回模型。
func assertRealPlanApprovalReply(t *testing.T, wirePath string) {
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
		case rec.Dir == "zcode→bridge" && rec.Kind == "interaction/requestUserInput":
			reqID = fmt.Sprint(rec.Msg["ID"])
		case rec.Dir == "bridge→zcode" && rec.Kind == "reply" && reqID != "" &&
			fmt.Sprint(rec.Msg["id"]) == reqID:
			reply, _ = rec.Msg["result"].(map[string]any)
		}
	}
	if reply == nil {
		t.Fatalf("线材日志里没有对 interaction/requestUserInput (%s) 的应答", reqID)
	}
	bts, _ := json.Marshal(reply)
	t.Logf("zcode 实际收到的 requestUserInput 应答: %s", bts)

	if action, _ := reply["action"].(string); action != "accept" {
		t.Fatalf("批准后应回 accept, got %q: %s", action, bts)
	}
	content, _ := reply["content"].(map[string]any)
	answers, _ := content["answers"].(map[string]any)
	got := answers[planApprovalQuestionText]
	if got != planApprovalApproveValue {
		t.Errorf("zcode 收到的答案是 %q, want %q —— 收到 label(%q) 会被逐字比对判为未批准, "+
			"并把该字面量当用户反馈喂回模型", got, planApprovalApproveValue, "Approve")
	}
}
