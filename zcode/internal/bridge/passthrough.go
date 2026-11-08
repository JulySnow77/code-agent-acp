package bridge

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/compat"
	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// passthroughMethods 是白名单透传的扩展方法 (上游 handle_acp 的非 baseline 方法)。
//
// 透传层不硬编码「哪个版本删了哪个方法」: 同号构建会漂移 (上游 2d449ff 实测
// App 3.12.3 构建删 workspace/* 7/8 与 updateRuntimeModelConfig), 一律以后端
// -32601 为准 → compat.RemovedCapabilityMessage (规则 passthrough-32601-removed)。
var passthroughMethods = []string{
	"session/fork",
	"session/rewind",
	"session/rewindCascade",
	"session/goal",
	"session/compact",
	"session/steer",
	"session/setThoughtLevel",
	"session/updateRuntimeModelConfig",
	"session/cancelBackgroundTask",
	"session/setMode",
	"workspace/readState",
	"workspace/generateText",
	"workspace/setDefaultModel",
	"workspace/setDefaultMode",
	"workspace/setDefaultThoughtLevel",
	"workspace/upsertModelProvider",
	"workspace/removeModelProvider",
	"workspace/updateProviderRegistry",
}

// aliasPrefix 是扩展方法的规范名前缀 (D2 双名字: 旧名与 _zcode/ 前缀都接受,
// 现有客户端 omnigent 等用旧名, 新客户端推荐规范名)。
const aliasPrefix = "_zcode/"

// passthroughRequired 是透传方法的必填参数 (对齐上游各 handler 的本地校验;
// 缺参在桥侧就报 -32602, 不浪费一次后端往返)。fork/rewind/goal 等不带本地
// 校验 (target 缺省合法); enhance 族在 0.16 可能已删除, 缺参报能力缺失。
var passthroughRequired = map[string][]string{
	"session/setThoughtLevel":          {"thoughtLevel"},
	"session/updateRuntimeModelConfig": {"runtimeModel"},
	"session/cancelBackgroundTask":     {"taskId"},
	"session/setMode":                  {"mode"},
	"workspace/generateText":           {"modelRef", "prompt"},
	"workspace/setDefaultModel":        {"model"},
	"workspace/setDefaultMode":         {"mode"},
	"workspace/setDefaultThoughtLevel": {"thoughtLevel"},
	"workspace/upsertModelProvider":    {"provider"},
	"workspace/removeModelProvider":    {"providerId"},
	"workspace/updateProviderRegistry": {"registry"},
}

// registerPassthrough 注册全部透传方法 (双名字)。
func (br *Bridge) registerPassthrough(conn *jsonrpc.Conn) {
	for _, m := range passthroughMethods {
		h := br.passthroughHandler(m)
		conn.Handle(m, h)
		conn.Handle(aliasPrefix+m, h)
	}
	// setModel 走带目录解析的适配器 (C4), 同样双名字。
	h := br.handleSetModel
	conn.Handle("session/setModel", h)
	conn.Handle(aliasPrefix+"session/setModel", h)

	br.registerEnhance(conn)
}

// passthroughHandler 生成一个方法的透传 handler:
// sessionId 映射 → compat 改写 → 错误映射 → result 原样返回。
func (br *Bridge) passthroughHandler(method string) jsonrpc.Handler {
	return func(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
		var p map[string]any
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, jsonrpc.Errf(-32602, "%s params 解析失败: %v", method, err)
			}
		}
		if p == nil {
			p = map[string]any{}
		}
		for _, req := range passthroughRequired[method] {
			if p[req] == nil {
				return nil, jsonrpc.Errf(-32602, "%s 需要 %s", shortMethodName(method), req)
			}
		}
		p = br.mapSessionID(p)
		// workspace 族: 解析并注入 workspace 选择器 (上游 _resolve_workspace)。
		if strings.HasPrefix(method, "workspace/") {
			p["workspace"] = br.resolveWorkspace(p)
		}
		if method == "workspace/generateText" {
			if p["querySource"] == nil {
				p["querySource"] = "bridge"
			}
		}

		resp, err := br.request(method, p)
		if err != nil {
			return nil, passthroughError(method, err, false)
		}
		result := resp.ResultMap()

		// fork 会创建新会话: 注册映射, 让后续 prompt 能用新 sid。
		if method == "session/fork" {
			if newSID := zcode.Str(result, "sessionId"); newSID != "" {
				br.mu.Lock()
				br.sessions[newSID] = &session{acpSID: newSID, zcodeSID: newSID}
				br.mu.Unlock()
				br.logger("session/fork → %s", shortID(newSID))
			}
		}
		// goal set/replace 启动内部 AI turn: 等锁释放, 否则后续 show/compact
		// 报 "prompt is running" (上游 _wait_for_turn_idle)。
		if method == "session/goal" {
			action := zcode.Str(p, "action")
			if action == "" {
				action = "show"
			}
			if action == "set" || action == "replace" {
				if !br.waitTurnIdle(p["sessionId"]) {
					br.logger("⚠ goal %s 后内部 turn 锁等待超时, 后续 show/compact 可能暂不可用", action)
				}
			}
		}
		return result, nil
	}
}

// mapSessionID 把 params.sessionId 从 ACP sid 换成 zcode sid (查不到就原样
// 透传 —— resume/fork 出来的会话两边 id 本就一致)。
func (br *Bridge) mapSessionID(p map[string]any) map[string]any {
	sid, _ := p["sessionId"].(string)
	if sid == "" {
		return p
	}
	br.mu.Lock()
	s := br.sessions[sid]
	br.mu.Unlock()
	if s != nil && s.zcodeSID != sid {
		out := make(map[string]any, len(p))
		for k, v := range p {
			out[k] = v
		}
		out["sessionId"] = s.zcodeSID
		return out
	}
	return p
}

// passthroughError 是透传方法的统一错误出口 (上游 _passthrough_error):
//
//   - 后端 -32601 → 当前构建已移除该能力 (code 保持 -32601, 客户端据此判版本)
//   - 其他 → -32603 "zcode <短名> failed: <后端原文>"
func passthroughError(method string, err error, redact bool) *jsonrpc.Error {
	var rpcErr *zcode.RPCError
	if ok := asErr(err, &rpcErr); ok && compat.IsMethodNotFound(rpcErr.Code) {
		return jsonrpc.Errf(-32601, "%s", compat.RemovedCapabilityMessage(method))
	}
	msg := ""
	if ok := asErr(err, &rpcErr); ok {
		msg = rpcErr.Message
	} else {
		msg = err.Error()
	}
	if redact {
		msg = redactSecret(msg)
	}
	return jsonrpc.Errf(-32603, "zcode %s failed: %s", shortMethodName(method), msg)
}

// shortMethodName: 去掉首段命名空间后路径的 "/" 换空格
// (session/fork → fork; prompt/enhance/start → enhance start; 与上游文案一致)。
func shortMethodName(method string) string {
	short := method
	if i := strings.Index(method, "/"); i >= 0 {
		short = method[i+1:]
	}
	return strings.ReplaceAll(short, "/", " ")
}

func asErr(err error, target **zcode.RPCError) bool {
	if e, ok := err.(*zcode.RPCError); ok {
		*target = e
		return true
	}
	return false
}

// redactSecret 对错误文案里的长 secret 做最小脱敏 (provider/registry 族方法的
// 后端原文可能回显 apiKey; 上游 P2-3)。
func redactSecret(s string) string {
	for _, marker := range []string{"sk-", "apiKey", "apiKey="} {
		if i := strings.Index(s, marker); i >= 0 {
			return s[:i] + marker + "***"
		}
	}
	return s
}

// handleSetModel 是 ACP session/setModel 的适配器 (C4): 接受旧 {modelId},
// 由会话目录解析 providerId 与缺省 reasoningLevel。
func (br *Bridge) handleSetModel(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		SessionID    string `json:"sessionId"`
		ModelID      string `json:"modelId"`
		ReasoningLvl string `json:"reasoningLevel"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ModelID == "" {
		return nil, jsonrpc.Errf(-32602, "setModel 需要 modelId")
	}
	br.mu.Lock()
	s := br.sessions[p.SessionID]
	br.mu.Unlock()
	if s == nil {
		return nil, jsonrpc.Errf(-32603, "unknown session: %s", p.SessionID)
	}
	spec := p.ModelID
	zcParams := map[string]any{"sessionId": s.zcodeSID, "modelId": spec}
	if p.ReasoningLvl != "" {
		zcParams["reasoningLevel"] = p.ReasoningLvl
	}
	rewriter := br.rewriter
	rewriter.Models = s.catalog
	rewritten, timeout, err := rewriter.Rewrite("session/setModel", zcParams)
	if err != nil {
		return nil, jsonrpc.Errf(-32602, "%v", err)
	}
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := br.backend.Request(ctx2, br.nextZcodeID(), "session/setModel", rewritten)
	if err != nil {
		return nil, passthroughError("session/setModel", err, false)
	}
	// setModel 响应可能带新 settings → 更新目录缓存。
	if fresh := zcode.ParseModelCatalog(resp.ResultMap()); !fresh.Empty() {
		s.catalog = fresh
	}
	br.logger("session/setModel → %s", spec)
	return resp.ResultMap(), nil
}

// waitTurnIdle 轮询 session/goal show 直到 prompt lock 释放 (goal set/replace
// 的内部 AI turn 耗时 10~45s+, 不等会让后续 show/compact 报 prompt is running)。
func (br *Bridge) waitTurnIdle(zcodeSID any) bool {
	sid, _ := zcodeSID.(string)
	if sid == "" {
		return false
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, err := br.request("session/goal", map[string]any{
			"sessionId": sid, "action": "show",
		})
		if err == nil || !strings.Contains(strings.ToLower(rpcMessage(err)), "prompt is running") {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// resolveWorkspace 从 params 解析 zcode workspace 选择器 (上游 _resolve_workspace)。
func (br *Bridge) resolveWorkspace(p map[string]any) map[string]any {
	if ws, ok := p["workspace"].(map[string]any); ok {
		if path, _ := ws["workspacePath"].(string); path != "" {
			return ws
		}
	}
	path := firstNonEmpty(
		stringField(p, "workspacePath"),
		stringField(p, "cwd"),
	)
	if path == "" {
		if wd, err := os.Getwd(); err == nil {
			path = wd
		} else {
			path = "."
		}
	}
	return map[string]any{"workspacePath": path, "workspaceKey": path}
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
