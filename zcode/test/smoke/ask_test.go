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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSmokeRealAsk 在真实 ZCode 上验证 AskUserQuestion 这条「问用户问题」的能力
// (T03/Phase 4 的核心交互之一)。
//
// 契约层 (test/e2e/ask_test.go) 用 fake zcode 把桥的转换契约钉死了 (两条路径、
// label≠value、多选、取消都覆盖)。这里要证的是另一半事实: **真实后端**把模型
// 的提问转成 ACP 的 AskUserQuestion 时, 桥确实能
//  1. 把 zcode 的 interaction/requestUserInput 变成客户端能看见的问答请求
//     (降级路径下是 session/request_permission, toolCall.title == "AskUserQuestion");
//  2. 把客户端的回答原样回灌给 zcode (线材日志里是 action:accept + content.answers),
//     而不是 cancel 或卡死等待。
//
// 前置: 与本包其它真机用例一致, 需要 ZCODE_BIN 与 provider 配置 (写在 env 里,
// TestMain 注入)。先用脚本拿到 provider/模型:
//
//	python3 scripts/make-smoke-provider-config.py    # 打印 providerId 与模型
//
// 然后 (env 已含 ZCODE_BIN / 两个 provider config / ZCODE_ACP_MODEL):
//
//	go test -count=1 -tags=real ./test/smoke -run TestSmokeRealAsk -v -timeout 12m
//
// **-count=1 不能省**: 换环境变量重跑时 go test 有时会命中缓存直接回上次结果。
//
// 可选环境变量:
//   - ZCODE_SMOKE_ASK_PROMPT:     覆盖默认提示词 (默认会逼模型先提一个带选项的问题)。
//   - ZCODE_SMOKE_ASK_WORKSPACE:  指定工作区 (默认临时目录; 本用例不落文件)。
//
// 注意: 本用例不声明 elicitation.form 能力, 所以走**降级路径**
// (session/request_permission 形式)。elicitation 路径由 e2e 用 fake zcode 覆盖。
//
// 应答方式默认自动选第一个选项; 设 ZCODE_SMOKE_INTERACTIVE=1 (Makefile 的
// ask-interactive target) 则把问题+选项打到终端、从 /dev/tty 读人的选择 ——
// 手动验证「真的在问人」时用它。
func TestSmokeRealAsk(t *testing.T) {
	bin := os.Getenv("ZCODE_BIN")
	if bin == "" {
		t.Skip("ZCODE_BIN 未设置 (真机 AskUserQuestion 验证需要)")
	}
	workspace := strings.TrimSpace(askWorkspace())
	if workspace == "" {
		workspace = t.TempDir()
	}
	prompt := strings.TrimSpace(askPrompt())
	if prompt == "" {
		// 逼模型真的调 AskUserQuestion 工具、且带选项 (否则降级路径会因「无合法
		// 选项」而整体 cancel, 看不到 AskUserQuestion 请求)。
		prompt = "在动手做任何事之前, 请务必先调用 AskUserQuestion 工具向我提一个**带 2 个选项**" +
			"的澄清问题 (例如: 你希望任务的输出风格是 A 还是 B?), 给出两个选项并等我的选择;" +
			"收到我的回答后, 用一句话总结你会按哪个选项继续。必须真的调用 AskUserQuestion " +
			"工具, 不要只用文字提问。"
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

	var (
		mu          sync.Mutex
		pending     = map[string]chan map[string]any{}
		promptDone  = make(chan map[string]any, 1)
		askObserved bool
		askAnswered bool
		askReqID    string // 线材日志里 interaction/requestUserInput 的 ID
		askQuestion string // 模型问的那一题的原文
		askAnswer   string // 我们替用户选的选项 value (降级路径是 optionId)
		// askClientContent 是回给桥的**扁平 property 表** (单选 key→value,
		// 多选 key[value]→bool)。断言拿它跟 zcode 实际收到的 answers 逐条比,
		// 证明「人所选的」真的到了 zcode, 而不只是「有个 accept」。
		askClientContent map[string]any
		// askText 拼接 agent 的发言 (agent_message_chunk)。人回答完, 模型的
		// 复述才是「人机回路真的闭上了」的证据 —— 只验「答案到了 zcode」看不到
		// 模型到底怎么回应。
		askText     strings.Builder
		askToolCall []string
		// askActions 按顺序记录每次 elicitation/create 的 action。模型可能重复
		// 提问 (prompt 只说「问我」而没说「答完做什么」时), 那些轮次也得有人看:
		// 只断言最后一次的话, 前几轮被 cancel 之类的问题就静默漏掉了。
		askActions []string
		// askZcodeReqIDs 按顺序记录每次 elicitation 背后的 zcode requestId
		// (从 _meta.zcode.requestId 取)。zcode 会**重发**没等到应答的提问
		// (新 JSON-RPC id、同一个 requestId), 桥必须只问一次 —— 同一个
		// requestId 出现两次就是重复提问, 而第二份答案还会被 zcode 静默丢弃。
		// 这条断言曾长期缺失: 重复提问时两轮都是 accept, 旧断言全过。
		askZcodeReqIDs []string
		askAnswerErr   error
	)

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
		case <-time.After(10 * time.Minute):
			t.Fatalf("等待 %s 超时", what)
			return nil
		}
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if m["method"] == "session/update" {
				params, _ := m["params"].(map[string]any)
				update, _ := params["update"].(map[string]any)
				mu.Lock()
				switch update["sessionUpdate"] {
				case "agent_message_chunk":
					if c, ok := update["content"].(map[string]any); ok {
						askText.WriteString(fmt.Sprint(c["text"]))
					}
				case "tool_call":
					name, _ := update["title"].(string)
					if name == "" {
						name, _ = update["toolCallId"].(string)
					}
					askToolCall = append(askToolCall, name)
				}
				mu.Unlock()
				continue
			}
			if m["method"] == "elicitation/create" {
				params, _ := m["params"].(map[string]any)
				raw, _ := json.Marshal(params["requestedSchema"])
				qs, perr := parseElicitationSchema(raw)
				mu.Lock()
				askObserved = true
				askReqID = fmt.Sprint(m["id"])
				if rid := elicitationZcodeRequestID(params); rid != "" {
					askZcodeReqIDs = append(askZcodeReqIDs, rid)
				}
				if len(qs) > 0 {
					askQuestion = qs[0].Question
				}
				mu.Unlock()
				var action string
				var content map[string]any
				if perr != nil {
					t.Logf("  ⚠ 解析 requestedSchema 失败 (%v), 回 decline", perr)
					mu.Lock()
					askAnswerErr = perr
					mu.Unlock()
					action = "decline"
				} else if smokeFlag("ZCODE_SMOKE_INTERACTIVE") {
					action, content = interactiveElicitationOutcome(t, params)
				} else {
					action, content = autoElicitationOutcome(qs)
					t.Logf("  ⇐ AskUserQuestion (自动应答): %d 题, answers=%s",
						len(qs), formatAnswers(content))
				}
				mu.Lock()
				askActions = append(askActions, action)
				if action == "accept" {
					askAnswered = true
					// clientAnswers 是**我们回给桥的扁平 property 表**, 拿它去跟
					// 线材日志里 zcode 实际收到的东西逐条比对。
					//
					// 这里曾经写成 content["answers"] —— 扁平 content 里没有这个
					// key, 于是 askAnswer 恒为空, 断言里的值比对整段被跳过, 测试
					// 带着一个从未校验过的多选答案就 PASS 了。
					askClientContent = content
					askAnswer = formatAnswers(content)
				}
				mu.Unlock()
				write(map[string]any{
					"jsonrpc": "2.0", "id": m["id"],
					"result": map[string]any{"action": action, "content": content},
				})
				continue
			}
			if m["method"] == "session/request_permission" {
				params, _ := m["params"].(map[string]any)
				toolCall, _ := params["toolCall"].(map[string]any)
				title, _ := toolCall["title"].(string)
				if title == "AskUserQuestion" {
					// 这就是我们要验的提问。应答方式与 task harness 共用
					// permissionOutcome: 默认自动选第一个选项, 设
					// ZCODE_SMOKE_INTERACTIVE=1 则把问题+选项打到终端、从 /dev/tty
					// 读人的选择 (见 Makefile 的 ask-interactive)。
					outcome := permissionOutcome(t, params)
					chosen, _ := outcome["optionId"].(string)
					mu.Lock()
					askObserved = true
					askReqID = fmt.Sprint(m["id"])
					askQuestion = askQuestionText(toolCall)
					if chosen == "" {
						askAnswerErr = fmt.Errorf("AskUserQuestion 没有可用选项 (outcome=%v): %v", outcome, params)
					} else {
						askAnswer = chosen
						askAnswered = true
					}
					mu.Unlock()
					t.Logf("  ⇐ AskUserQuestion: %q → 选 %q", askQuestion, chosen)
					write(map[string]any{
						"jsonrpc": "2.0", "id": m["id"],
						"result": map[string]any{"outcome": outcome},
					})
					continue
				}
				// 其它工具授权 (模型拿到答案后可能还想改点东西): 允许一次, 别悬。
				optionID := pickAllowOnce(params)
				outcome := map[string]any{"outcome": "selected", "optionId": optionID}
				if optionID == "" {
					outcome = map[string]any{"outcome": "cancelled"}
				}
				write(map[string]any{
					"jsonrpc": "2.0", "id": m["id"],
					"result": map[string]any{"outcome": outcome},
				})
				continue
			}
			if m["method"] != nil {
				continue // 其它通知
			}
			key := fmt.Sprint(m["id"])
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

	// 交互模式声明 elicitation.form → 桥走 elicitation/create (支持多选);
	// 否则不声明, 走降级路径 session/request_permission。
	caps := map[string]any{"protocolVersion": 1}
	if c := elicitationClientCapabilities(); c != nil {
		caps["clientCapabilities"] = c
		t.Logf("✓ 声明 elicitation.form 能力 → 走 elicitation 路径 (支持多选)")
	} else {
		t.Logf("✓ 不声明 elicitation.form → 走降级路径 (session/request_permission, 无多选)")
	}
	request("1", "initialize", caps)
	if m := await("1", "initialize"); m["error"] != nil {
		t.Fatalf("initialize 失败: %v", m["error"])
	}
	newParams := map[string]any{"cwd": workspace, "mode": "build"}
	if caps["clientCapabilities"] != nil {
		newParams["clientCapabilities"] = caps["clientCapabilities"]
	}
	request("2", "session/new", newParams)
	m := await("2", "session/new")
	if m["error"] != nil {
		t.Fatalf("session/new 失败 (doctor 可诊断): %v", m["error"])
	}
	sid, _ := m["result"].(map[string]any)["sessionId"].(string)
	t.Logf("✓ session/new (mode=build, workspace=%s) → %s", workspace, sid)

	go request(promptReqID, "session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	})

	select {
	case final := <-promptDone:
		stop()
		if final["error"] != nil {
			t.Logf("⚠ session/prompt 返回错误: %v", final["error"])
		} else {
			stopReason := fmt.Sprint(final["result"].(map[string]any)["stopReason"])
			t.Logf("✓ session/prompt → %s", stopReason)
		}
	case <-time.After(12 * time.Minute):
		stop()
		t.Fatal("12 分钟内 turn 未收尾 (模型没发起 AskUserQuestion, 或桥没把回答回灌?)")
	}
	// 桥进程已被 stop() 杀掉, 它的 stdout 随之关闭 → 读帧 goroutine 收尾。
	// 有界等待: 超时也只提示, 不让这里成为新的挂死点。
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Logf("⚠ 读帧 goroutine 5s 内没退出, 继续断言")
	}

	mu.Lock()
	obs, ans, q, a := askObserved, askAnswered, askQuestion, askAnswer
	clientContent := askClientContent
	actions := append([]string{}, askActions...)
	reqErr := askAnswerErr
	rID := askReqID
	mu.Unlock()

	if !obs {
		t.Fatalf("后端没有发起 AskUserQuestion (模型没用 AskUserQuestion 工具提问?"+
			" 可加大提示词的强制程度, 或用 ZCODE_SMOKE_ASK_PROMPT 覆盖。原 prompt:\n%s", prompt)
	}
	if reqErr != nil {
		t.Fatalf("AskUserQuestion 解析失败: %v", reqErr)
	}
	if !ans {
		t.Fatalf("看到了 AskUserQuestion 但没能回答问题 (选项提取失败?)")
	}
	// 每一轮提问都必须被 accept。模型重复提问时, 那些轮次同样要算数 —— 少 accept
	// 一次就意味着「有一轮人的作答没以 accept 形式到达 zcode」。
	if len(actions) > 0 {
		t.Logf("本轮 elicitation 应答 action 序列: %v", actions)
		bad := 0
		for _, act := range actions {
			if act != "accept" {
				bad++
			}
		}
		if bad > 0 {
			t.Errorf("%d/%d 次 elicitation 应答不是 accept (序列 %v) —— "+
				"这些轮次人的作答没以 accept 形式到达 zcode", bad, len(actions), actions)
		}
	}
	// 同一个 zcode requestId 只能问客户端一次。zcode 会重发没等到应答的提问
	// (新 JSON-RPC id、同一个 requestId), 桥若照单全收, 人就被问两遍, 而且第二份
	// 答案会被 zcode 静默丢弃 —— 人选了等于没选。
	//
	// 这条断言曾长期缺失: 重复提问时两轮都是 accept, 上面的 action 断言照样全过。
	mu.Lock()
	zcodeReqIDs := append([]string{}, askZcodeReqIDs...)
	mu.Unlock()
	if len(zcodeReqIDs) > 0 {
		t.Logf("本轮 elicitation 对应的 zcode requestId 序列: %v", zcodeReqIDs)
		seen := map[string]int{}
		dup := map[string]int{}
		for _, rid := range zcodeReqIDs {
			seen[rid]++
			if seen[rid] == 2 {
				dup[rid] = seen[rid]
			}
		}
		if len(dup) > 0 {
			t.Errorf("zcode requestId %v 各被问了不止一次 (序列 %v) —— 同一道题重复提问, "+
				"第二份作答不会被模型看到 (zcode 在首个应答后注销了全部重发 id)",
				keysOfCounts(dup), zcodeReqIDs)
		}
	}
	t.Logf("✓ 客户端收到提问 %q 并选择了 %q", q, a)

	// 人回答完, 模型**必须**给出后续发言 —— 否则「回答到了 zcode」也只证明
	// 了投递, 证明不了人机回路真的闭上 (比如模型拿到答案却直接收尾不说话)。
	mu.Lock()
	finalText := strings.TrimSpace(askText.String())
	toolCalls := append([]string{}, askToolCall...)
	mu.Unlock()
	if len(toolCalls) > 0 {
		t.Logf("工具调用 (%d): %s", len(toolCalls), strings.Join(toolCalls, " → "))
	}
	if finalText == "" {
		t.Errorf("收到回答后 agent 没有任何发言 —— 人机回路没闭上 "+
			"(action 序列 %v)", actions)
	} else {
		t.Logf("agent 在收到回答后的答复:\n%s", indent(truncate(finalText, 2000)))
	}

	// 线材日志: 桥回给 zcode 的那一包必须 accept 且带 answers (不是 cancel)。
	//
	// rID 是**桥发给客户端**的那次 session/request_permission 的 ACP 请求 id
	// (形如 brq-1), 只用来确认「我们回的那条对得上我们看到的那条」; 线材日志里
	// 的应答 id 是 zcode 侧的请求 id, 另一套 id 空间, 不能拿它去匹配 (那是这个
	// 断言早先一直失败的根因)。
	assertRealAskReply(t, wirePath, rID, a, clientContent)
}

// elicitationZcodeRequestID 从桥发来的 elicitation/create 里取 zcode 侧的
// requestId (桥放在 _meta.zcode.requestId)。这是 zcode 那次交互的注册键 ——
// 重发时 JSON-RPC id 会变, 但它不变, 所以它才是「同一道题」的判据。
func elicitationZcodeRequestID(params map[string]any) string {
	meta, _ := params["_meta"].(map[string]any)
	zmeta, _ := meta["zcode"].(map[string]any)
	rid, _ := zmeta["requestId"].(string)
	return rid
}

// keysOfCounts 把计数表摊平成可读的键列表, 免得错误信息只印一个 map。
func keysOfCounts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k, n := range m {
		out = append(out, fmt.Sprintf("%s×%d", k, n))
	}
	sort.Strings(out)
	return out
}

// askQuestionText 从 toolCall 里取出模型问的问题原文 (降级路径投在 rawInput 里)。
func askQuestionText(toolCall map[string]any) string {
	raw, _ := toolCall["rawInput"].(map[string]any)
	qs, _ := raw["questions"].([]any)
	if len(qs) == 0 {
		return ""
	}
	q, _ := qs[0].(map[string]any)
	return fmt.Sprint(q["question"])
}

// assertRealAskReply 从线材日志里取桥对 interaction/requestUserInput 的应答,
// 断言它是 accept + 非空 answers (证明回答真的回灌给 zcode, 而非 cancel)。
//
// acpReqID 是**客户端看到的**那次 session/request_permission 的 ACP 请求 id。
// 它与线材日志里的 id 不是同一套编号 (后者是 zcode→bridge 请求的 ID, 配对的
// 是 bridge→zcode reply 的 msg.id), 所以只能拿它做校验用的提示, 不能当匹配键。
func assertRealAskReply(t *testing.T, wirePath, acpReqID, wantValue string, clientContent map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(wirePath)
	if err != nil {
		t.Fatalf("读线材日志失败: %v", err)
	}
	var zcodeReqID string
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
		// 先记下 zcode 侧请求的 ID, 再用它配对桥的应答 —— 两步都要在同一次扫描里
		// 按出现顺序做 (见 test/smoke/plan_approval_test.go 的同类断言)。
		switch {
		case rec.Dir == "zcode→bridge" && rec.Kind == "interaction/requestUserInput":
			zcodeReqID = fmt.Sprint(rec.Msg["ID"])
		case rec.Dir == "bridge→zcode" && rec.Kind == "reply" && zcodeReqID != "" &&
			fmt.Sprint(rec.Msg["id"]) == zcodeReqID:
			reply, _ = rec.Msg["result"].(map[string]any)
		}
	}
	if reply == nil {
		t.Fatalf("线材日志里没有对 interaction/requestUserInput (%s) 的应答"+
			" (客户端侧 ACP 请求 id=%s; 桥是否把回答回灌给 zcode?)", zcodeReqID, acpReqID)
	}
	bts, _ := json.Marshal(reply)
	t.Logf("zcode 实际收到的 AskUserQuestion 应答: %s", bts)

	if action, _ := reply["action"].(string); action != "accept" {
		t.Errorf("ask 应答应为 accept, 实际 action=%v (msg: %s)", action, bts)
	}
	content, _ := reply["content"].(map[string]any)
	if content == nil {
		t.Fatalf("ask 应答无 content: %s", bts)
	}
	answers, _ := content["answers"].(map[string]any)
	if len(answers) == 0 {
		t.Errorf("ask 应答的 content.answers 为空, 回答没回灌: %s", bts)
	} else {
		t.Logf("✓ 回答已回灌 (answers=%v)", answers)
	}
	// 降级路径: 我们选的就是一个 optionId (= 选项 value), 直接查它有没有到 zcode。
	if wantValue != "" && len(clientContent) == 0 {
		hit := false
		for _, v := range answers {
			if fmt.Sprint(v) == wantValue {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("answers 里没有我们选的 %q: %s", wantValue, bts)
		}
		return
	}

	// elicitation 路径: 把「我们回的扁平 property 表」逐条翻译成 zcode 该收到的
	// answers 再比对。多选在这里才算真的被验过 —— 只验「有个 accept」是不够的,
	// 勾错了选项一样会 accept。
	if len(clientContent) == 0 {
		t.Errorf("没有记录到回给桥的 answers, 无法核对所选值 (wantValue=%q)", wantValue)
		return
	}
	assertClientAnswersReachedZcode(t, clientContent, answers, bts)
}

// assertClientAnswersReachedZcode 核对「人所选的」与「zcode 实际收到的」一致。
//
// 映射按 internal/bridge/elicitation.go 的约定:
//   - 单选 key → 值必须逐字相同 (zcode 会拿它做字面量比对, label≠value 会出事)
//   - 多选 key[value]=true → zcode 的 answers[key] 必须是含该 value 的数组,
//     且集合完全一致 (漏一个或多一个都是错)
func assertClientAnswersReachedZcode(t *testing.T, client, answers map[string]any, replyJSON []byte) {
	t.Helper()
	// 把多选的 key[value] 收拢回 key。
	single := map[string]any{}
	multi := map[string]map[string]bool{}
	for name, v := range client {
		if i := strings.LastIndex(name, "["); i > 0 && strings.HasSuffix(name, "]") {
			prefix, value := name[:i], name[i+1:len(name)-1]
			if multi[prefix] == nil {
				multi[prefix] = map[string]bool{}
			}
			if b, _ := v.(bool); b {
				multi[prefix][value] = true
			}
			continue
		}
		single[name] = v
	}

	for key, want := range single {
		got, present := answers[key]
		if !present {
			t.Errorf("zcode 的 answers 里缺 %q (我们回的是 %v): %s", key, want, replyJSON)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q: zcode 收到 %v, 我们选的是 %v —— 逐字比对会判为未答复: %s",
				key, got, want, replyJSON)
		}
	}

	for key, picked := range multi {
		got, present := answers[key]
		if !present {
			t.Errorf("zcode 的 answers 里缺多选题 %q: %s", key, replyJSON)
			continue
		}
		arr, ok := got.([]any)
		if !ok {
			// 也接受 []string (JSON 解码路径可能给出它)
			if ss, ok2 := got.([]string); ok2 {
				arr = make([]any, 0, len(ss))
				for _, v := range ss {
					arr = append(arr, v)
				}
			} else {
				t.Errorf("多选题 %q: zcode 收到 %T (%v), 期望数组: %s", key, got, got, replyJSON)
				continue
			}
		}
		gotSet := map[string]bool{}
		for _, v := range arr {
			gotSet[fmt.Sprint(v)] = true
		}
		for v := range picked {
			if !gotSet[v] {
				t.Errorf("多选题 %q: 我们勾了 %q, zcode 收到的 %v 里没有: %s", key, v, arr, replyJSON)
			}
		}
		for v := range gotSet {
			if !picked[v] {
				t.Errorf("多选题 %q: 我们没勾 %q, zcode 却收到了: %s", key, v, replyJSON)
			}
		}
	}
}
