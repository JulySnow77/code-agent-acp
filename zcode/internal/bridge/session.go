package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/compat"
	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// acpProtocolVersion 与 agentInfo 对齐上游 (BRIDGE_INFO)。
const (
	acpProtocolVersion = 1
	agentName          = "zcode-acp-bridge"
	agentTitle         = "ZCode (Go bridge)"
)

// session 是一个 ACP 会话的桥侧状态。
type session struct {
	acpSID   string
	zcodeSID string
	// cwd 是这个会话的工作区。session/resume 省略 cwd 时用它回填 —— 见
	// resolveResumeCwd 里那个坑: 不回填的话工作区会漂到桥的启动目录。
	cwd       string
	catalog   *zcode.ModelCatalog // session/create 缓存的模型目录 (C3/C4)
	subscribe bool                // 是否已成功订阅过事件流
}

// activeTurn 是一个在飞 prompt 的取消句柄。
type activeTurn struct {
	cancel     chan struct{}
	cancelOnce sync.Once
	promptID   string
	// abort 掐断**正在等答案的交互**。cancel 通道只是让 turn 循环下一轮就退出,
	// 而循环此刻正停在 handlePermission/handleUserInput 的 case body 里等客户端 ——
	// 不掐这一把, 取消最多要等 compat.Question (默认 10 分钟) 才生效。
	// 可能为 nil (turn 还没建好 interactCtx), 此时 cancel 语义不变。
	abort context.CancelFunc
}

// Bridge 是 ACP Agent 主体: 持有 zcode backend, 处理 ACP 方法。
//
// P3 架构裁决 (会话/后端隔离评估): **所有 ACP 会话共享一个 zcode 后端进程**,
// 不建 per-session 后端。依据: ① zcode 冷启动 20s 量级 (zcode-protocol.md C2),
// per-session 后端让建会话成本不可接受; ② zcode app-server 本来就是多会话设计
// (session/create 每会话独立 workspace/mode/权限上下文), 工作区隔离由后端按
// 会话承载, 桥不重复造; ③ 同会话 turn 串行由 promptGates 闸口保证 (排队深度
// 上限 maxQueuedPrompts), 跨会话 turn 并行 —— 这正是 L1 桥该有的形状。
type Bridge struct {
	backend  *zcode.Backend
	rewriter compat.Rewriter
	timeouts compat.Timeouts
	conn     *jsonrpc.Conn
	logger   func(format string, args ...any)
	model    string // ZCODE_ACP_MODEL
	mode     string // 缺省会话权限模式: ZCODE_ACP_MODE / --mode (空→yolo)

	mu         sync.Mutex
	sessions   map[string]*session    // acp sid → session
	turns      map[string]*activeTurn // zcode sid → 在飞 turn
	clientCaps clientCaps             // initialize 里声明的客户端能力 (T03)
	nextID     int

	// T003 flow-questions v1: 停泊交互表 + 后端连接身份 (native_ref.connection_id,
	// 每次 Bridge 启动分配一次, 问题通知与活路径回执保持一致)。
	flow          *flowHub
	backendConnID string

	// promptGates 是每会话的 turn 闸口 (P3): 容量 1 的 channel, 持令牌者才
	// 准进 runTurn —— 同会话并发 prompt 从 -32600 硬拒绝改为排队等待。
	// promptQueued 记每个会话**正在排队**的 prompt 数, 封顶 maxQueuedPrompts,
	// 超了仍 -32600 (排队不是无界缓冲)。
	promptGates  map[string]chan struct{}
	promptQueued map[string]int
}

// Options 构造 Bridge。
type Options struct {
	Backend  *zcode.Backend
	Version  zcode.Version
	Timeouts compat.Timeouts
	Model    string // ZCODE_ACP_MODEL: 建会话后立即 setModel
	Mode     string // 缺省会话权限模式 (ZCODE_ACP_MODE / --mode); 空→yolo
	Logger   func(format string, args ...any)
}

// New 构造 Bridge 并在 conn 上注册 ACP 方法。调用方随后 conn.Run。
func New(opts Options, conn *jsonrpc.Conn) *Bridge {
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}
	if opts.Timeouts.Create == 0 {
		opts.Timeouts = compat.DefaultTimeouts()
	}
	br := &Bridge{
		backend:  opts.Backend,
		rewriter: compat.Rewriter{Version: opts.Version, Timeout: opts.Timeouts},
		timeouts: opts.Timeouts,
		conn:     conn,
		logger:   opts.Logger,
		sessions: map[string]*session{},
		turns:    map[string]*activeTurn{},
		nextID:   10_000_000,
		model:    opts.Model,
		mode:     opts.Mode,

		promptGates:  map[string]chan struct{}{},
		promptQueued: map[string]int{},

		backendConnID: fmt.Sprintf("zbconn-%d", time.Now().UnixNano()),
	}
	br.flow = newFlowHub(flowHubDeps{
		Notify: func(method string, params map[string]any) error {
			return conn.Notify(method, params)
		},
		Request: func(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
			resp, err := br.backend.Request(ctx, br.nextZcodeID(), method, params)
			if err != nil {
				return nil, err
			}
			return resp.ResultMap(), nil
		},
		StopTurn: br.stopBackendTurn,
		Logger:   opts.Logger,
	})
	br.register(conn)
	return br
}

// nextZcodeID 生成对 zcode 请求的 id (基数 10_000_001, 与 zcode 主动请求的
// id 空间错开 —— 上游 P1-1)。
func (br *Bridge) nextZcodeID() string {
	br.mu.Lock()
	defer br.mu.Unlock()
	br.nextID++
	return fmt.Sprintf("%d", br.nextID)
}

// request 对 zcode 发请求, 走 compat.Rewrite 改写参数并应用超时。
func (br *Bridge) request(method string, params map[string]any) (*zcode.Response, error) {
	rewritten, timeout, err := br.rewriter.Rewrite(method, params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return br.backend.Request(ctx, br.nextZcodeID(), method, rewritten)
}

func (br *Bridge) register(conn *jsonrpc.Conn) {
	conn.Handle("initialize", br.handleInitialize)
	conn.Handle("session/new", br.handleSessionNew)
	conn.Handle("session/prompt", br.handleSessionPrompt)
	conn.Handle("session/list", br.handleSessionList)
	conn.Handle("session/resume", br.handleSessionResume)
	conn.Handle("session/load", br.handleSessionLoad)                         // P2: loadSession 能力
	conn.Handle("session/set_mode", br.handleSessionSetMode)                  // T04 标准名
	conn.Handle("session/set_config_option", br.handleSessionSetConfigOption) // T04 标准名
	conn.HandleNotification("session/cancel", br.handleSessionCancel)
	// T003 flow-questions v1: daemon → 桥的两个扩展入口。
	conn.Handle("_multica/flow/answer", br.handleFlowAnswer)
	conn.Handle("_multica/flow/suspend", br.handleFlowSuspend)
	br.registerPassthrough(conn)
}

// ---- initialize ----

func (br *Bridge) handleInitialize(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		ProtocolVersion int `json:"protocolVersion"`
		ClientInfo      struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
		// 部分客户端 (如 Zed) 会在 initialize 带 mcpServers, 请 agent 代为连接
		// MCP server。acp-go-sdk v0.13.5 的 stable/unstable schema 都没有这个
		// 参数 (只在 session/load 里有), 属于事实上的客户端扩展。
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	_ = json.Unmarshal(params, &p)
	br.logger("initialize: client protocolVersion=%d, client=%s", p.ProtocolVersion, p.ClientInfo.Name)

	// P2 拍板 (mcpServers 三选一): **显式告警的忽略**。zcode 只从用户配置
	// ~/.zcode/cli/config.json 的 mcp.servers 读 MCP (0.16.5 实测, 顶层
	// mcpServers 键不被读取), 后端没有按会话注入 MCP 的通道 —— 桥假装支持
	// 会让客户端以为工具已就位。带了的客户端在这里得到明确指引, 不静默吞。
	if m := strings.TrimSpace(string(p.MCPServers)); m != "" && m != "null" && m != "[]" && m != "{}" {
		br.logger("⚠ initialize 收到 mcpServers (%d bytes), 但 zcode 不支持按连接注入 MCP "+
			"—— MCP server 请配置到 ~/.zcode/cli/config.json 的 mcp.servers; 该参数被忽略", len(m))
	}

	// T03: 记住客户端能力。问答走哪条路完全取决于这一步 —— 未声明的一律当不支持
	// (ACP: "MUST treat all capabilities omitted in the initialize request as
	// UNSUPPORTED")。
	caps := parseClientCaps(params)
	br.mu.Lock()
	br.clientCaps = caps
	br.mu.Unlock()
	br.logger("initialize: elicitation.form=%v", caps.elicitationForm)

	// T04: 专有方法收敛到扩展边界 —— 规范名是 _zcode/ 前缀 (registerPassthrough
	// 同时接受裸名兼容旧客户端), 这里向客户端声明扩展的存在与清单。
	// ACP 的扩展发现机制就是 initialize 响应的 _meta (schema: "_meta … reserved
	// for extensions")。
	return map[string]any{
		"protocolVersion": acpProtocolVersion,
		"agentCapabilities": map[string]any{
			// P2: loadSession 由 session/load 承接 (→ zcode session/resume)。
			"loadSession": true,
			// promptCapabilities 三项全 false 是**如实声明**: text 与
			// resource_link 是 ACP baseline (PromptCapabilities 的 schema 描述:
			// "beyond the baseline (text and resource links)"), 不计入能力位;
			// image/audio/embeddedContext zcode 的 session/send (content 必须是
			// string, C5) 表达不了。
			"promptCapabilities":  map[string]any{"image": false, "audio": false, "embeddedContext": false},
			"mcpCapabilities":     map[string]any{"http": false, "sse": false},
			"sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}},
		},
		"agentInfo": map[string]any{
			"name": agentName, "title": agentTitle, "version": bridgeVersion,
		},
		"authMethods": []any{},
		"_meta": map[string]any{
			"_zcode": map[string]any{
				"version":       1,
				"methods":       extMethodNames(),
				"cancelRequest": true,
			},
			// T003 flow-questions v1: 声明无损题型/稳定问题 id/挂起证明/消费回执
			// 能力; 未声明 flowQuestions 的客户端不受影响。
			"multica": flowQuestionsMeta(),
		},
	}, nil
}

// bridgeVersion 由 cmd 注入 (避免 import cycle: cmd → bridge 单向)。
// 默认值与 cmd 侧的 defaultBridgeVersion 保持一致 —— 注入发生前 initialize 报的
// 也必须是项目自己的版本号, 不能是空或占位。
var bridgeVersion = "00.00.01"

// SetVersion 注入桥版本号 (initialize 的 agentInfo.version)。
func SetVersion(v string) { bridgeVersion = v }

// ---- session/new ----

func (br *Bridge) handleSessionNew(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		Cwd  string `json:"cwd"`
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(params, &p)
	cwd := p.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		} else {
			cwd = "."
		}
	}
	// 缺省模式解析: 客户端显式 mode → 桥配置默认 (ZCODE_ACP_MODE / --mode,
	// 兼容旧 ZCODE_ACP_DEFAULT_MODE) → yolo。yolo 跳过工具权限确认, 注重安全
	// 可设 ZCODE_ACP_MODE=build 收紧。
	mode := p.Mode
	if mode == "" {
		mode = br.mode
	}
	if mode == "" {
		mode = "yolo"
	}
	if !validZcodeMode(mode) {
		return nil, jsonrpc.Errf(-32602, "未知模式 %q (可选: build/edit/plan/yolo)", mode)
	}
	if mode == "yolo" {
		br.logger("⚠ session/new 缺省 mode=yolo (跳过工具权限确认); 注重安全可设 ZCODE_ACP_MODE=build")
	}

	// 账号快照: 无头启动没有桌面宿主替我们推送订阅状态, 不推的话每个
	// account:* provider 都会读成 entitled:false, 用户的订阅模型整个从
	// settings.model.available 消失 (实测 setModel 报 "Provider Registry 中不存在
	// Model")。必须在 session/create 之前推 —— 注册表在建会话时就定型了。
	br.pushAccountProviderConfig()

	createParams := map[string]any{
		"workspace": map[string]any{"workspacePath": cwd, "workspaceKey": cwd},
		"mode":      mode,
	}
	// T003: 协商了 flowQuestions 的连接, 其会话向 zcode 声明 flow 模式 ——
	// v4 自动回答关闭、未决交互进持久 store、可按 interaction_id 恢复。
	// 0.16+ 新协议的 zod 校验层拒收 create 的 _meta (Unrecognized key,
	// 实测 3.16.2 app-server 直接 -32603); 声明只对旧协议服务端发送,
	// v16 (含探测未完成的缺省 v16) 一律不带。
	if br.supportsFlowQuestions() && br.backend.Mode() != zcode.ProtocolV16 {
		createParams["_meta"] = map[string]any{"multica": map[string]any{"flowQuestions": "v1"}}
	}
	resp, err := br.request("session/create", createParams)
	if err != nil {
		return nil, jsonrpc.Errf(-32603, "zcode create failed: %v", rpcMessage(err))
	}
	result := resp.ResultMap()
	zcodeSID := zcode.Str(zcode.Sub(result, "session"), "sessionId")
	if zcodeSID == "" {
		zcodeSID = zcode.Str(result, "sessionId") // 兼容顶层
	}
	if zcodeSID == "" {
		return nil, jsonrpc.Errf(-32603, "zcode create returned no sessionId")
	}
	catalog := zcode.ParseModelCatalog(result)

	s := &session{acpSID: zcodeSID, zcodeSID: zcodeSID, cwd: cwd, catalog: catalog}
	br.mu.Lock()
	br.sessions[s.acpSID] = s
	br.mu.Unlock()

	// ZCODE_ACP_MODEL: 建会话后立即 setModel (模型不在目录时提前报, 而不是
	// 让 zcode 在首个 turn 才拒)。成功后的响应带着新 settings —— 下面的
	// configOptions 要用它, 否则投放的「当前模型」是 setModel 之前的旧值
	// (真机实测: 显示成 create 缺省的 opus, 而会话实际已切 GLM)。
	displayResult := result
	if br.model != "" {
		if r, err := br.setModel(s, br.model); err != nil {
			br.logger("⚠ ZCODE_ACP_MODEL=%s 设定失败: %v", br.model, err)
		} else {
			br.logger("✓ ZCODE_ACP_MODEL=%s 已生效", br.model)
			if len(r) > 0 {
				displayResult = r
			}
		}
	}
	br.logger("session/new → %s (mode=%s, %d 模型)", shortID(s.acpSID), mode, len(catalog.Entries))
	// T04: 按规范投放 modes 与 configOptions (模型目录)。当前模式取后端
	// settings, 后端没给就用请求里的 mode 兜底 —— currentModeId 必填, 不能省。
	// configOptions 的选项用 create 时的完整目录; 当前值优先 setModel 之后的
	// settings (ZCODE_ACP_MODEL 已切模型时, create 里的 current 是旧值)。
	out := map[string]any{"sessionId": s.acpSID}
	out["modes"] = modesState(currentModeFromResult(result, mode))
	curRef, ok := currentModelRef(displayResult)
	if !ok {
		curRef, ok = currentModelRef(result)
	}
	if ok {
		if co := configOptionsFor(catalog, curRef); co != nil {
			out["configOptions"] = co
		}
	}
	return out, nil
}

// setModel 对会话设定模型。spec: providerId/modelId[/reasoningLevel]。
// 旧 {modelId} 升格由 compat 规则 setmodel-upgrade 完成。
func (br *Bridge) setModel(s *session, spec string) (map[string]any, error) {
	// compat.Rewriter 的 Models 为 nil 时不解析目录; 这里显式带上会话目录。
	params := map[string]any{"sessionId": s.zcodeSID, "modelId": spec}
	if i := strings.LastIndex(spec, "/"); i >= 0 && s.catalog.Find(spec[:i], spec[i+1:]) != nil {
		// spec 已含合法 providerId/modelId: 拆开走新格式, 避免 "/" 被当 modelId 的一部分。
		params = map[string]any{
			"sessionId": s.zcodeSID,
			"model": map[string]any{
				"providerId": spec[:i],
				"modelId":    spec[i+1:],
			},
		}
	}
	rewriter := br.rewriter
	rewriter.Models = s.catalog
	rewritten, timeout, err := rewriter.Rewrite("session/setModel", params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := br.backend.Request(ctx, br.nextZcodeID(), "session/setModel", rewritten)
	if err != nil {
		return nil, err
	}
	return resp.ResultMap(), nil
}

// ---- session/prompt ----

func (br *Bridge) handleSessionPrompt(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		SessionID string          `json:"sessionId"`
		Prompt    json.RawMessage `json:"prompt"`
		Content   json.RawMessage `json:"content"` // 别名兜底
	}
	_ = json.Unmarshal(params, &p)

	br.mu.Lock()
	s := br.sessions[p.SessionID]
	br.mu.Unlock()
	if s == nil {
		return nil, jsonrpc.Errf(-32603, "unknown session: %s", p.SessionID)
	}

	promptBlocks := p.Prompt
	if len(promptBlocks) == 0 {
		promptBlocks = p.Content
	}
	promptText, rerr := promptToText(promptBlocks)
	if rerr != nil {
		return nil, rerr
	}

	br.logger("session/prompt: sid=%s, text=%.40q", shortID(s.acpSID), promptText)

	// P3: 同会话并发 prompt 排队。闸口保证一个会话同时只有一个 prompt 进
	// runTurn; 排队等待期间客户端 ctx 取消 (断连/客户端自己撤销) 会让它退出
	// 队列。抢到闸口后 runTurn 内部的 turns 检查 (T02 时序防线) 依然生效,
	// 只是按闸口的串行约定它不会再触发。
	gate, gerr := br.acquirePromptGate(s.zcodeSID, ctx, string(id))
	if gerr != nil {
		return nil, gerr
	}
	defer br.releasePromptGate(s.zcodeSID, gate)
	return br.runTurn(ctx, s, string(id), promptText)
}

// maxQueuedPrompts 是每会话的 prompt 排队深度上限 (P3)。排队不是无界缓冲:
// 排队者超过这个数仍回 -32600, 提示客户端等当前 turn 结束。
const maxQueuedPrompts = 8

// acquirePromptGate 拿到会话的 turn 闸口; 拿不到就排队等, 等到或取消。
// 返回的 channel 持有一个令牌, releasePromptGate 归还。
func (br *Bridge) acquirePromptGate(zsid string, ctx context.Context, promptID string) (chan struct{}, *jsonrpc.Error) {
	br.mu.Lock()
	gate := br.promptGates[zsid]
	if gate == nil {
		gate = make(chan struct{}, 1)
		br.promptGates[zsid] = gate
	}
	if br.promptQueued[zsid] >= maxQueuedPrompts {
		queued := br.promptQueued[zsid]
		br.mu.Unlock()
		return nil, jsonrpc.Errf(-32600,
			"该会话已有 %d 个 prompt 在排队 (上限 %d), 请等当前 turn 结束或先发 session/cancel",
			queued, maxQueuedPrompts)
	}
	br.promptQueued[zsid]++
	queued := br.promptQueued[zsid]
	br.mu.Unlock()

	br.logger("session/prompt: 会话 %s 的 turn 闸口被占, prompt %s 排队 (第 %d 个等待)",
		shortID(zsid), promptID, queued)

	select {
	case gate <- struct{}{}:
		br.mu.Lock()
		br.promptQueued[zsid]--
		br.mu.Unlock()
		if ctx.Err() != nil {
			<-gate // 归还令牌, 别让闸口空转一轮
			return nil, jsonrpc.Errf(-32603, "session/prompt 被客户端取消")
		}
		return gate, nil
	case <-ctx.Done():
		br.mu.Lock()
		br.promptQueued[zsid]--
		br.mu.Unlock()
		return nil, jsonrpc.Errf(-32603, "session/prompt 在排队等待中被客户端取消")
	}
}

// releasePromptGate 归还闸口令牌, 下一个排队的 prompt 随即获得 turn 资格。
func (br *Bridge) releasePromptGate(zsid string, gate chan struct{}) {
	<-gate
}

// promptToText 把 ACP prompt (ContentBlock[] 或裸 string) 拼成纯文本 (C5:
// 0.16.5 的 session/send content 必须是 string)。resource_link 转路径提示。
func promptToText(raw json.RawMessage) (string, *jsonrpc.Error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", jsonrpc.Errf(-32602, "empty prompt (需 text 或 resource_link)")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "", jsonrpc.Errf(-32602, "empty prompt (需 text 或 resource_link)")
		}
		return s, nil
	}
	var blocks []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		URI   string `json:"uri"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", jsonrpc.Errf(-32602, "prompt 解析失败: %v", err)
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource_link":
			if b.URI != "" {
				if b.Title != "" {
					parts = append(parts, fmt.Sprintf("[相关资源: %s](%s)", b.Title, b.URI))
				} else {
					parts = append(parts, fmt.Sprintf("[相关资源] %s", b.URI))
				}
			}
		}
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		return "", jsonrpc.Errf(-32602, "empty prompt (需 text 或 resource_link)")
	}
	return text, nil
}

// ---- session/cancel (notification) ----

func (br *Bridge) handleSessionCancel(ctx context.Context, params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	br.mu.Lock()
	s := br.sessions[p.SessionID]
	br.mu.Unlock()
	if s == nil {
		return
	}
	br.mu.Lock()
	turn := br.turns[s.zcodeSID]
	br.mu.Unlock()
	if turn != nil {
		// turn 循环自己补发 session/stop (防双发, 上游 P1-2)。
		br.logger("session/cancel: 标记 turn %s 取消", turn.promptID)
		turn.cancelOnce.Do(func() {
			close(turn.cancel)
			// 同时掐断在飞的交互: 循环此刻正卡在等客户端答案的 case body 里,
			// 只关通道的话它要等 Question 超时才醒 (旧实现最坏挂 10 分钟)。
			if turn.abort != nil {
				turn.abort()
			}
		})
		return
	}
	// 空闲: 立即直发; 后端报错忽略 (stop 幂等)。
	br.logger("session/cancel: 无活动 turn, 直发 session/stop")
	if _, err := br.request("session/stop", map[string]any{"sessionId": s.zcodeSID}); err != nil {
		br.logger("session/cancel: session/stop 后端报错 (忽略): %.80s", rpcMessage(err))
	}
}

// stopBackendTurn 向 zcode 转发取消 (session/stop, 0.16 的取消方法)。
// Bounded acknowledgement: on EOF the caller closes the backend immediately
// afterwards, so a fire-and-forget write can be killed before ZCode reads it.
// An acknowledgement is not proof that all descendant processes have stopped;
// the owner must still close/reap its managed process tree.
func (br *Bridge) stopBackendTurn(zcodeSID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := br.backend.Request(ctx, br.nextZcodeID(), "session/stop", map[string]any{"sessionId": zcodeSID}); err != nil {
		br.logger("session/stop not acknowledged: %v", err)
	}
}

// ---- session/list ----

func (br *Bridge) handleSessionList(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		Cwd             string `json:"cwd"`
		IncludeArchived bool   `json:"includeArchived"`
		Limit           int    `json:"limit"`
	}
	_ = json.Unmarshal(params, &p)

	zcParams := map[string]any{"includeArchived": p.IncludeArchived}
	if p.Cwd != "" {
		zcParams["workspace"] = map[string]any{"workspacePath": p.Cwd, "workspaceKey": p.Cwd}
	}
	if p.Limit > 0 {
		zcParams["limit"] = p.Limit
	}
	resp, err := br.request("session/list", zcParams)
	if err != nil {
		return nil, jsonrpc.Errf(-32603, "zcode list failed: %v", rpcMessage(err))
	}
	var sessions []map[string]any
	for _, raw := range zcode.Slice(resp.ResultMap(), "sessions") {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		entry := map[string]any{
			"sessionId": zcode.Str(m, "sessionId"),
			"cwd":       zcode.Str(zcode.Sub(m, "workspace"), "workspacePath"),
		}
		if t := zcode.Str(m, "title"); t != "" {
			entry["title"] = t
		}
		sessions = append(sessions, entry)
	}
	br.logger("session/list → %d sessions", len(sessions))
	return map[string]any{"sessions": sessions}, nil
}

// ---- session/resume ----

func (br *Bridge) handleSessionResume(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	_ = json.Unmarshal(params, &p)
	if p.SessionID == "" {
		return nil, jsonrpc.Errf(-32602, "sessionId required")
	}
	return br.resumeSession(ctx, p.SessionID, p.Cwd, "resume")
}

// ---- session/load (P2: loadSession 能力) ----

// handleSessionLoad 按 id 装载一个已存在的会话, 典型来源是 session/list
// (桥透传 zcode 的 sessionId, 两边同构)。与 session/resume 走同一条 zcode
// 路径 (session/resume), 差别在语义: resume 重连**本进程创建过**的会话,
// load 装载**任何已知 id** 的会话。
//
// mcpServers 在 LoadSessionRequest 的 schema 里是必填, 但 zcode 没有按会话
// 注入 MCP 的通道 (只读用户配置的 mcp.servers, 见 initialize 的拍板注释) —
// 与 initialize 一致: 显式告警, 不伪装支持。additionalDirectories 同理暂不
// 承接 (zcode 会话是单工作区模型), 收到不报错, 由 cwd 主路径承载。
func (br *Bridge) handleSessionLoad(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		SessionID  string          `json:"sessionId"`
		Cwd        string          `json:"cwd"`
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	_ = json.Unmarshal(params, &p)
	if p.SessionID == "" {
		return nil, jsonrpc.Errf(-32602, "sessionId required")
	}
	if m := strings.TrimSpace(string(p.MCPServers)); m != "" && m != "null" && m != "[]" && m != "{}" {
		br.logger("⚠ session/load 收到 mcpServers, 但 zcode 不支持按会话注入 MCP " +
			"(只读 ~/.zcode/cli/config.json 的 mcp.servers) —— 该参数被忽略")
	}
	return br.resumeSession(ctx, p.SessionID, p.Cwd, "load")
}

// resumeSession 是 session/resume 与 session/load 共用的核心: 解析 cwd,
// 发 zcode session/resume, 登记会话映射, 投放 modes/configOptions。
func (br *Bridge) resumeSession(ctx context.Context, sessionID, cwd, via string) (any, *jsonrpc.Error) {
	cwd, cwdSource := br.resolveResumeCwd(sessionID, cwd)
	if !cwdSource {
		// 谁都没告诉过我们这个会话的工作区 —— 此时退回桥自己的工作目录。
		// 与旧行为的差别: 旧代码填字面量 ".", 看着像相对路径, 实际由 zcode
		// 按它自己的进程 cwd 解析 (而那是继承来的桥启动目录), 更难看出来。
		br.logger("⚠ session/%s %s 未给 cwd, 且该会话不是本进程创建的 —— "+
			"退回桥的工作目录 %s", via, shortID(sessionID), cwd)
	}
	resp, err := br.request("session/resume", map[string]any{
		"sessionId": sessionID,
		"workspace": map[string]any{"workspacePath": cwd, "workspaceKey": cwd},
	})
	if err != nil {
		return nil, jsonrpc.Errf(-32603, "zcode resume failed: %v", rpcMessage(err))
	}
	catalog := zcode.ParseModelCatalog(resp.ResultMap())
	br.mu.Lock()
	br.sessions[sessionID] = &session{
		acpSID:   sessionID,
		zcodeSID: sessionID,
		cwd:      cwd,
		catalog:  catalog,
	}
	br.mu.Unlock()
	br.logger("session/%s → %s", via, shortID(sessionID))
	// T04: resume/load 响应同样投放 modes / configOptions。当前模式后端没报就不投
	// (currentModeId 必填, 不能编一个)。
	out := map[string]any{}
	if mode := currentModeFromResult(resp.ResultMap(), ""); mode != "" {
		out["modes"] = modesState(mode)
	}
	if ref, ok := currentModelRef(resp.ResultMap()); ok {
		if co := configOptionsFor(catalog, ref); co != nil {
			out["configOptions"] = co
		}
	}
	return out, nil
}

// resolveResumeCwd 决定 resume 带着哪个工作区去找 zcode。
//
// 为什么不能像以前那样「没给 cwd 就填 '.'」: zcode 拿到的 workspacePath 会被当成
// 会话的工作区根, 而 '.' 是**相对路径** —— 由 zcode 自己的进程 cwd 解析。那个 cwd
// 是 exec.Command 没设 Dir 时继承来的桥启动目录 (internal/zcode/backend.go),
// 于是客户端 resume 时不传 cwd, 会话的工作区就漂到桥的目录去了, 而不是它原本那个。
//
// 优先级: 本次显式给的 → 会话创建时记下的 → 桥自己的工作目录。第二个布尔返回值
// 表示前两级有没有命中 (false = 这个会话不是本进程创建的, 只能猜)。
func (br *Bridge) resolveResumeCwd(sid, explicit string) (string, bool) {
	if explicit != "" {
		return explicit, true
	}
	br.mu.Lock()
	defer br.mu.Unlock()
	if s, ok := br.sessions[sid]; ok && s.cwd != "" {
		return s.cwd, true
	}
	if wd, err := os.Getwd(); err == nil {
		return wd, false
	}
	return ".", false
}

// ---- 工具 ----

func shortID(sid string) string {
	if len(sid) > 20 {
		return sid[:20] + "…"
	}
	return sid
}

// rpcMessage 从 backend 错误里提取可读文案。
func rpcMessage(err error) string {
	if err == nil {
		return ""
	}
	var rpcErr *zcode.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Message
	}
	return err.Error()
}

// ---- 账号 provider 快照 ----

// pushAccountProviderConfig 向 zcode 后端推送订阅 provider 的授权快照。
//
// 尽力而为: 失败只记日志并返回 false —— 建会话必须照常进行, 账号 provider
// 只是暂时不出现在模型下拉里。老 CLI 不认识这个方法时是静默 no-op 而非告警
// (-32601 表示方法不存在, 属于正常版本差异)。
func (br *Bridge) pushAccountProviderConfig() bool {
	payload := zcode.BuildAccountProviderConfig("", func(msg string) { br.logger("%s", msg) })
	if payload == nil {
		return false
	}
	resp, err := br.request(zcode.AccountProviderConfigMethod, map[string]any{
		"revision":                    payload.Revision,
		"basedOnZCodeBuiltinRevision": payload.BasedOnZCodeBuiltinRevisio,
		"providers":                   payload.Providers,
		"states":                      payload.States,
	})
	if err != nil {
		var rpcErr *zcode.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -32601 {
			br.logger("account-provider: 后端不支持 provider/updateAccountConfig (旧版 CLI) —— 已跳过")
		} else {
			br.logger("account-provider: 推送失败: %v", rpcMessage(err))
		}
		return false
	}
	n := 0
	if resp != nil {
		var result struct {
			ProviderCount int    `json:"providerCount"`
			Status        string `json:"status"`
		}
		if m := resp.ResultMap(); m != nil {
			if data, err := json.Marshal(m); err == nil {
				_ = json.Unmarshal(data, &result)
			}
		}
		n = result.ProviderCount
		if n == 0 {
			n = len(payload.Providers)
		}
		if result.Status != "" {
			br.logger("account-provider: 已推送 %d 个 provider (status=%s)", n, result.Status)
		} else {
			br.logger("account-provider: 已推送 %d 个 provider", n)
		}
	}
	return n > 0
}
