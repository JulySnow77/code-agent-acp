package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/compat"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// cmdRecord 录制协议样本 (PORTING_PLAN §5.3: ZCode 升级后的固定动作)。
//
// 录下: session/create 的请求与响应、session/subscribe 的请求与响应、
// 一次 turn 的全部 session/event、出现过的所有 server→client 请求方法名。
// 产物落在 testdata/zcode-<版本>/, 与上一版做结构 diff 即可发现协议漂移。
func cmdRecord(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record", stderr)
	out := fs.String("out", "", "输出目录 (默认 testdata/zcode-<CLI版本>)")
	prompt := fs.String("prompt", "只回复两个字: 收到", "录制用 prompt")
	cwd := fs.String("cwd", "", "工作区路径 (默认当前目录)")
	turnTimeout := fs.Duration("turn-timeout", 3*time.Minute, "等待 turn 结束的最长时间")
	model := fs.String("model", "", "建会话后 setModel 的 providerId/modelId (可选)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := newLogger(stderr)
	b := newBootstrap(log)
	if err := b.prepare(); err != nil {
		fmt.Fprintln(stderr, "record: 环境自检失败:", err)
		return 1
	}
	if v, err := detectZcodeVersion(b.report.ResolvedBin, b.env); err != nil {
		log("⚠ zcode --version 探测失败: %v", err)
	} else {
		b.version = v
	}

	dir := *out
	if dir == "" {
		ver := b.version.String()
		if b.version.Zero() {
			ver = "unknown"
		}
		dir = filepath.Join("testdata", "zcode-"+ver)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(stderr, "record: 创建输出目录失败:", err)
		return 1
	}

	be, err := b.start()
	if err != nil {
		fmt.Fprintln(stderr, "record: 启动后端失败:", err)
		return 1
	}
	defer be.Close()

	workDir := *cwd
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		} else {
			workDir = "."
		}
	}
	rw := compat.Rewriter{Version: b.version, Timeout: b.timeouts}
	nextID := 10_000_000

	write := func(name string, v any) {
		bts, _ := json.MarshalIndent(v, "", "  ")
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, append(bts, '\n'), 0o644); err != nil {
			log("⚠ 写入 %s 失败: %v", p, err)
			return
		}
		fmt.Fprintf(stdout, "  → %s\n", p)
	}

	fmt.Fprintf(stdout, "录制 ZCode 协议样本到 %s\n", dir)

	// ---- session/create ----
	createParams := map[string]any{
		"workspace": map[string]any{"workspacePath": workDir, "workspaceKey": workDir},
		"mode":      b.cfg.Mode,
	}
	createParams, createTimeout, err := rw.Rewrite("session/create", createParams)
	if err != nil {
		fmt.Fprintln(stderr, "record: 改写 create 参数失败:", err)
		return 1
	}
	nextID++
	createID := fmt.Sprint(nextID)
	write("session-create.request.json", map[string]any{"id": createID, "method": "session/create", "params": createParams})
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	start := time.Now()
	resp, err := be.Request(ctx, createID, "session/create", createParams)
	cancel()
	if err != nil {
		fmt.Fprintln(stderr, "record: session/create 失败:", err)
		return 1
	}
	write("session-create.response.json", decode(resp.Result))
	fmt.Fprintf(stdout, "  session/create 耗时 %.1fs\n", time.Since(start).Seconds())

	createResult := resp.ResultMap()
	sid := zcode.Str(zcode.Sub(createResult, "session"), "sessionId")
	if sid == "" {
		sid = zcode.Str(createResult, "sessionId")
	}
	if sid == "" {
		fmt.Fprintln(stderr, "record: session/create 未返回 sessionId")
		return 1
	}
	catalog := zcode.ParseModelCatalog(createResult)
	// setModel 的参数升格 (C4) 需要模型目录才能解析 providerId 与缺省
	// reasoningLevel, 目录只在 create 之后才有。
	rw.Models = catalog

	// ---- 可选 setModel (C4 样本) ----
	if *model != "" {
		params, timeout, rwErr := rw.Rewrite("session/setModel", map[string]any{"sessionId": sid, "modelId": *model})
		if rwErr != nil {
			log("⚠ setModel 改写失败: %v", rwErr)
		} else {
			nextID++
			id := fmt.Sprint(nextID)
			write("session-setmodel.request.json", map[string]any{"id": id, "method": "session/setModel", "params": params})
			sctx, scancel := context.WithTimeout(context.Background(), timeout)
			sresp, serr := be.Request(sctx, id, "session/setModel", params)
			scancel()
			if serr != nil {
				log("⚠ setModel 失败: %v", serr)
			} else {
				write("session-setmodel.response.json", decode(sresp.Result))
			}
		}
	}
	_ = catalog

	// ---- session/subscribe ----
	l := zcode.NewListener(sid)
	be.RegisterListener(l)
	defer be.UnregisterListener(sid)

	subParams, subTimeout, _ := rw.Rewrite("session/subscribe", map[string]any{"sessionId": sid})
	nextID++
	subID := fmt.Sprint(nextID)
	write("session-subscribe.request.json", map[string]any{"id": subID, "method": "session/subscribe", "params": subParams})
	sctx, scancel := context.WithTimeout(context.Background(), subTimeout)
	subResp, err := be.Request(sctx, subID, "session/subscribe", subParams)
	scancel()
	if err != nil {
		fmt.Fprintln(stderr, "record: session/subscribe 失败:", err)
		return 1
	}
	write("session-subscribe.response.json", decode(subResp.Result))

	// ---- session/send + 收集事件 ----
	nextID++
	sendID := fmt.Sprint(nextID)
	sendParams := map[string]any{"sessionId": sid, "content": *prompt}
	sendParams, sendTimeout, _ := rw.Rewrite("session/send", sendParams)
	write("session-send.request.json", map[string]any{"id": sendID, "method": "session/send", "params": sendParams})

	sctx, scancel = context.WithTimeout(context.Background(), sendTimeout)
	sendResp, err := be.Request(sctx, sendID, "session/send", sendParams)
	scancel()
	if err != nil {
		fmt.Fprintln(stderr, "record: session/send 失败:", err)
		return 1
	}
	write("session-send.response.json", decode(sendResp.Result))

	events := make([]zcode.Event, 0, 64)
	serverMethods := map[string]int{}
	deadline := time.Now().Add(*turnTimeout)
	for time.Now().Before(deadline) {
		select {
		case ev := <-l.Events:
			events = append(events, ev)
			if ev.Kind() == "turn.completed" || ev.Kind() == "turn.failed" {
				goto done
			}
		case req := <-l.Requests:
			serverMethods[req.Method]++
			// 录制时不实现人在回路: 回 cancel / -32601, 让 turn 尽快收尾。
			if req.Method == "interaction/requestUserInput" {
				_ = be.Respond(req.ID, map[string]any{"action": "cancel", "reason": "record: no interactive turn"}, nil)
			} else {
				_ = be.Respond(req.ID, nil, &zcode.RPCError{Code: -32601, Message: "record: not implemented"})
			}
		case <-time.After(200 * time.Millisecond):
			if be.Dead() {
				goto done
			}
		}
	}
done:
	writeJSONL(dir, "events.jsonl", events, log, stdout)
	for m, n := range be.ObservedServerMethods() {
		serverMethods[m] += n
	}
	write("server-requests.json", serverMethods)
	if len(events) == 0 {
		fmt.Fprintln(stderr, "record: 未收到任何 session/event (协议可能已漂移)")
		return 1
	}
	fmt.Fprintf(stdout, "  录制完成: %d 个事件, %d 种 server 请求\n", len(events), len(serverMethods))
	return 0
}

// writeJSONL 按 JSON Lines 写事件序列, 一行一条 (便于 diff 与逐条回放成测试)。
func writeJSONL(dir, name string, events []zcode.Event, log func(string, ...any), stdout io.Writer) {
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		log("⚠ 写入 %s 失败: %v", p, err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			log("⚠ 写入 %s 失败: %v", p, err)
			return
		}
	}
	fmt.Fprintf(stdout, "  → %s (%d 行)\n", p, len(events))
}

// decode 把 RawMessage 解析成可打印的结构 (失败时保留原字符串)。
func decode(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}
