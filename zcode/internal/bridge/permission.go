package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// handlePermission 把 zcode 的 interaction/requestPermission (工具执行前的
// 权限确认, build/edit 模式) 转成 ACP session/request_permission。
//
// 契约事实 (本地 ZCode 源码基线 29628c9 / 3.14.3):
//   - 请求参数 zcodePermissionRequestParamsSchema: {requestId, sessionId, turnId?,
//     toolCallId, toolName, reason, riskLevel, input, origin?, options[]}。
//   - 选项 zcodePermissionOptionSchema (.strict()):
//     {optionId, kind, name, description?, response}。
//   - 应答 zcodePermissionResponseSchema (.strict()): {decision, reason?,
//     modifiedInput?, permissionUpdates?}, decision ∈
//     {allow, deny, escalate, modify}; zcode 的 broker 就是拿这个 schema 校验
//     桥的回包 (interaction-broker.ts 的 requestClient 第三参)。
//
// 因此桥的回包是**选中项 response 原文** (permissionUpdates 一并带回去, 不能丢),
// 而不是旧实现的 {action, optionId} —— 后者既不是合法 PermissionResponse,
// 也丢掉了持久规则。桥不合成允许决策: 取消/超时/未知 ID/无有效选项一律回合法 deny。
func (br *Bridge) handlePermission(ctx context.Context, acpSID string, req *zcode.ServerRequest) {
	options := parsePermissionOptions(req.Params)
	if len(options) == 0 {
		br.logger("  [perm] zcode 未给出可用选项 (rpcId=%s), 直接回 deny", req.ID)
		br.respondPermissionDeny(req, "zcode offered no valid permission option")
		return
	}
	byID := make(map[string]permOption, len(options))
	acpOptions := make([]map[string]any, 0, len(options))
	optionMeta := make(map[string]any, len(options))
	for _, o := range options {
		byID[o.optionID] = o
		acpOptions = append(acpOptions, map[string]any{
			"optionId": o.optionID,
			"name":     o.name,
			"kind":     o.kind,
		})
		meta := map[string]any{"kind": o.zcodeKind, "decision": zcode.Str(o.response, "decision")}
		if o.desc != "" {
			meta["description"] = o.desc
		}
		optionMeta[o.optionID] = meta
	}

	// 超时上限由 askClient 统一施加 (br.timeouts.Question + ctx 取消)。
	var reply permOutcome
	rerr := br.askClient(ctx, map[string]any{
		"sessionId": acpSID,
		"toolCall": map[string]any{
			"toolCallId": firstNonEmpty(
				zcode.Str(req.Params, "toolCallId"),
				zcode.Str(req.Params, "requestId"),
				"permission",
			),
			// toolName 是模型可影响的展示文本 (P0: 截断 + 去控制字符)。
			"title": firstNonEmpty(sanitizeDisplay(zcode.Str(req.Params, "toolName"), maxOptionLabelLen), "Tool permission"),
			"name":  sanitizeDisplay(zcode.Str(req.Params, "toolName"), maxOptionLabelLen),
			"kind":  permissionToolKind(zcode.Str(req.Params, "toolName")),
			// ACP 的 rawInput 是 object; zcode 的 input 是 unknown, 标量时包一层,
			// 原值在下方 _meta.zcode.input 里原样保留。
			"rawInput": permissionRawInput(req.Params["input"]),
		},
		"options": acpOptions,
		"_meta":   map[string]any{"zcode": permissionRequestMeta(req.Params, optionMeta)},
	}, &reply)

	if rerr != nil {
		// 取消与超时对 zcode 都是「没有拿到授权」, 但文案要能区分: 取消是用户
		// 主动终止 turn, 超时是客户端没响应。两者都回合法 deny。
		reason := "no answer from the ACP client before the permission timeout"
		if errors.Is(ctx.Err(), context.Canceled) {
			reason = "the turn was cancelled while waiting for the permission answer"
		}
		br.logger("  [perm] 等待客户端应答失败 (%v), 回 deny: %s", rerr, reason)
		br.respondPermissionDeny(req, reason)
		return
	}
	if reply.Outcome.Outcome != "selected" {
		br.logger("  [perm] 客户端 outcome=%q, 回 deny", reply.Outcome.Outcome)
		br.respondPermissionDeny(req, "the user cancelled the permission request in the ACP client")
		return
	}
	chosen, ok := byID[reply.Outcome.OptionID]
	if !ok {
		br.logger("  [perm] 客户端返回未知 optionId=%q, 回 deny", reply.Outcome.OptionID)
		br.respondPermissionDeny(req,
			fmt.Sprintf("the ACP client returned an unknown option id %q", reply.Outcome.OptionID))
		return
	}
	br.logger("  [perm] 用户选择 %s (%s/%s) → decision=%s",
		chosen.optionID, chosen.zcodeKind, chosen.kind, zcode.Str(chosen.response, "decision"))
	_ = br.backend.Respond(req.ID, chosen.response, nil)
}

// permissionToolKind 把 zcode 工具名映到 ACP ToolKind 闭集 (schema.json $defs.
// ToolKind: read/edit/delete/move/search/execute/think/fetch/switch_mode/other)。
//
// 改造 P1: 权限请求的 kind 不再恒为 "other" —— kind 是客户端画图标/归类的依据,
// 恒 other 让「读文件」和「跑命令」在授权弹窗里长得一样, 用户无法按风险分级。
// 复用 toolKindMap (translate.go); 认不出的工具回 "other" —— 权限场景下「未知」
// 比瞎猜安全: 宁可图标朴素, 不让用户低估风险 (注意与 tool_call 展示翻译的
// 兜底 "execute" 不同, 那是渲染, 这是授权)。
func permissionToolKind(toolName string) string {
	if k := toolKindMap[toolName]; k != "" {
		return k
	}
	return "other"
}

// permOption 是本次请求里一个通过校验的 zcode 权限选项: 展示给 ACP 客户端的
// optionId/kind/name, 以及必须原样回传给 zcode 的 response。
type permOption struct {
	optionID  string
	kind      string // ACP PermissionOptionKind (闭集)
	name      string
	desc      string
	zcodeKind string
	response  map[string]any
}

// parsePermissionOptions 解析并校验 zcode 的选项列表。
//
// 只保留能安全表达成 ACP 选项的项: optionId 非空、response 合法 (strict)、
// 且 decision 是 ACP 能表示的 allow/deny。escalate/modify 在 ACP 的 kind 闭集里
// 没有对应槽位, 展示出来会让用户以为点了「拒绝」而实际是别的决策 —— 不投放。
func parsePermissionOptions(params map[string]any) []permOption {
	var out []permOption
	seen := map[string]bool{}
	for _, raw := range zcode.Slice(params, "options") {
		o, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := zcode.Str(o, "optionId")
		if id == "" || seen[id] {
			continue
		}
		response, ok := validPermissionResponse(o["response"])
		if !ok {
			continue
		}
		switch zcode.Str(response, "decision") {
		case "allow", "deny":
		default:
			continue
		}
		seen[id] = true
		out = append(out, permOption{
			optionID:  id,
			kind:      acpPermissionKind(zcode.Str(o, "kind"), response),
			name:      firstNonEmpty(sanitizeDisplay(zcode.Str(o, "name"), maxOptionLabelLen), id),
			desc:      sanitizeDisplay(zcode.Str(o, "description"), maxQuestionLen),
			zcodeKind: zcode.Str(o, "kind"),
			response:  response,
		})
	}
	return out
}

// acpPermissionKind 把 zcode 的选项 kind 映到 ACP PermissionOptionKind 闭集
// {allow_once, allow_always, reject_once, reject_always}。
//
// allow_session 在 ACP 里没有槽位, 归入 allow_always —— 与 zcode 自己 v4 投影的
// 既有做法一致 (product-projection: 「会话免确认的 kind 映到闭集里的
// allowAlways」)。真实作用域由 zcode 的 name/description 承载, 桥不改 response,
// 客户端按 kind 画的图标不会比实际授权更宽。
//
// 认不出的 kind (含 v4 的 "custom") 退回按 response 推导: 授权类看有没有
// permissionUpdates (持久规则), 拒绝类看有没有 deny 规则。
func acpPermissionKind(zcodeKind string, response map[string]any) string {
	switch normalizeKind(zcodeKind) {
	case "allowonce":
		return "allow_once"
	case "allowalways", "allowsession":
		return "allow_always"
	case "deny", "reject", "rejectonce":
		return "reject_once"
	case "rejectalways":
		return "reject_always"
	}
	if zcode.Str(response, "decision") == "allow" {
		if hasPermissionUpdates(response) {
			return "allow_always"
		}
		return "allow_once"
	}
	if hasDenyRule(response) {
		return "reject_always"
	}
	return "reject_once"
}

// normalizeKind 抹平大小写与分隔差异: zcode v3 用 allow_once, v4 投影用 allowOnce。
func normalizeKind(kind string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(kind) {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// validPermissionResponse 按 zcodePermissionResponseSchema (.strict()) 校验应答体:
// 桥回给 zcode 的东西必须是 zcode 自己会接受的形状, 否则这次工具调用会挂在
// 一个永远不来的应答上。null 视作缺省 (zod .optional() 不接受 null, 但序列化
// 不会产出 null, 按缺省处理更宽容)。
func validPermissionResponse(raw any) (map[string]any, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	switch zcode.Str(m, "decision") {
	case "allow", "deny", "escalate", "modify":
	default:
		return nil, false
	}
	if v, present := m["reason"]; present && v != nil {
		if _, ok := v.(string); !ok {
			return nil, false
		}
	}
	if v, present := m["permissionUpdates"]; present && v != nil {
		if !validPermissionUpdates(v) {
			return nil, false
		}
	}
	// modifiedInput 是 z.unknown(): 原样透传, 不校验。
	return m, onlyKeys(m, "decision", "reason", "modifiedInput", "permissionUpdates")
}

// validPermissionUpdates 校验 zcodePermissionUpdateSchema (.strict()):
// {type:"addRules", behavior: allow|deny|ask, rules: [{toolName, ruleContent?}]}。
func validPermissionUpdates(raw any) bool {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	for _, item := range items {
		u, ok := item.(map[string]any)
		if !ok || zcode.Str(u, "type") != "addRules" {
			return false
		}
		switch zcode.Str(u, "behavior") {
		case "allow", "deny", "ask":
		default:
			return false
		}
		if !onlyKeys(u, "type", "behavior", "rules") {
			return false
		}
		rules, ok := u["rules"].([]any)
		if !ok || len(rules) == 0 {
			return false
		}
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok || zcode.Str(rule, "toolName") == "" {
				return false
			}
			if v, present := rule["ruleContent"]; present && v != nil {
				if _, ok := v.(string); !ok {
					return false
				}
			}
			if !onlyKeys(rule, "toolName", "ruleContent") {
				return false
			}
		}
	}
	return true
}

func hasPermissionUpdates(response map[string]any) bool {
	v, present := response["permissionUpdates"]
	if !present || v == nil {
		return false
	}
	updates, ok := v.([]any)
	return ok && len(updates) > 0
}

func hasDenyRule(response map[string]any) bool {
	v, present := response["permissionUpdates"]
	if !present || v == nil {
		return false
	}
	updates, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range updates {
		if u, ok := item.(map[string]any); ok && zcode.Str(u, "behavior") == "deny" {
			return true
		}
	}
	return false
}

// onlyKeys 校验 m 只含 allowed 里的键 (null 值当缺省)。
func onlyKeys(m map[string]any, allowed ...string) bool {
	for k, v := range m {
		if v == nil {
			continue
		}
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// permissionDeniedByUserContent 与 zcode 的 PERMISSION_DENIED_BY_USER_CONTENT
// 逐字一致 (apps/zcode-cli/packages/bootstrap/src/permission-options.ts)。
// 该 reason 会作为 provider-visible tool_result.content 回给模型, 必须明确说清
// 「工具没有执行」; 少了这句 core 会退化成通用的 Permission denied, 模型会
// 以为还能重试, 于是绕过式重试。
const permissionDeniedByUserContent = "The user doesn't want to proceed with this tool use. " +
	"The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). " +
	"STOP what you are doing and wait for the user to tell you how to proceed."

// permissionDeniedContent 与 zcode 的 buildPermissionDeniedContent 同义。
func permissionDeniedContent(feedback string) string {
	if t := strings.TrimSpace(feedback); t != "" {
		return permissionDeniedByUserContent + " To tell you how to proceed, the user said:\n" + t
	}
	return permissionDeniedByUserContent
}

// denyPermissionResponse 合成一个合法 deny。zcode 的 response schema 是 strict
// 且 decision 必填, 所以取消/超时/未知 ID/无有效选项都必须显式给出拒绝, 不能沉默。
func denyPermissionResponse(feedback string) map[string]any {
	return map[string]any{"decision": "deny", "reason": permissionDeniedContent(feedback)}
}

// respondPermissionDeny 向 zcode 回一个合法 deny 并记日志。
func (br *Bridge) respondPermissionDeny(req *zcode.ServerRequest, feedback string) {
	_ = br.backend.Respond(req.ID, denyPermissionResponse(feedback), nil)
}

// permissionRawInput 把 zcode 的 input (unknown) 整成 ACP 的 object 形态。
func permissionRawInput(input any) map[string]any {
	switch v := input.(type) {
	case map[string]any:
		return v
	case nil:
		return map[string]any{}
	default:
		return map[string]any{"value": v}
	}
}

// permissionRequestMeta 把 ACP 选项形状装不下的 zcode 事实放进协议保留的
// _meta (riskLevel / reason / origin / 每个选项的真实 kind 与说明)。客户端可以
// 读它把会话级授权显示准确, 不读也不影响主链路。
func permissionRequestMeta(params map[string]any, optionMeta map[string]any) map[string]any {
	meta := map[string]any{
		"requestId": zcode.Str(params, "requestId"),
		"toolName":  zcode.Str(params, "toolName"),
		"riskLevel": zcode.Str(params, "riskLevel"),
		// reason 是模型生成的展示文本, 过 sanitize (P0)。input 是**授权对象本身**,
		// 展示不全 = 盲目授权, 方向相反的风险, 原样保留。
		"reason":     sanitizeDisplay(zcode.Str(params, "reason"), maxQuestionLen),
		"input":      params["input"],
		"optionMeta": optionMeta,
	}
	if origin := zcode.Sub(params, "origin"); origin != nil {
		meta["origin"] = origin
	}
	return meta
}
