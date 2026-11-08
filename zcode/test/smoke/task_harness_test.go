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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 这一层把「起桥 → 建会话 → 发 prompt → 自动应答权限 → 收 end_turn」抽成一个
// 装置, 让用例只需要描述两件事: **在哪个目录干活**、**干什么活**。
//
// 为什么要抽: 这段 JSON-RPC 样板以前只存在于 TestSmokeRealBuildTask 里, 想换
// 个目录跑自己的任务就得整段复制 —— 而两百行样板里但凡有一处不同 (比如权限
// 悬着不应答), 现象就是「模型卡住了」, 看不出是装置问题还是桥问题。现在
// TestSmokeRealBuildTask 与 TestSmokeRealTask 共用同一条路径。
//
// 装置刻意不替调用方决定「任务算成功」: 它只负责忠实跑完一个 turn 并把观察到
// 的东西带回来 (ToolCalls / ToolOutput / FinalText), 成败由用例的断言说了算。

const (
	// 注意 promptReqID 在 plan_approval_test.go 里已定义, 同包共用同一个值 ——
	// 别在这里重复声明。
	// defaultMode: build 模式才会真的请求工具授权, 也就是真的动手改文件。
	defaultMode = "build"
	// defaultTimeout 是等一个 turn 收尾的上限。真机冷启动 20s 量级, 多步任务
	// 又是分钟级, 给足余量 —— 超时才能明确区分「模型慢」和「装置没应答」。
	defaultTimeout = 20 * time.Minute
	// taskTimeoutKey 让你不改代码就能放宽/收紧等待上限 (单位: 分钟)。
	taskTimeoutKey = "ZCODE_SMOKE_TASK_TIMEOUT_MIN"
)

// TaskOptions 是一次真机任务的输入。真正需要外部给的就是 Workspace 与 Prompt
// 两项, 其余都有日常默认值。
type TaskOptions struct {
	// Workspace 是任务的工作目录 —— 直接作为 session/new 的 cwd, 模型看到的、
	// 改动的都是它。要求是已存在的绝对路径: 宁可明确报错, 也不悄悄拿临时目录
	// 顶替 (那样跑出来的结果没有意义, 还以为任务生效了)。
	Workspace string
	// Prompt 是交给模型的任务描述。
	Prompt string
	// Mode 是会话模式, 空则用 defaultMode (build)。
	Mode string
	// Timeout 是等 turn 收尾的上限, <=0 则用 defaultTimeout
	// (可用 ZCODE_SMOKE_TASK_TIMEOUT_MIN 覆盖, 单位分钟)。
	Timeout time.Duration
}

// TaskResult 是把一个 turn 跑完之后观察到的东西。字段都是「看到了什么」,
// 不是「成功与否」 —— 后者交给用例断言。
type TaskResult struct {
	SessionID string
	// StopReason 是 session/prompt 返回的 stopReason: end_turn 表示正常收尾。
	StopReason string
	// PromptError 非 nil 时 turn 以错误返回 (错误可见是 D4 的预期行为,
	// 装置不替调用方把它升级成 Fatal)。
	PromptError any
	// PermAnswered 是应答过的工具授权请求次数。0 意味着 zcode 把工具判成了
	// 只读/低风险 —— 值得留意, 但不算失败。
	PermAnswered int
	// ToolCalls 是模型发起的工具调用名, 按发生顺序。
	ToolCalls []string
	// ToolOutput 是拼接起来的工具输出, 可能很长。
	ToolOutput string
	// FinalText 是模型最后说的正文 (agent_message_chunk 拼接)。
	FinalText string
	// WireLog 是这一轮 ACP 线路日志的路径, 事后排查用。
	WireLog string
	// AskAnswers 是这一轮我们回给桥的 elicitation 答案 (扁平的 property 表,
	// 单选 key→value / 多选 key[value]→bool)。本轮没走 elicitation 时为 nil。
	//
	// 之所以要带出来: 「turn 正常 end_turn」并不代表「人的作答到了 zcode」——
	// 桥把整组问题 cancel 掉、模型自己改问一遍照样能 end_turn。没有它, 这个用例
	// 对 AskUserQuestion 链路等于零断言。
	AskAnswers map[string]any
	// AskActions 记录本轮每次 elicitation/create 的 action, 顺序即发生顺序。
	AskActions []string
}

// observations 收集读帧 goroutine 沿途看到的东西。它是并发写的, 所以所有字段
// 只在持锁时访问; 主协程等读帧收尾后才把结果搬进 TaskResult —— 不这么做的话
// 主协程读的同时 goroutine 还在写, -race 会抓到。
type observations struct {
	mu           sync.Mutex
	permAnswered int
	toolCalls    []string
	toolOutput   strings.Builder
	finalText    strings.Builder
	// askAnswers 是最近一次 accept 的 elicitation 答案 (扁平 property 表)。
	askAnswers map[string]any
	// askActions 按顺序记录每次 elicitation/create 的 action。
	askActions []string
}

// noteElicitation 记录一次 elicitation/create 应答。
func (o *observations) noteElicitation(action string, answers map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.askActions = append(o.askActions, action)
	if action == "accept" && answers != nil {
		o.askAnswers = answers
	}
}

// elicitationAnswers 取出记录下来的作答与 action 序列。
func (o *observations) elicitationAnswers() (map[string]any, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.askAnswers, append([]string{}, o.askActions...)
}

// notePermission 记录一次工具授权应答。
func (o *observations) notePermission() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.permAnswered++
}

// noteUpdate 从 session/update 里捡出事后有用的三样: 工具名、工具输出、正文。
// 放在这里而不是用例里, 是为了两个用例看到的是同一份事实。
func (o *observations) noteUpdate(update map[string]any) {
	if update == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch update["sessionUpdate"] {
	case "tool_call":
		name, _ := update["title"].(string)
		if name == "" {
			name, _ = update["toolCallId"].(string)
		}
		o.toolCalls = append(o.toolCalls, name)
	case "tool_call_update":
		if s, _ := update["rawOutput"].(string); s != "" {
			o.toolOutput.WriteString(s)
			o.toolOutput.WriteString("\n")
		}
	case "agent_message_chunk":
		if c, ok := update["content"].(map[string]any); ok {
			o.finalText.WriteString(fmt.Sprint(c["text"]))
		}
	}
}

// snapshot 把并发累积的观察结果搬成返回值。
func (o *observations) snapshot() (perm int, calls []string, out, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.permAnswered, append([]string{}, o.toolCalls...), o.toolOutput.String(), o.finalText.String()
}

// runTask 在真实 ZCode 上跑完整一个 turn。
func runTask(t *testing.T, opt TaskOptions) TaskResult {
	t.Helper()
	if opt.Workspace == "" {
		t.Fatal("runTask: Workspace 为空 (没有指定要干活的项目目录)")
	}
	if opt.Prompt == "" {
		t.Fatal("runTask: Prompt 为空 (没有指定要执行的任务)")
	}
	if opt.Mode == "" {
		opt.Mode = defaultMode
	}
	if opt.Timeout <= 0 {
		opt.Timeout = taskTimeout()
	}

	var res TaskResult
	res.WireLog = filepath.Join(t.TempDir(), "wire.jsonl")

	bridge := bridgeBinary(t)
	cmd := exec.Command(bridge, "serve", "-v")
	cmd.Env = append(os.Environ(), "ZCODE_ACP_WIRE_LOG="+res.WireLog)
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
	go func() { _, _ = io.Copy(os.Stderr, stderr) }() // 诊断直接透传

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)

	var (
		obs        observations
		mu         sync.Mutex
		pending    = map[string]chan map[string]any{}
		promptDone = make(chan map[string]any, 1)
		readDone   = make(chan struct{})
	)

	write := func(frame map[string]any) {
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
		case <-time.After(opt.Timeout):
			t.Fatalf("等待 %s 超过 %v", what, opt.Timeout)
			return nil
		}
	}

	go func() {
		defer close(readDone)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			switch m["method"] {
			case "session/request_permission":
				// 每个工具授权/提问请求都要给个回执: 默认自动选第一个 allow 选项,
				// 避免装置悬着导致模型"卡死" (现象像模型不动, 实际是客户端没应答)。
				// 设 ZCODE_SMOKE_INTERACTIVE=1 时改成真的把问题打到 stderr、从
				// stdin 读人的选择 —— 用来手动验证 AskUserQuestion 这类要人答的流程。
				params, _ := m["params"].(map[string]any)
				toolCall, _ := params["toolCall"].(map[string]any)
				obs.notePermission()
				t.Logf("  ⇐ 权限请求 #%d: title=%v", obs.permAnswered, toolCall["title"])
				outcome := permissionOutcome(t, params)
				write(map[string]any{
					"jsonrpc": "2.0", "id": fmt.Sprint(m["id"]),
					"result": map[string]any{"outcome": outcome},
				})
				continue
			case "elicitation/create":
				// 客户端声明了 elicitation.form 时桥走这条路 (而不是
				// session/request_permission)。少了这一段, 多选题会被桥整组
				// cancel, 模型被迫改问单选 —— 终端里就永远见不到多选。
				params, _ := m["params"].(map[string]any)
				raw, _ := json.Marshal(params["requestedSchema"])
				qs, perr := parseElicitationSchema(raw)
				obs.notePermission()
				var action string
				var content map[string]any
				switch {
				case perr != nil:
					t.Logf("  ⇐ elicitation 表解析失败 (%v), 回 decline", perr)
					action = "decline"
				case smokeFlag("ZCODE_SMOKE_INTERACTIVE"):
					t.Logf("  ⇐ elicitation 提问 #%d (%d 题), 到终端等你的选择",
						obs.permAnswered, len(qs))
					action, content = interactiveElicitationOutcome(t, params)
				default:
					action, content = autoElicitationOutcome(qs)
					t.Logf("  ⇐ elicitation 提问 #%d (自动应答): %d 题, answers=%s",
						obs.permAnswered, len(qs), formatAnswers(content))
				}
				obs.noteElicitation(action, content)
				write(map[string]any{
					"jsonrpc": "2.0", "id": fmt.Sprint(m["id"]),
					"result": map[string]any{"action": action, "content": content},
				})
				continue
			case "session/update":
				params, _ := m["params"].(map[string]any)
				update, _ := params["update"].(map[string]any)
				obs.noteUpdate(update)
				continue
			case nil:
				// 应答, 落到下面
			default:
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

	// 交互模式声明 elicitation.form → 桥走 elicitation/create, **多选题也支持**;
	// 不声明则走降级路径 session/request_permission, 桥表达不了 multiSelect 会
	// 整组 cancel (模型随后把多选题改问成单选)。见 wantElicitationForm。
	initParams := map[string]any{"protocolVersion": 1}
	if caps := elicitationClientCapabilities(); caps != nil {
		initParams["clientCapabilities"] = caps
		t.Logf("✓ 声明 elicitation.form 能力 → 多选题可答 (交互模式)")
	}
	request("1", "initialize", initParams)
	if m := await("1", "initialize"); m["error"] != nil {
		t.Fatalf("initialize 失败: %v", m["error"])
	}
	newParams := map[string]any{"cwd": opt.Workspace, "mode": opt.Mode}
	if initParams["clientCapabilities"] != nil {
		newParams["clientCapabilities"] = initParams["clientCapabilities"]
	}
	request("2", "session/new", newParams)
	m := await("2", "session/new")
	if m["error"] != nil {
		t.Fatalf("session/new 失败 (doctor 可诊断): %v", m["error"])
	}
	res.SessionID, _ = m["result"].(map[string]any)["sessionId"].(string)
	t.Logf("✓ session/new (mode=%s, workspace=%s, bridge=%s) → %s",
		opt.Mode, opt.Workspace, bridge, res.SessionID)

	request(promptReqID, "session/prompt", map[string]any{
		"sessionId": res.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": opt.Prompt}},
	})
	final := <-promptDone

	// turn 收尾了: 停掉后端, 等读帧 goroutine 退出再搬观察结果。
	stop()
	<-readDone
	res.PermAnswered, res.ToolCalls, res.ToolOutput, res.FinalText = obs.snapshot()
	res.AskAnswers, res.AskActions = obs.elicitationAnswers()

	if final["error"] != nil {
		res.PromptError = final["error"]
		t.Logf("⚠ session/prompt 返回错误 (错误可见是 D4 预期): %v", final["error"])
		return res
	}
	res.StopReason = fmt.Sprint(final["result"].(map[string]any)["stopReason"])
	t.Logf("✓ session/prompt → %s (工具调用 %d 次, 授权应答 %d 次)",
		res.StopReason, len(res.ToolCalls), res.PermAnswered)
	return res
}

// bridgeBinary 决定用哪个桥: env 指定的就用它 (验的才是「日常入口」), 否则
// 从当前源码现编一个。
func bridgeBinary(t *testing.T) string {
	t.Helper()
	if b := bridgeBinOverride(); b != "" {
		return b
	}
	b := filepath.Join(t.TempDir(), "zcode-acp-bridge")
	out, err := exec.Command("go", "build", "-o", b,
		"github.com/tizerluo/zcode-open-bridge-go/cmd/zcode-acp-bridge").CombinedOutput()
	if err != nil {
		t.Fatalf("构建桥失败: %v\n%s", err, out)
	}
	return b
}

// taskTimeout 读 ZCODE_SMOKE_TASK_TIMEOUT_MIN (分钟), 没配就用默认值。
//
// 配了但不是正整数时会告警: 静默退回默认值的代价是「我明明放宽了超时, 结果还是
// 超时」, 而那更可能让人去查别的地方。
func taskTimeout() time.Duration {
	n, ok := taskTimeoutMinutes(int(defaultTimeout / time.Minute))
	if !ok {
		fmt.Fprintf(os.Stderr, "⚠ %s=%q 不是正整数, 退回默认 %v\n",
			taskTimeoutKey, smokeCfg[taskTimeoutKey], defaultTimeout)
	}
	return time.Duration(n) * time.Minute
}

// permissionOutcome 决定对一条权限/提问请求怎么回。默认自动选第一个 allow 选项
// (pickAllowOnce), 这样 turn 不会因为没人应答而卡死; 设 ZCODE_SMOKE_INTERACTIVE=1
// 时改成真的把问题+选项打到 stderr、从 stdin 读人的选择 —— 用来手动验证
// AskUserQuestion 这类「要人答」的流程。
//
// 返回的是 result.outcome 那一层 ({"outcome":"selected","optionId":...} 或
// {"outcome":"cancelled"}), 调用方再包进 {"result":{"outcome":...}}。
func permissionOutcome(t *testing.T, params map[string]any) map[string]any {
	if smokeFlag("ZCODE_SMOKE_INTERACTIVE") {
		return interactiveOutcome(t, params)
	}
	if id := pickAllowOnce(params); id != "" {
		return map[string]any{"outcome": "selected", "optionId": id}
	}
	// 桥一个可用选项都没给: 回「作废」让它走放弃路径, 别悬着。
	return map[string]any{"outcome": "cancelled"}
}

// interactiveOutcome 在终端把问题+选项展示出来, 从**控制终端**读人的选择。
// 输入规则: 回车(空行)=作废; 数字=选项编号(1 起); 其它按原样当 optionId 匹配。
// 匹配不上任何选项也作废。
//
// 两个坑, 都已在真机上撞过:
//  1. stdin 不是终端。go test 会把测试进程的 stdin 接到 /dev/null (实测
//     os.Stdin.Stat() 出来是字符设备 Dcrw-rw-rw-, 读一行立刻 EOF), 所以哪怕人
//     坐在终端前, 读 os.Stdin 也一定失败 —— 现象是「(读 stdin 失败, 作废)」。
//     这里直接开 /dev/tty 绕过它。
//  2. 选项字段不是 label/value。桥投放的是 {optionId, name, kind}
//     (bridge/permission.go 与 bridge/ask.go), 而问题原文在
//     toolCall.rawInput.questions[].question —— 按 label/value 取会整排显示成空。
func interactiveOutcome(t *testing.T, params map[string]any) map[string]any {
	cancelled := map[string]any{"outcome": "cancelled"}
	tty := openTTY()
	out := io.Writer(os.Stderr)
	if tty != nil {
		// 提示词打到人真正看着的那个终端: stderr 被 tee/重定向时也能看见。
		// **不要**在这里 Close: 会话是进程级常驻的, 关掉句柄会让带缓冲的 reader
		// 悬空, 后面的提问立刻读不到东西 (见 ttySession 的说明)。
		out = tty
	}
	options := permissionOptionList(params)

	renderPermissionPrompt(out, params)
	fmt.Fprint(out, "  请输入选项编号或 optionId (直接回车=作废): ")

	line, ok := readLineTTY(tty)
	if !ok {
		fmt.Fprintln(out, "  (拿不到终端输入, 作废) —— 提示: go test 会把 stdin 接到 /dev/null, "+
			"本装置改开 /dev/tty; 若仍失败说明进程没有控制终端 (后台/CI 里跑), 交互模式不可用")
		return cancelled
	}
	if line == "" {
		fmt.Fprintln(out, "  → 作废")
		return cancelled
	}
	if n, e := strconv.Atoi(line); e == nil && n >= 1 && n <= len(options) {
		if id := plainStr(options[n-1]["optionId"]); id != "" {
			fmt.Fprintf(out, "  → 选择: %s\n", printableOr(id, "(内含不可打印字符)"))
			return map[string]any{"outcome": "selected", "optionId": id}
		}
	}
	for _, o := range options {
		if id := plainStr(o["optionId"]); id != "" && id == line {
			fmt.Fprintf(out, "  → 选择: %s\n", id)
			return map[string]any{"outcome": "selected", "optionId": id}
		}
	}
	fmt.Fprintln(out, "  (无匹配选项, 作废)")
	return cancelled
}

// renderPermissionPrompt 把一条权限/提问请求画成人类可读的样子。
// 抽出来是为了能被直接单测 —— 「选项整排显示成空的 [n]」就是这里出的错。
func renderPermissionPrompt(out io.Writer, params map[string]any) {
	toolCall, _ := params["toolCall"].(map[string]any)
	options := permissionOptionList(params)
	meta := optionMetaByID(params)

	fmt.Fprintf(out, "\n🔶 人工应答 (session/request_permission)\n")
	fmt.Fprintf(out, "  标题: %v\n", firstNonEmpty(
		fmt.Sprint(toolCall["title"]), fmt.Sprint(toolCall["name"]), "(无标题)"))
	if q := askQuestionText(toolCall); q != "" && q != "<nil>" {
		fmt.Fprintf(out, "  问题: %s\n", q)
	}
	if len(options) == 0 {
		fmt.Fprintf(out, "  (桥没有投放任何选项)\n")
		return
	}
	for i, o := range options {
		idPart := ""
		if id := plainStr(o["optionId"]); printableOptionID(id) {
			idPart = "  id=" + id
		}
		fmt.Fprintf(out, "  [%d] %s%s\n", i+1, optionLine(o, meta), idPart)
	}
}

// permissionOptionList 取权限请求里可选项的对象列表 (丢掉非对象项)。
func permissionOptionList(params map[string]any) []map[string]any {
	var out []map[string]any
	for _, raw := range sliceOfAny(params["options"]) {
		if o, ok := raw.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// optionLine 拼一条选项的展示文案: ACP 的 name 打底, 能找到更原始的信息就补上
// —— 问答走 zcode 提问里的 label/description, 工具授权走 _meta.zcode 里的
// description。末尾附 ACP kind (允许一次 / 始终允许 / 拒绝 …)。
func optionLine(o map[string]any, metaByValue map[string]map[string]any) string {
	id := plainStr(o["optionId"])
	name := plainStr(o["name"])
	kind := plainStr(o["kind"])
	desc := plainStr(metaByValue[id]["description"])
	detail := ""
	switch {
	case desc != "" && desc != name:
		detail = desc
	case name == "" && desc == "":
		detail = "(无名称)"
	}
	line := name
	if line == "" {
		line = desc
	}
	if detail != "" {
		if line != "" {
			line += "  — " + detail
		} else {
			line = detail
		}
	}
	if kind != "" {
		line += fmt.Sprintf("  [%s]", kind)
	}
	return line
}

// 注: 问题原文用 ask_test.go 里已存在的 askQuestionText(toolCall) —— 降级路径把
// 整组问题塞在 toolCall.rawInput.questions 里 (一次只问一题), 普通工具授权没有
// 这一层, 取到空串就跳过「问题:」这一行。

// optionMetaByID 把「原始选项信息」按 value/index 归好索引, 供 optionLine 查。
//
// 两条来源分别对应两种请求: 问答走 rawInput.questions[].options (key=value,
// 恰好就是 ACP 的 optionId —— bridge/ask.go 的 askOptions 用 value 当 optionId);
// 工具授权走 _meta.zcode.options (key=optionId)。取不到就返回空表, 不影响展示。
func optionMetaByID(params map[string]any) map[string]map[string]any {
	toolCall, _ := params["toolCall"].(map[string]any)
	meta := map[string]map[string]any{}
	collect := func(raw any) {
		for _, item := range sliceOfAny(raw) {
			o, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if key := plainStr(o["value"]); key != "" {
				meta[key] = o
			}
		}
	}
	if rawInput, ok := toolCall["rawInput"].(map[string]any); ok {
		for _, item := range sliceOfAny(rawInput["questions"]) {
			if q, ok := item.(map[string]any); ok {
				collect(q["options"])
			}
		}
	}
	if top, ok := mapIndex(params, "_meta"); ok {
		if zc, ok := mapIndex(top, "zcode"); ok {
			if opts, ok := zc["options"].(map[string]any); ok {
				for k, raw := range opts {
					if o, ok := raw.(map[string]any); ok {
						meta[k] = o
					}
				}
			}
		}
	}
	return meta
}
func params2Top(toolCall map[string]any) map[string]any { return nil }

// mapIndex 取 m 里 key 对应的对象; 不存在或非对象返回 nil,false。
func mapIndex(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key].(map[string]any)
	return v, ok
}

// sliceOfAny 把未知类型的值当数组用: 是数组就返回, 否则空。
func sliceOfAny(raw any) []any {
	s, _ := raw.([]any)
	return s
}

// plainStr 取字符串值; 非字符串返回空串 (避免把 <nil> 打进终端)。
func plainStr(v any) string {
	s, _ := v.(string)
	return s
}

// printableOptionID 判断 optionId 能不能安全地打给人看并让他照着输入。
// 桥的问答取消项 optionId 里含 NUL (bridge/ask.go 的 askCancelOptionID),
// 打到终端是乱码, 也不能指望人敲得出来 —— 这种只给「取消」的展示名, 不给 id。
func printableOptionID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// printableOr 决定怎么打印 optionId: 不可打印时给个说明, 而不是把控制字符吐出来。
func printableOr(s, fallback string) string {
	if printableOptionID(s) {
		return s
	}
	return fallback
}

// ttySession 是进程级的控制终端会话: 句柄与 bufio.Reader **必须同生共死**。
//
// 踩过的坑: 早先 openTTY 每次提问都新开一个 /dev/tty, 而带缓冲的 reader 存在
// 进程级变量里。第一次提问结束就 Close 了句柄, 于是 1) 第二次提问(模型追问)
// 拿到的是已关闭 fd, 读不到任何东西, 现象是「第一张表答得好好的, 第二张直接
// 作废」; 2) 一次粘多行时, 每个 Reader 各缓冲一部分, 后创建的再也读不到前面
// 缓冲走的内容。所以这里句柄与 reader 一起缓存, 全进程只开一次。
type ttySession struct {
	f   *os.File
	r   *bufio.Reader
	fmu sync.Mutex // 读侧: 防两个提问 goroutine 抢同一个 reader
	wmu sync.Mutex // 写侧: 提示输出同理
}

// Write 让会话直接当 io.Writer 用 (提示打到人真正看着的那个终端)。
func (s *ttySession) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.f.Write(p)
}

func (s *ttySession) readLine() (string, bool) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if line, err := s.r.ReadString('\n'); err == nil || line != "" {
		return strings.TrimSpace(line), true
	}
	return "", false
}

var (
	ttyOnce   sync.Once
	ttyShared *ttySession
	stdinOnce sync.Once
	stdinShrd *bufio.Reader
	stdinRead sync.Mutex
)

// openTTY 返回进程级常驻的控制终端会话; 没有控制终端时返回 nil。
func openTTY() *ttySession {
	ttyOnce.Do(func() {
		f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return // 留在 nil: 后台任务/CI 没有控制终端
		}
		ttyShared = &ttySession{f: f, r: bufio.NewReader(f)}
	})
	return ttyShared
}

// readStdinLine 是没有控制终端时的后备: 读 os.Stdin (直接跑测试二进制的情况)。
func readStdinLine() (string, bool) {
	stdinOnce.Do(func() { stdinShrd = bufio.NewReader(os.Stdin) })
	stdinRead.Lock()
	defer stdinRead.Unlock()
	if line, err := stdinShrd.ReadString('\n'); err == nil || line != "" {
		return strings.TrimSpace(line), true
	}
	return "", false
}

// readLineTTY 从控制终端读一行; 没有控制终端时回退到 os.Stdin。
// 返回 (去空白的行, 是否成功读到一行)。
func readLineTTY(tty *ttySession) (string, bool) {
	if tty != nil {
		return tty.readLine()
	}
	return readStdinLine()
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
