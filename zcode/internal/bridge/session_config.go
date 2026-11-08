package bridge

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// T04: 模式与模型配置的标准 ACP 展示 + 专有方法的扩展声明。
//
// 协议事实 (2026-09-27 核对 agentclientprotocol.com/protocol/schema):
//   - NewSessionResponse / ResumeSessionResponse 支持
//     modes: {currentModeId, availableModes: [{id, name, description?}]} 与
//     configOptions: [{id, name, category, currentValue, options: [{id, name, …}]}]
//     (select 类; category 闭集 model|mode|thought|unknown|下划线前缀)。
//   - session/set_mode: {sessionId, modeId}, modeId 必须是投放过的选项;
//     门控就是响应里投放过 modes (AgentCapabilities 无 modes/models 字段)。
//   - session/set_config_option: {sessionId, configId, value, type?};
//     type 缺省按 value_id 处理 ("This is the default when type is absent")。
//   - ACP 扩展方法的规范出口: 方法名加命名空间前缀 + initialize 响应 _meta 声明。
//
// zcode 事实:
//   - 权限模式闭集 build|edit|plan|yolo (ZCode 3.14.3
//     packages/shared/src/zcode-protocol/index.ts zcodeOffPeakPermissionModeSchema)。
//   - session/create 与 session/resume 的 result 携带展示所需的一切:
//     settings.mode.current (当前模式)、settings.model.current
//     {providerId, modelId, options.reasoningLevel} (当前模型)、
//     settings.model.available[] (目录)。不需要新的后端调用。

// zcodeModeList 是投放给客户端的模式表。描述只写已核实的行为
// (packages/core/src/permission/service.ts 的 mode.build.lowRisk /
// mode.build.highRisk / mode.edit.fileEdit 分支与 plan/yolo 语义)。
var zcodeModeList = []map[string]any{
	{
		"id":          "build",
		"name":        "Build",
		"description": "默认模式: 低风险工具自动执行, 高风险工具需确认",
	},
	{
		"id":          "edit",
		"name":        "Edit",
		"description": "工作区内的文件编辑免确认, 其余与 Build 相同",
	},
	{
		"id":          "plan",
		"name":        "Plan",
		"description": "只读规划, 结束时提交计划供用户审批",
	},
	{
		"id":          "yolo",
		"name":        "Yolo",
		"description": "跳过工具权限确认 (无审批)",
	},
}

func validZcodeMode(mode string) bool {
	for _, m := range zcodeModeList {
		if m["id"] == mode {
			return true
		}
	}
	return false
}

// modesState 构造 NewSessionResponse.modes。
func modesState(current string) map[string]any {
	return map[string]any{
		"currentModeId":  current,
		"availableModes": zcodeModeList,
	}
}

// currentModeFromResult 从 session/create 或 session/resume 的 result 里取
// 当前模式。settings.mode.current 是权威来源; session.mode / projection.mode
// 兜底; 都没有时用 fallback (session/new 是请求里的 mode, resume 是空)。
func currentModeFromResult(result map[string]any, fallback string) string {
	if v := zcode.Str(zcode.Sub(zcode.Sub(result, "settings"), "mode"), "current"); v != "" {
		return v
	}
	if v := zcode.Str(zcode.Sub(result, "session"), "mode"); v != "" {
		return v
	}
	if v := zcode.Str(zcode.Sub(result, "projection"), "mode"); v != "" {
		return v
	}
	return fallback
}

// currentModelRef 从 session/create / session/resume / setModel 的 result
// 里取当前模型引用 (settings.model.current)。
func currentModelRef(result map[string]any) (zcode.ModelRef, bool) {
	cur := zcode.Sub(zcode.Sub(zcode.Sub(result, "settings"), "model"), "current")
	if cur == nil {
		return zcode.ModelRef{}, false
	}
	ref := zcode.ModelRef{ProviderID: zcode.Str(cur, "providerId"), ModelID: zcode.Str(cur, "modelId")}
	if ref.ModelID == "" {
		return zcode.ModelRef{}, false
	}
	return ref, true
}

// configOptionsFor 把会话的模型目录投影成 ACP 的一个 select 类配置项
// (category "model")。规范: 属性必须是 id/name 这类原始形状, 值是
// SessionConfigValueId 字符串 —— 这里用 "providerId/modelId"。
// 展示不出来的事实 (contextWindow / reasoning 等级) 放协议保留的 _meta.zcode。
//
// **选项列表永远用调用方给的目录** (session/new 缓存的那份完整目录), 当前值
// 用 ref 单独传入 —— 真机实测 (2026-09-27): setModel 响应里的
// settings.model.available 会被后端收窄到所选模型, 照它派生选项会让客户端
// 再也看不到其它模型。
//
// 返回 nil 表示无法诚实展示 (目录为空或当前模型不在目录里) —— 此时整个
// configOptions 字段省略, 而不是投一个 currentValue 指向不存在选项的半成品。
func configOptionsFor(catalog *zcode.ModelCatalog, curRef zcode.ModelRef) []map[string]any {
	if catalog.Empty() {
		return nil
	}
	var currentID string
	foundCurrent := false
	options := make([]map[string]any, 0, len(catalog.Entries))
	seen := map[string]bool{}
	for _, e := range catalog.Entries {
		if e.Ref.ModelID == "" {
			continue
		}
		id := e.Ref.String()
		if seen[id] {
			continue
		}
		seen[id] = true
		opt := map[string]any{"id": id, "name": e.Label}
		if opt["name"] == "" {
			opt["name"] = e.Ref.ModelID
		}
		if e.ProviderLabel != "" {
			opt["description"] = e.ProviderLabel
		}
		meta := map[string]any{}
		if e.ContextWindow > 0 {
			meta["contextWindow"] = e.ContextWindow
		}
		if len(e.Levels) > 0 {
			meta["reasoningLevels"] = e.Levels
		}
		if e.DefaultLevel != "" {
			meta["defaultReasoningLevel"] = e.DefaultLevel
		}
		if len(meta) > 0 {
			opt["_meta"] = map[string]any{"zcode": meta}
		}
		options = append(options, opt)
		if e.Ref == curRef {
			foundCurrent = true
			currentID = id
		}
	}
	if !foundCurrent || len(options) == 0 {
		return nil
	}
	return []map[string]any{{
		"id":           "model",
		"name":         "Model",
		"category":     "model",
		"currentValue": currentID,
		"options":      options,
	}}
}

// handleSessionSetMode 是标准 ACP session/set_mode ({sessionId, modeId})。
// 收尾后推 current_mode_update —— 客户端不重发请求也拿得到新状态。
//
// 兼容: modeId 缺失时接受旧字段 mode (与 _zcode/session/setMode 的透传参数
// 同形)。未知 mode 报 -32602 并列出可投放的选项 (规范: modeId 必须是
// availableModes 之一, 桥侧拒绝比让 zcode 报 ZodError 更可读)。
func (br *Bridge) handleSessionSetMode(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		SessionID string `json:"sessionId"`
		ModeID    string `json:"modeId"`
		Mode      string `json:"mode"`
	}
	_ = json.Unmarshal(params, &p)
	mode := p.ModeID
	if mode == "" {
		mode = p.Mode
	}
	if mode == "" {
		return nil, jsonrpc.Errf(-32602, "session/set_mode 需要 modeId (可选: build/edit/plan/yolo)")
	}
	if !validZcodeMode(mode) {
		return nil, jsonrpc.Errf(-32602, "未知模式 %q (可选: build/edit/plan/yolo)", mode)
	}
	br.mu.Lock()
	s := br.sessions[p.SessionID]
	br.mu.Unlock()
	if s == nil {
		return nil, jsonrpc.Errf(-32603, "unknown session: %s", p.SessionID)
	}
	if _, err := br.request("session/setMode", map[string]any{"sessionId": s.zcodeSID, "mode": mode}); err != nil {
		return nil, passthroughError("session/setMode", err, false)
	}
	_ = br.conn.Notify("session/update", map[string]any{
		"sessionId": s.acpSID,
		"update":    map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": mode},
	})
	br.logger("session/set_mode → %s", mode)
	return map[string]any{}, nil
}

// handleSessionSetConfigOption 是标准 ACP session/set_config_option。
// 目前只有一个配置项 ("model", select 类): value 是 "providerId/modelId",
// 经会话目录解析后走 setModel 适配器 (缺省 reasoningLevel 由目录注入)。
// 成功后推 config_option_update 并返回刷新后的完整 configOptions (规范如此)。
func (br *Bridge) handleSessionSetConfigOption(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		SessionID string          `json:"sessionId"`
		ConfigID  string          `json:"configId"`
		Type      string          `json:"type"`
		Value     json.RawMessage `json:"value"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ConfigID == "" {
		return nil, jsonrpc.Errf(-32602, "session/set_config_option 需要 configId (支持: model)")
	}
	if p.ConfigID != "model" {
		return nil, jsonrpc.Errf(-32602, "不支持的配置项 %q (支持: model)", p.ConfigID)
	}
	// value_id 是缺省变体; "boolean" 对 select 类无意义, 显式拒绝而不是猜。
	if p.Type == "boolean" {
		return nil, jsonrpc.Errf(-32602, "配置项 model 是 select 类, 不接受 boolean 值")
	}
	var value string
	if err := json.Unmarshal(p.Value, &value); err != nil || value == "" {
		return nil, jsonrpc.Errf(-32602, "session/set_config_option 需要 value (字符串 \"providerId/modelId\")")
	}
	br.mu.Lock()
	s := br.sessions[p.SessionID]
	br.mu.Unlock()
	if s == nil {
		return nil, jsonrpc.Errf(-32603, "unknown session: %s", p.SessionID)
	}
	// setModel 成功后的兜底「当前值」: 目录能解析就解析, 不能就原样拆。
	valueRef, _ := s.catalog.Resolve(value)
	result, err := br.setModel(s, value)
	if err != nil {
		var rpcErr *zcode.RPCError
		if errors.As(err, &rpcErr) {
			return nil, passthroughError("session/setModel", err, false)
		}
		// 目录解析失败 (模型不在目录/歧义): 客户端的 value 非法, -32602。
		return nil, jsonrpc.Errf(-32602, "%v", err)
	}
	fresh := func() []map[string]any {
		// 选项用会话缓存的完整目录 (setModel 响应里的 available 会被收窄);
		// 当前值优先响应里的 settings.model.current, 响应没带就用刚设置的值
		// —— 那是我们刚请求的, 是已知事实。
		ref, ok := currentModelRef(result)
		if !ok {
			ref = valueRef
		}
		return configOptionsFor(s.catalog, ref)
	}()
	update := map[string]any{"sessionUpdate": "config_option_update"}
	if fresh != nil {
		update["configOptions"] = fresh
	}
	_ = br.conn.Notify("session/update", map[string]any{
		"sessionId": s.acpSID,
		"update":    update,
	})
	br.logger("session/set_config_option model → %s", value)
	out := map[string]any{}
	if fresh != nil {
		out["configOptions"] = fresh
	}
	return out, nil
}

// extMethodNames 列出全部专有方法的规范名 (_zcode/ 前缀)。
// initialize 响应的 _meta._zcode.methods 用它声明扩展边界; 裸名 (无前缀)
// 是兼容旧客户端的别名, 见 registerPassthrough 的 aliasPrefix。
func extMethodNames() []string {
	out := make([]string, 0, len(passthroughMethods)+4)
	for _, m := range passthroughMethods {
		out = append(out, aliasPrefix+m)
	}
	return append(out,
		aliasPrefix+"session/setModel",
		aliasPrefix+"prompt/enhance",
		aliasPrefix+"prompt/enhance/start",
		aliasPrefix+"prompt/enhance/cancel",
		// T003 flow-questions v1: daemon 调用的扩展入口 (bridge 本地处理,
		// 不透传 zcode; _multica/flow/{question,receipt} 是桥→daemon 方向,
		// 不在此列)。
		"_multica/flow/answer",
		"_multica/flow/suspend",
	)
}
