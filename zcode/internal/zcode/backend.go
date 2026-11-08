// Package zcode 封装 zcode app-server 子进程: JSON Lines 双向通信、pending 请求表、
// 事件流路由、server→client 反向调用应答、状态投影。
//
// 对应上游 ZCodeBackend。与上游的关键差异:
//  1. 子进程 stderr 不再丢 DEVNULL, 而保留最后 20 行 —— 0.16.5 的
//     「无法定位 CLI ZCode Built-in Provider Config」正是因此被吞掉的
//     (PORTING_PLAN §2.2 C1 / §5.4)。
//  2. 进程退出时所有 pending 请求立即失败并带上退出码与最后几行 stderr。
package zcode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// currentDir 返回当前工作目录, 失败时返回 "."。
func currentDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return ".", err
	}
	return wd, nil
}

// maxLine 是单行 JSON 的读取上限。zcode 的快照/工具输出会很长, 16 MB 是实测
// 够用的量级 (bufio.Scanner 默认 64 KB 会在大 projection 上直接报错)。
const maxLine = 16 << 20

// maxSessionStates 是状态投影的软上限。ACP baseline 没有 session 关闭钩子可挂清理,
// 超上限 FIFO 淘汰最旧条目, 防长驻进程内存单调增长 (上游 _MAX_SESSION_STATES)。
const maxSessionStates = 64

// RuntimePreferences 是 session/requestRuntimePreferences 的应答体。
// schema 实测自 bundle zod: 桥没有这些客户端能力, 全部关闭。
var RuntimePreferences = map[string]any{
	"nativeSearchEnhancementsEnabled":      false,
	"memoryEnabled":                        false,
	"askUserQuestionAutoResolutionEnabled": false,
}

// RPCError 是 JSON-RPC 错误对象。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return fmt.Sprintf("zcode rpc error %d", e.Code)
	}
	return e.Message
}

// Response 是 zcode 对一次请求的响应 (无 jsonrpc 字段)。
type Response struct {
	ID     string
	Result json.RawMessage
	Error  *RPCError
}

// Err 把响应里的 error 转成 Go error; 无错误返回 nil。
func (r *Response) Err() error {
	if r == nil {
		return errors.New("nil response")
	}
	if r.Error != nil {
		return r.Error
	}
	return nil
}

// ResultMap 把 result 解析成宽类型的 map。解析失败返回空 map。
func (r *Response) ResultMap() map[string]any {
	if r == nil || len(r.Result) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(r.Result, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// Listener 是一个会话的事件/反向调用接收端。
//
// 事件与反向调用分成两个通道: turn 循环用 select 同时消费, 保证
// interaction/requestUserInput 到达时即使事件流正忙也能被及时应答。
type Listener struct {
	SessionID string
	Events    chan Event
	Requests  chan *ServerRequest
	// ContextWindow 是订阅快照里的 projection.contextWindow。
	// 0.16.1 的 turn.completed.usage 只有 token 明细、没有 contextWindow,
	// 用它兜底 usage_update 的 size (上游 bb7ada5)。
	ContextWindow int

	lastSeq int64
}

// NewListener 构造监听器。通道带缓冲, 短 burst 不会阻塞 reader 线程。
func NewListener(sid string) *Listener {
	return &Listener{
		SessionID: sid,
		Events:    make(chan Event, 256),
		Requests:  make(chan *ServerRequest, 32),
	}
}

// Seq 返回已见的最大事件序号。
func (l *Listener) Seq() int64 { return atomic.LoadInt64(&l.lastSeq) }

// observeSeq 记录事件序号 (只增不减)。
func (l *Listener) observeSeq(seq int64) {
	for {
		cur := atomic.LoadInt64(&l.lastSeq)
		if seq <= cur {
			return
		}
		if atomic.CompareAndSwapInt64(&l.lastSeq, cur, seq) {
			return
		}
	}
}

// Options 控制后端启动方式。
type Options struct {
	// Bin 是 zcode 可执行文件路径 (已解析为绝对路径)。
	Bin string
	// Args 是子命令与参数, 默认 ["app-server", "--stdio"]。
	Args []string
	// Env 是完整子进程环境 (由 compat 推导: 含 provider 配置路径与凭证)。
	Env []string
	// Logger 用于诊断输出, 必须写 stderr; 为空则丢弃。
	Logger func(format string, args ...any)
	// TraceNotifications 单独打开「被忽略的未知通知」日志。它**不**由 serve -v
	// 派生: 一次真实 turn 能收到上千条 v4/telemetry/event, 连 -v 下逐条/聚合打印
	// 都会把 stderr 淹掉, 把真正要看的诊断信息冲走。要查这批通知时用
	// ZCODE_ACP_TRACE_NOTIF=1 单独开。
	TraceNotifications bool
	// Wire 是可选的线材日志记录器。
	Wire interface {
		Log(dir, kind string, msg any)
	}
	// ProbeTimeout 是协议探测窗口, 0 表示用默认 45s。
	// 0.16.5 的 app-server 冷启动约 20s (大会话库), 窗口必须覆盖它。
	ProbeTimeout time.Duration
}

func (o Options) args() []string {
	if len(o.Args) > 0 {
		return o.Args
	}
	return []string{"app-server", "--stdio"}
}

// Backend 管理一个 zcode app-server 子进程。
type Backend struct {
	opts   Options
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *stderrRing

	writeMu sync.Mutex // 保护 stdin 写入

	respMu   sync.Mutex
	pending  map[string]chan *Response
	listener map[string]*Listener
	enhance  map[string]chan map[string]any

	stateMu  sync.Mutex
	state    map[string]map[string]any
	stateSeq []string // FIFO 淘汰顺序

	// observed 累计出现过的 server→client 请求方法名, 供 doctor/record 输出
	// (PORTING_PLAN §5.3 要求记录「所有出现过的 server 请求方法名」)。
	observedMu sync.Mutex
	observed   map[string]int

	probeMu sync.Mutex
	mode    ProtocolMode
	// confirmed 表示协议判定是否有实锤。超时兜底判出的 v16 为 false,
	// 允许被 subscribe 失败推翻回退 legacy (上游 zcode review P1-3)。
	confirmed bool
	// ProtocolVersion 是 session/create 返回的 protocol.version (0.16+)。
	ProtocolVersion int

	dead     atomic.Bool
	exitCode atomic.Int32
	exitErr  atomic.Value // error
	done     chan struct{}
	wg       sync.WaitGroup

	// ignoredNotif 统计被静默丢弃的未知通知 (按 method), 只用于 verbose 日志的
	// 聚合输出, 不影响协议行为。
	ignoredNotif sync.Map // method(string) → *atomic.Int64
}

// ignoredNotifCnt 记一次该 method 的忽略, 返回累计次数。
func (b *Backend) ignoredNotifCnt(method string) int64 {
	v, _ := b.ignoredNotif.LoadOrStore(method, new(atomic.Int64))
	return v.(*atomic.Int64).Add(1)
}

// Start 启动子进程与 reader goroutine, 并做一次性协议探测。
func Start(opts Options) (*Backend, error) {
	if opts.Bin == "" {
		return nil, errors.New("zcode 可执行文件路径为空")
	}
	args := opts.args()
	cmd := exec.Command(opts.Bin, args...)
	cmd.Env = opts.Env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("建立 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("建立 stdout 管道失败: %w", err)
	}
	ring := newStderrRing(20)
	cmd.Stderr = ring
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s %v 失败: %w", opts.Bin, args, err)
	}

	b := &Backend{
		opts:     opts,
		cmd:      cmd,
		stdin:    stdin,
		stderr:   ring,
		pending:  map[string]chan *Response{},
		listener: map[string]*Listener{},
		enhance:  map[string]chan map[string]any{},
		state:    map[string]map[string]any{},
		observed: map[string]int{},
		done:     make(chan struct{}),
	}
	b.logf("已启动 zcode app-server (pid=%d)", cmd.Process.Pid)

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.readLoop(stdout)
	}()

	// 协议探测在后台跑: 0.16.5 的 app-server 冷启动约 20s (本机 1.2GB 会话库),
	// 探测帧要等它起来才回。同步等待会把 serve 的 initialize 也拖住 20s+。
	// 未出结果前 Mode() 按当前支持矩阵返回 v16 (§5.1 主目标)。
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.DetectProtocol()
	}()
	return b, nil
}

func (b *Backend) logf(format string, args ...any) {
	if b.opts.Logger != nil {
		b.opts.Logger(format, args...)
	}
}

func (b *Backend) wire(dir, kind string, msg any) {
	if b.opts.Wire != nil {
		b.opts.Wire.Log(dir, kind, msg)
	}
}

// Pid 返回子进程 pid。
func (b *Backend) Pid() int {
	if b.cmd == nil || b.cmd.Process == nil {
		return 0
	}
	return b.cmd.Process.Pid
}

// Dead 报告 reader 是否已退出 (后端不可再用)。
func (b *Backend) Dead() bool { return b.dead.Load() }

// ExitError 返回子进程退出错误 (尚未退出时为 nil)。
func (b *Backend) ExitError() error {
	if v, ok := b.exitErr.Load().(error); ok {
		return v
	}
	return nil
}

// StderrTail 返回最近若干行子进程 stderr (最多 n 行)。
func (b *Backend) StderrTail(n int) []string { return b.stderr.tail(n) }

// ExitDetail 拼出 §5.4 要求的失败面文案: 退出码 + 最后几行 stderr。
func (b *Backend) ExitDetail() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("zcode backend exited (code %d", b.exitCode.Load()))
	lines := b.StderrTail(5)
	if len(lines) > 0 {
		sb.WriteString(", last stderr: ")
		sb.WriteString(strings.Join(lines, " | "))
	}
	sb.WriteString(")")
	return sb.String()
}

// ObservedServerMethods 返回已出现过的 server→client 请求方法名及次数。
func (b *Backend) ObservedServerMethods() map[string]int {
	b.observedMu.Lock()
	defer b.observedMu.Unlock()
	out := make(map[string]int, len(b.observed))
	for k, v := range b.observed {
		out[k] = v
	}
	return out
}

// Mode 返回协议模式。探测未完成时按当前支持矩阵返回 v16 (§5.1: 0.16.5 是主目标,
// 0.15.x 仅尽力兼容), 故调用方可直接使用而不必等待。
func (b *Backend) Mode() ProtocolMode {
	b.probeMu.Lock()
	m := b.mode
	b.probeMu.Unlock()
	if m == "" {
		return ProtocolV16
	}
	return m
}

// ProbeDone 报告协议探测是否已出结果。
func (b *Backend) ProbeDone() bool {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	return b.mode != ""
}

// Confirmed 报告协议判定是否有实锤 (超时兜底判出的 v16 为 false)。
func (b *Backend) Confirmed() bool {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	return b.confirmed
}

// WaitProbe 等待协议探测完成, 超时返回 false。doctor 用它拿实锤结论。
func (b *Backend) WaitProbe(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b.ProbeDone() {
			return true
		}
		if b.Dead() {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return b.ProbeDone()
}

// readLoop 是 reader goroutine: 持续读 stdout, 按消息类型分发。
func (b *Backend) readLoop(r io.Reader) {
	defer func() {
		b.dead.Store(true)
		b.failAllPending(errors.New(b.ExitDetail()))
		close(b.done)
	}()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			// 非 JSON 行 (启动横幅等) 静默跳过, 协议流不能被单行使坏。
			continue
		}
		b.dispatch(msg)
	}
	// 进程退出: 等 Wait 拿到退出码, 让 ExitDetail 带上真实 code。
	waitErr := b.cmd.Wait()
	if waitErr != nil {
		b.exitErr.Store(waitErr)
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			b.exitCode.Store(int32(ee.ExitCode()))
		}
	}
	b.logf("reader 线程退出 (backend 已标记为 dead)")
}

func (b *Backend) dispatch(msg map[string]any) {
	rawID, hasID := msg["id"]
	method, _ := msg["method"].(string)

	switch {
	case hasID && method == "":
		// 响应: 有 id 无 method
		b.deliverResponse(canonicalID(rawID), msg)
	case hasID && method != "":
		// server→client 反向调用
		b.handleServerRequest(canonicalID(rawID), method, msg)
	case method == "session/event":
		b.routeEvent(msg)
	case method == "state.updated":
		b.mergeStatePatch(msg)
	case method == "prompt/enhance/result":
		b.routeEnhanceResult(msg)
	default:
		// 其他 notification (v4/telemetry/event、computer-use/operation-event、
		// process/mcpTelemetry、startup/storageState …): 静默丢弃。
		// 它们高频且桥不消费, 入队只会无界积压 (§2.2 C9)。
		b.verboseNotification(method)
	}
}

// verboseNotification 记录「忽略了一条未知通知」, 但只在
// Options.TraceNotifications 打开时输出, 且按 method 聚合计数 ——
//
//	一次真实 turn 就能收到上千条 v4/telemetry/event。逐条打印会把 stderr 淹掉,
//	真正要看的诊断信息 (ask/permission/turn.failed) 反而被冲走。所以: 首次出现
//	打一条, 之后每 suppressedEvery 条再打一条累计数。
func (b *Backend) verboseNotification(method string) {
	if !b.opts.TraceNotifications || b.opts.Logger == nil {
		return
	}
	const suppressedEvery = 100
	n := b.ignoredNotifCnt(method)
	if n == 1 {
		b.opts.Logger("  [notification] 忽略 %s (后续同类不再逐条打印)", method)
		return
	}
	if n%suppressedEvery == 0 {
		b.opts.Logger("  [notification] 忽略 %s ×%d", method, n)
	}
}

func canonicalID(raw any) string {
	switch t := raw.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconvInt(t)
	default:
		b, err := json.Marshal(raw)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func strconvInt(f float64) string {
	// 请求 id 都是整数, 直接格式化避免浮点科学计数法。
	return fmt.Sprintf("%d", int64(f))
}

func (b *Backend) deliverResponse(id string, msg map[string]any) {
	resp := &Response{ID: id}
	if raw, ok := msg["result"]; ok && raw != nil {
		if bts, err := json.Marshal(raw); err == nil {
			resp.Result = bts
		}
	}
	if raw, ok := msg["error"]; ok && raw != nil {
		bts, err := json.Marshal(raw)
		if err == nil {
			var e RPCError
			if json.Unmarshal(bts, &e) == nil {
				resp.Error = &e
			} else {
				resp.Error = &RPCError{Code: -32603, Message: string(bts)}
			}
		}
	}
	b.respMu.Lock()
	ch := b.pending[id]
	delete(b.pending, id)
	b.respMu.Unlock()
	if ch == nil {
		// 没人等: 迟到的响应丢弃 (不进通用队列 —— 那里无消费者, 只会积压)。
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

func (b *Backend) routeEvent(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		return
	}
	var ev Event
	bts, err := json.Marshal(params)
	if err != nil {
		return
	}
	if err := json.Unmarshal(bts, &ev); err != nil {
		return
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	b.wire("zcode→bridge", "session/event", ev)

	b.respMu.Lock()
	l := b.listener[ev.SessionID]
	b.respMu.Unlock()
	if l == nil {
		// 没人监听: 忽略 (订阅是按需的)。
		return
	}
	l.observeSeq(ev.Seq)
	select {
	case l.Events <- ev:
	default:
		b.logf("⚠ session/event 通道已满, 丢弃 type=%s seq=%d", ev.Type, ev.Seq)
	}
}

// registerServerRequest 把反向调用交给该会话的监听器; 无人接收时返回 false。
func (b *Backend) registerServerRequest(req *ServerRequest) bool {
	b.respMu.Lock()
	var l *Listener
	if sid := Str(req.Params, "sessionId"); sid != "" {
		l = b.listener[sid]
	}
	// 兜底: 某些构建发出的 interaction/requestUserInput 不带顶层 sessionId
	// (或带的与 session/create 返回的不一致)。桥是「一个会话一个 turn」模型,
	// 只有一个活跃 listener 时目标无歧义 —— 路由过去, 而不是静默 cancel
	// (静默 cancel 会让模型重问, 且已渲染的卡片提交时 404)。
	if l == nil && len(b.listener) == 1 {
		for _, v := range b.listener {
			l = v
		}
		if b.opts.Logger != nil {
			b.logf("  %s 未匹配到会话 listener, 路由到唯一活跃 listener", req.Method)
		}
	}
	b.respMu.Unlock()
	if l == nil {
		return false
	}
	select {
	case l.Requests <- req:
		return true
	default:
		return false
	}
}

func (b *Backend) handleServerRequest(id, method string, msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	req := &ServerRequest{ID: id, Method: method, Params: params}
	b.wire("zcode→bridge", method, req)
	b.observedMu.Lock()
	b.observed[method]++
	b.observedMu.Unlock()

	switch method {
	case "session/requestRuntimePreferences":
		// 必须应答, 否则 session/create|send 永久挂起 (上游 832d647 实测)。
		scope := Str(params, "scope")
		b.logf("  ← 应答 session/requestRuntimePreferences (scope=%s)", scope)
		_ = b.Respond(id, RuntimePreferences, nil)
		return
	case "interaction/requestUserInput", "interaction/requestPermission",
		"interaction/requestProviderRuntimeHeaders":
		// 交给在飞的 turn (Phase 4 的 ask.go / permission.go, 以及
		// provider_auth.go)。无人接收时立即回 cancel / -32601, 绝不让
		// zcode 挂死等待。
		//
		// requestProviderRuntimeHeaders 必须走这里而不是落到 default:
		// 订阅型 provider (含 Start Plan) 在每次模型请求前都会问一次,
		// default 那条 -32601 会被 zcode 读成「宿主缺席」, turn 直接以
		// ModelRequestAuthMissing 告终。
		if b.registerServerRequest(req) {
			return
		}
		if method == "interaction/requestUserInput" {
			b.logf("  ← %s 无在飞 turn, 回 cancel", method)
			_ = b.Respond(id, map[string]any{
				"action": "cancel",
				"reason": "bridge has no interactive turn to ask the user",
			}, nil)
		} else {
			b.logf("  ← %s 无在飞 turn, 回 -32601", method)
			_ = b.Respond(id, nil, &RPCError{Code: -32601, Message: "no interactive turn to request permission"})
		}
		return
	default:
		// 未知反向调用: 一律回 -32601, 避免 server 侧空等挂起 (§2.2 C8)。
		b.logf("  ← 未知 server→client 请求 %s, 应答 -32601", method)
		_ = b.Respond(id, nil, &RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("method not implemented by bridge: %s", method),
		})
	}
}

func (b *Backend) mergeStatePatch(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		return
	}
	sid := Str(params, "sessionId")
	patch := Sub(params, "patch")
	if sid == "" || patch == nil {
		return
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	if _, ok := b.state[sid]; !ok && len(b.state) >= maxSessionStates {
		evicted := b.stateSeq[0]
		b.stateSeq = b.stateSeq[1:]
		delete(b.state, evicted)
		b.logf("⚠ state 投影超上限 (%d), 淘汰最旧 session: %s", maxSessionStates, evicted)
	}
	if b.state[sid] == nil {
		b.state[sid] = map[string]any{}
		b.stateSeq = append(b.stateSeq, sid)
	}
	for k, v := range patch {
		b.state[sid][k] = v
	}
}

// Projection 取 state.updated 合并出的状态投影副本 (无则 nil)。
func (b *Backend) Projection(sid string) map[string]any {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	p, ok := b.state[sid]
	if !ok {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// ResetProjection 清掉某会话的状态投影 (新 turn 入口调用, 防跨 turn 陈旧)。
func (b *Backend) ResetProjection(sid string) {
	b.stateMu.Lock()
	delete(b.state, sid)
	for i, s := range b.stateSeq {
		if s == sid {
			b.stateSeq = append(b.stateSeq[:i], b.stateSeq[i+1:]...)
			break
		}
	}
	b.stateMu.Unlock()
}

func (b *Backend) routeEnhanceResult(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		return
	}
	rid := Str(params, "requestId")
	b.respMu.Lock()
	ch := b.enhance[rid]
	delete(b.enhance, rid)
	b.respMu.Unlock()
	if ch == nil {
		b.logf("prompt/enhance/result (requestId=%s) 无等待者, 丢弃", rid)
		return
	}
	select {
	case ch <- params:
	default:
	}
}

// RegisterEnhance 注册 prompt/enhance 异步 job 的 result 等待通道。
// 必须发 start 之前调用, 否则 start 返回与 result 推送之间到达的通知会丢。
// 该 requestId 已有等待者时返回错误 (重复提交)。
func (b *Backend) RegisterEnhance(requestID string) (<-chan map[string]any, error) {
	ch := make(chan map[string]any, 1)
	b.respMu.Lock()
	defer b.respMu.Unlock()
	if _, ok := b.enhance[requestID]; ok {
		return nil, fmt.Errorf("duplicate requestId %s (已有 pending enhance job)", requestID)
	}
	b.enhance[requestID] = ch
	return ch, nil
}

// UnregisterEnhance 注销 enhance result 等待 (超时/取消时清理)。
func (b *Backend) UnregisterEnhance(requestID string) {
	b.respMu.Lock()
	delete(b.enhance, requestID)
	b.respMu.Unlock()
}

// RegisterListener 注册会话监听器。同 sid 覆盖注册会让前一个 listener 的事件
// 无人消费, 打告警防御未来引入同 session 并发 prompt 时难排查。
func (b *Backend) RegisterListener(l *Listener) {
	b.respMu.Lock()
	if _, ok := b.listener[l.SessionID]; ok {
		b.logf("⚠ RegisterListener: sid=%s 已有 listener, 覆盖注册 (前一个 listener 的事件将无人消费)", l.SessionID)
	}
	b.listener[l.SessionID] = l
	b.respMu.Unlock()
}

// UnregisterListener 注销会话监听器。
func (b *Backend) UnregisterListener(sid string) {
	b.respMu.Lock()
	delete(b.listener, sid)
	b.respMu.Unlock()
}

// write 序列化并写入一行 JSON (无 jsonrpc 字段 —— ZCode Protocol 对内)。
func (b *Backend) write(msg any) error {
	bts, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	bts = append(bts, '\n')
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.stdin == nil {
		return errors.New("zcode stdin 已关闭")
	}
	_, err = b.stdin.Write(bts)
	return err
}

// Send 发一条请求给 zcode (不等响应)。
func (b *Backend) Send(id, method string, params any) error {
	if params == nil {
		params = map[string]any{}
	}
	return b.write(map[string]any{"id": id, "method": method, "params": params})
}

// Notify 发一条通知给 zcode (无 id)。
func (b *Backend) Notify(method string, params any) error {
	if params == nil {
		params = map[string]any{}
	}
	return b.write(map[string]any{"method": method, "params": params})
}

// Respond 应答 zcode 的反向调用。管道断裂时只返回 error, 不 panic。
func (b *Backend) Respond(id string, result any, errObj *RPCError) error {
	reply := map[string]any{"id": id}
	if errObj != nil {
		reply["error"] = errObj
	} else {
		if result == nil {
			result = map[string]any{}
		}
		reply["result"] = result
	}
	b.wire("bridge→zcode", "reply", reply)
	return b.write(reply)
}

// Request 同步请求: 注册 pending, 发送, 等待对应 id 的响应或超时。
//
// 期间到达的事件/通知由 reader goroutine 异步分发到各自的通道, 不会被本方法吞掉
// (这是上游把 reader 拆成独立线程的原因: 修复旧版 request 与事件推送的竞态)。
func (b *Backend) Request(ctx context.Context, id, method string, params any) (*Response, error) {
	if params == nil {
		params = map[string]any{}
	}
	if b.dead.Load() {
		return nil, errors.New(b.ExitDetail())
	}
	ch := make(chan *Response, 1)
	b.respMu.Lock()
	b.pending[id] = ch
	b.respMu.Unlock()

	if err := b.Send(id, method, params); err != nil {
		b.respMu.Lock()
		delete(b.pending, id)
		b.respMu.Unlock()
		b.dead.Store(true)
		return nil, fmt.Errorf("zcode backend pipe broken: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp, resp.Error
		}
		return resp, nil
	case <-ctx.Done():
		b.respMu.Lock()
		delete(b.pending, id)
		b.respMu.Unlock()
		return nil, ctx.Err()
	case <-b.done:
		b.respMu.Lock()
		delete(b.pending, id)
		b.respMu.Unlock()
		return nil, errors.New(b.ExitDetail())
	}
}

// RequestWithTimeout 是带超时的便捷封装。
func (b *Backend) RequestWithTimeout(id, method string, params any, d time.Duration) (*Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return b.Request(ctx, id, method, params)
}

func (b *Backend) failAllPending(err error) {
	b.respMu.Lock()
	defer b.respMu.Unlock()
	for id, ch := range b.pending {
		select {
		case ch <- &Response{ID: id, Error: &RPCError{Code: -32603, Message: err.Error()}}:
		default:
		}
		delete(b.pending, id)
	}
	for rid, ch := range b.enhance {
		select {
		case ch <- map[string]any{"requestId": nil, "status": "failed", "errorMessage": err.Error()}:
		default:
		}
		delete(b.enhance, rid)
	}
}

// Close 终止子进程并等待 reader 收尾。
func (b *Backend) Close() {
	if b == nil || b.cmd == nil {
		return
	}
	if b.stdin != nil {
		_ = b.stdin.Close()
		b.stdin = nil
	}
	if b.cmd.Process != nil {
		// 先给一次优雅终止的机会, 再 kill。reader 会因 stdout 关闭而退出。
		_ = b.cmd.Process.Signal(syscall.SIGTERM)
		graceful := make(chan struct{})
		go func() { b.wg.Wait(); close(graceful) }()
		select {
		case <-graceful:
		case <-time.After(3 * time.Second):
			_ = b.cmd.Process.Kill()
			b.wg.Wait()
		}
	}
	b.wg.Wait()
}

// ---- 协议探测 ----

// DetectProtocol 启动一次性协议探测 (≤0.15 旧协议 vs 0.16+ 新协议), 结果缓存。
//
// 方法: 以旧信封 (含 jsonrpc 键) 发 workspace/readState。
//   - 0.16+ zod 校验拒收 jsonrpc 键 → -32600 invalid_union; 实测该错误帧的 id
//     是固定占位符 "invalid-message" (非请求 id) → 判 v16
//   - ≤0.15 理解信封 → 回显 probe id 的正常响应 → legacy
//   - 超时无法判定 → 暂按 v16, 但标 confirmed=false, 之后 subscribe 失败可推翻
func (b *Backend) DetectProtocol() {
	b.probeMu.Lock()
	done := b.mode != ""
	b.probeMu.Unlock()
	if done {
		return
	}
	if b.dead.Load() {
		// 后端已死, 模式无实际影响; 取当前发行版。confirmed=true: 回退 legacy
		// 轮询同样活不了, 不做无谓降级。
		b.setProbe(ProtocolV16, true)
		return
	}
	const probeID = "bridge-probe-1"
	const rejectID = "invalid-message"
	probeCh := make(chan *Response, 1)
	rejectCh := make(chan *Response, 1)
	b.respMu.Lock()
	b.pending[probeID] = probeCh
	b.pending[rejectID] = rejectCh
	b.respMu.Unlock()

	cwd := "."
	if wd, err := currentDir(); err == nil {
		cwd = wd
	}
	err := b.write(map[string]any{
		"jsonrpc": "2.0", "id": probeID, "method": "workspace/readState",
		"params": map[string]any{
			"workspace": map[string]any{"workspacePath": cwd, "workspaceKey": cwd},
		},
	})
	if err != nil {
		b.finishProbe(probeID, rejectID)
		b.setProbe(ProtocolV16, true)
		b.logf("协议探测: 发送失败 (%v), 按 0.16+ 新协议处理", err)
		return
	}

	// 冷启动慢: 0.16.5 的 app-server 在 1.2GB 会话库下约 20s 才响应
	// (实测 session/create 22.5s, zcode --version 21s), 故探测窗口放宽到
	// ProbeTimeout (默认 45s), 拿到响应即返回。
	deadline := time.Now().Add(b.probeTimeout())
	for time.Now().Before(deadline) {
		select {
		case msg := <-probeCh:
			b.finishProbe(probeID, rejectID)
			b.setProbe(modeOf(msg), true)
			return
		case msg := <-rejectCh:
			b.finishProbe(probeID, rejectID)
			b.setProbe(modeOf(msg), true)
			return
		case <-b.done:
			b.finishProbe(probeID, rejectID)
			b.setProbe(ProtocolV16, true)
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	// 超时兜底: 无实锤, 允许被 subscribe 失败推翻 (本 Go 版不实现轮询降级,
	// 该标记仅供 doctor 与 compat 规则判定参考)。
	b.finishProbe(probeID, rejectID)
	b.setProbe(ProtocolV16, false)
	b.logf("⚠ 协议探测超时 (%s), 暂按 0.16+ 处理 (未确认)", b.probeTimeout())
}

func modeOf(msg *Response) ProtocolMode {
	if isEnvelopeReject(msg) {
		return ProtocolV16
	}
	return ProtocolLegacy
}

// setProbe 写入探测结论并打日志。
func (b *Backend) setProbe(mode ProtocolMode, confirmed bool) {
	b.probeMu.Lock()
	b.mode, b.confirmed = mode, confirmed
	b.probeMu.Unlock()
	tail := ""
	if !confirmed {
		tail = " (超时兜底未确认)"
	}
	b.logf("协议探测: mode=%s (%s)%s", mode, mode.String(), tail)
}

// probeTimeout 返回探测窗口。默认 45s, 覆盖 0.16.5 的冷启动实测 (~20s)。
func (b *Backend) probeTimeout() time.Duration {
	if b.opts.ProbeTimeout > 0 {
		return b.opts.ProbeTimeout
	}
	return 45 * time.Second
}

func (b *Backend) finishProbe(ids ...string) {
	b.respMu.Lock()
	for _, id := range ids {
		delete(b.pending, id)
	}
	b.respMu.Unlock()
}

// isEnvelopeReject 判定响应是否为 0.16+ 的信封拒绝 (-32600 / invalid_union)。
func isEnvelopeReject(msg *Response) bool {
	if msg == nil || msg.Error == nil {
		return false
	}
	if msg.Error.Code == -32600 {
		return true
	}
	blob := string(msg.Error.Data) + msg.Error.Message
	return strings.Contains(blob, "invalid_union") || strings.Contains(blob, "unrecognized_keys")
}

// ---- stderr 环形缓冲 ----

type stderrRing struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

func newStderrRing(cap int) *stderrRing { return &stderrRing{cap: cap} }

func (r *stderrRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ln := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		r.lines = append(r.lines, ln)
		if len(r.lines) > r.cap {
			r.lines = r.lines[len(r.lines)-r.cap:]
		}
	}
	return len(p), nil
}

func (r *stderrRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}
