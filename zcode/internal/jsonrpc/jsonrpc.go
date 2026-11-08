// Package jsonrpc 是 ACP 侧的 JSON-RPC 2.0 over stdio 连接层。
//
// 设计决策 (PORTING_PLAN §8 D3): 不用 acp-go-sdk 的连接层, 自写分发 (~200 行)。
// 原因: SDK 的 ExtensionMethodHandler 只路由 _ 前缀方法, 而桥要保留上游的
// 旧扩展方法名 (session/fork、workspace/* 等) 以兼容现有客户端 (omnigent 等)。
//
// 并发模型: 读循环每条请求 spawn 一个 goroutine 跑 handler —— session/prompt
// 是分钟级阻塞操作, 不能卡住读循环, 否则 prompt 期间的 session/cancel 永远
// 到不了 handler (上游用 inbox 队列 + _drain_inbox 解决同一问题)。
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// maxLine 与 backend 相同: 单行 JSON 上限 16 MB。
const maxLine = 16 << 20

// Error 是 JSON-RPC 错误对象。
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// Errf 构造 *Error。
func Errf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Handler 处理一条 client→agent 请求。返回 result (可为 nil) 与错误;
// 两者都为 nil 时写回空 result。
type Handler func(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *Error)

// NotificationHandler 处理一条 notification (无 id, 无响应)。
type NotificationHandler func(ctx context.Context, params json.RawMessage)

// Conn 是一条 JSON-RPC 连接 (通常 stdin/stdout)。
type Conn struct {
	w       io.Writer
	writeMu sync.Mutex // 帧级写锁: 响应/通知/agent→client 请求共用 (上游 P1-2)

	mu       sync.Mutex
	handlers map[string]Handler
	notifs   map[string]NotificationHandler

	pendingMu sync.Mutex
	pending   map[string]*pendingReq
	nextID    atomic.Int64

	wg     sync.WaitGroup
	logger func(format string, args ...any)
	// traceNotif 打开「未注册通知被忽略」这类高频诊断行。它**不**由 serve -v
	// 派生: 一次真实 turn 能收到上千条未知通知, -v 下打印会淹掉其它诊断。
	// 独立开关见 ZCODE_ACP_TRACE_NOTIF。
	traceNotif bool
	// ignoredNotif 按 method 聚合被忽略的通知次数, 供该模式打累计数。
	ignoredNotif sync.Map // method(string) → *atomic.Int64
}

// New 构造连接。logger 为 nil 时丢弃诊断日志。
//
// traceNotif 只影响「未注册通知被忽略」这类高频噪声; 其余诊断 (panic 恢复、
// 非 JSON 行、迟到应答) 始终打印, 它们一 turn 也就零星几条。
func New(w io.Writer, logger func(format string, args ...any), traceNotif bool) *Conn {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Conn{
		w:          w,
		handlers:   map[string]Handler{},
		notifs:     map[string]NotificationHandler{},
		pending:    map[string]*pendingReq{},
		logger:     logger,
		traceNotif: traceNotif,
	}
}

// Handle 注册请求方法。
func (c *Conn) Handle(method string, h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[method] = h
}

// HandleNotification 注册通知方法。未注册的通知静默忽略 (ACP 客户端可能发
// notifications/initialized 等)。
func (c *Conn) HandleNotification(method string, h NotificationHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifs[method] = h
}

// frame 是线上的一帧。ID 用 RawMessage 原样保存 —— ACP 客户端的 id 是
// string 或 number, 响应必须原样回写, 不能重排类型。
type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Run 是读循环: 阻塞直到 reader 结束或 ctx 取消。
func (c *Conn) Run(ctx context.Context, r io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		// EOF and scanner errors are connection cancellation, too. Release
		// handlers before joining them, including reverse-RPC callbacks.
		cancel()
		c.wg.Wait()
	}()
	// A closable stream can also be interrupted while Scan is blocked.
	// Callers with a non-closable reader must arrange to unblock its Read.
	if closer, ok := r.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := trimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			// 单行坏帧不杀死整条连接 (客户端某次 bug 不应桥接全断)。
			c.logger("⚠ ACP 侧非 JSON 行, 跳过: %.120s", line)
			continue
		}
		c.dispatch(ctx, f)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

func (c *Conn) dispatch(ctx context.Context, f frame) {
	hasID := len(f.ID) > 0 && string(f.ID) != "null"
	switch {
	case hasID && f.Method != "":
		// client→agent 请求
		c.mu.Lock()
		h := c.handlers[f.Method]
		c.mu.Unlock()
		if h == nil {
			_ = c.write(frame{JSONRPC: "2.0", ID: f.ID, Error: Errf(-32601, "Method not supported: %s", f.Method)})
			return
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					c.logger("⚠ handler %s panic: %v", f.Method, r)
					_ = c.write(frame{JSONRPC: "2.0", ID: f.ID,
						Error: Errf(-32603, "internal error: %v", r)})
				}
			}()
			result, rerr := h(ctx, f.ID, f.Params)
			if rerr == nil && result == nil {
				result = map[string]any{}
			}
			_ = c.write(frame{JSONRPC: "2.0", ID: f.ID, Result: marshalOrNull(result), Error: rerr})
		}()

	case hasID && f.Method == "":
		// client 对 agent→client 请求的响应: id 路由到 pending。
		p := c.takePending(rawIDKey(f.ID))
		if p == nil {
			return
		}
		// 与放弃路径 (RequestAbandonable) 抢同一个 CAS: 谁先谁说了算。
		// 输了说明这条请求已被放弃、桥已经补写过结果帧, 再投递就会给同一个
		// id 发两帧 (JSON-RPC 违规), 只能丢弃。
		if !p.claim() {
			c.logger("  [late-reply] 客户端对已放弃的请求 %s 的应答被丢弃", rawIDKey(f.ID))
			return
		}
		resp := map[string]json.RawMessage{}
		if len(f.Result) > 0 {
			resp["result"] = f.Result
		}
		if f.Error != nil {
			if bts, err := json.Marshal(f.Error); err == nil {
				resp["error"] = bts
			}
		}
		select {
		case p.ch <- resp:
		default:
		}

	case !hasID && f.Method != "":
		// notification
		c.mu.Lock()
		h := c.notifs[f.Method]
		c.mu.Unlock()
		if h == nil {
			c.logIgnoredNotification(f.Method)
			return
		}
		h(ctx, f.Params)
	}
}

// logIgnoredNotification 记录一条未注册通知被忽略, 按 method 聚合:
// 首次打一条, 之后每 100 条再打一条累计数 (单 turn 上千条 telemetry 通知)。
// 默认 (含 serve -v) 完全不打, 需 ZCODE_ACP_TRACE_NOTIF=1。
func (c *Conn) logIgnoredNotification(method string) {
	if !c.traceNotif {
		return
	}
	const suppressedEvery = 100
	v, _ := c.ignoredNotif.LoadOrStore(method, new(atomic.Int64))
	n := v.(*atomic.Int64).Add(1)
	if n == 1 {
		c.logger("  [notification] 忽略 %s (后续同类不再逐条打印)", method)
		return
	}
	if n%suppressedEvery == 0 {
		c.logger("  [notification] 忽略 %s ×%d", method, n)
	}
}

func marshalOrNull(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	bts, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return bts
}

func rawIDKey(raw json.RawMessage) string {
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// write 写一帧 (加锁, 保证 JSON 行不交错)。
func (c *Conn) write(f frame) error {
	bts, err := json.Marshal(f)
	if err != nil {
		return err
	}
	bts = append(bts, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(bts)
	return err
}

// Notify 向 client 发通知。
func (c *Conn) Notify(method string, params any) error {
	return c.write(frame{JSONRPC: "2.0", Method: method, Params: marshalOrNull(params)})
}

// pendingReq 是一条在飞的 agent→client 请求。
type pendingReq struct {
	ch   chan map[string]json.RawMessage
	once atomic.Bool // 已被应答或已被放弃; CAS 保证只有一个赢家
}

// claim 抢占这条请求。返回 false 表示另一方 (客户端应答 / 放弃路径) 已经抢到。
func (p *pendingReq) claim() bool { return p.once.CompareAndSwap(false, true) }

// Request 发一条 agent→client 请求并等响应 (session/request_permission 等)。
// result/out 写入响应的 result (可为 nil 表示不需要解析)。
//
// ctx 取消时单方面放弃等待, 客户端那条请求**收不到任何应答** —— 对
// session/request_permission 这类有 UI 的方法就是挂死。需要给客户端收尾的调用
// 走 RequestAbandonable。
func (c *Conn) Request(ctx context.Context, method string, params any, out any) *Error {
	return c.RequestAbandonable(ctx, method, params, out, nil)
}

// RequestAbandonable 是 Request 的可放弃变体: ctx 取消导致放弃等待时, 若确实
// 抢到了这条请求 (客户端还没应答), 先把它从 pending 摘掉再调 onAbandon(id),
// 由调用方给客户端补一个结果帧 (见 ReplyResult)。onAbandon 为 nil 时等价于
// Request。
//
// 为什么需要: 客户端把 session/request_permission 的应答一直接受就永远等不到
// 下一个 id, UI 停在"等待授权"。T02 要求取消/超时/断连三条路径上客户端都能收尾。
func (c *Conn) RequestAbandonable(ctx context.Context, method string, params any, out any, onAbandon func(id string)) *Error {
	id := fmt.Sprintf("brq-%d", c.nextID.Add(1))
	p := &pendingReq{ch: make(chan map[string]json.RawMessage, 1)}
	c.pendingMu.Lock()
	c.pending[id] = p
	c.pendingMu.Unlock()

	if err := c.write(frame{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Method: method, Params: marshalOrNull(params)}); err != nil {
		c.dropPending(id)
		return Errf(-32603, "agent→client 请求发送失败: %v", err)
	}

	select {
	case resp := <-p.ch:
		if rawErr, ok := resp["error"]; ok {
			var e Error
			if json.Unmarshal(rawErr, &e) == nil {
				return &e
			}
			return Errf(-32603, "client 返回未知错误形态: %s", rawErr)
		}
		if out != nil {
			if raw, ok := resp["result"]; ok {
				_ = json.Unmarshal(raw, out)
			}
		}
		return nil
	case <-ctx.Done():
		// 只有抢到才补结果帧; 输了说明客户端恰好同时应答了, 由 dispatch 投递,
		// 这时再写一帧就成了同一 id 两帧。
		if p.claim() {
			c.dropPending(id)
			if onAbandon != nil {
				onAbandon(id)
			}
		}
		return Errf(-32603, "等待 client 响应超时或取消 (%s): %v", method, ctx.Err())
	}
}

// ReplyResult 给一条已放弃的 agent→client 请求补写结果帧 (id 传裸字符串,
// 本包生成的 id 一定是 string 形态)。
func (c *Conn) ReplyResult(id string, result any) error {
	return c.write(frame{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Result: marshalOrNull(result)})
}

func (c *Conn) takePending(id string) *pendingReq {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	p := c.pending[id]
	delete(c.pending, id)
	return p
}

func (c *Conn) dropPending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}
