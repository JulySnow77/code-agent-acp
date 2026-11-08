// zcode 账号 provider 的鉴权应答。
//
// 背景: 对 zhipu-account provider (订阅型), zcode 后端在每次模型请求之前都
// 会向宿主发 interaction/requestProviderRuntimeHeaders, 要一把 provider
// runtime 的 apiKey (`zcode.cjs` 的 refreshRuntimeHeadersBeforeAttempt)。
// 拿不到就直接抛 ModelRequestAuthMissing —— 实测文案是 "Provider request
// auth was not returned before model request attempt"。
//
// 谁回答: 桌面 App 会 (它算得出订阅状态), 无头桥接原本不会 ——
// declineServerRequest 把所有未知反向请求一律回 -32601, 于是订阅模型的
// turn 必然以鉴权失败告终。这个 handler 把该请求接过来应答。
//
// 协议 (取自 zcode 协议 schema, 不要自创字段名):
//
//	请求 params: {requestId, sessionId, turnId?, workspace, modelSelection,
//	             providerId, accountAccess?, reason}
//	  reason:     "model-request" | "captcha-retry"
//	应答 result:  {headersApplied: true, requestAuth: {apiKey}}
//	  或          {headersApplied: false, errorMessage}
//
// accountAccess 带出订阅种类, 但不作为可信输入: 它由后端给出, 而我们要回答
// 的问题只有一个 —— 这台机器上有没有这把 key。用 zcode.CodingPlanAPIKeyFor
// 判定, 它只服务 individual coding plan (start-plan 需阿里云验证码, 保持
// 拒绝, 上游 issue #123; team/off-peak 的 key 是存储加密的, 读不到)。
package bridge

import (
	"encoding/json"
	"strings"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// providerRuntimeHeadersMethod 是账号 provider 的鉴权询问方法名。
const providerRuntimeHeadersMethod = "interaction/requestProviderRuntimeHeaders"

// providerRuntimeHeadersRequest 是 zcode 发来的鉴权询问。
type providerRuntimeHeadersRequest struct {
	ProviderID    string `json:"providerId"`
	ModelID       string `json:"modelId"`
	Reason        string `json:"reason"`
	AccountAccess struct {
		Type        string `json:"type"`
		AccountType string `json:"accountType"`
		Mode        string `json:"mode"`
	} `json:"accountAccess"`
}

// providerAuthDecision 是对一次鉴权询问的判定结果: 有 key 就给 key, 没有就
// 给一句能让人看懂为什么没有的话。
type providerAuthDecision struct {
	APIKey string
	Why    string
}

// decideProviderAuth 判定这次询问该不该给凭证。
//
// accountAccess 由后端给出, 不作为可信输入 —— 唯一的问题是「这台机器上有没有
// 这把 key」, 交给 zcode.CodingPlanAPIKeyFor 判定 (它只服务 individual coding
// plan)。抽成纯函数是为了能脱离传输层测试。
func decideProviderAuth(providerID, providerMode string) providerAuthDecision {
	if key := zcode.CodingPlanAPIKeyFor(providerID, ""); key != "" {
		return providerAuthDecision{APIKey: key}
	}
	switch {
	case !strings.HasPrefix(providerID, "account:"):
		return providerAuthDecision{Why: "provider is not a subscription (account:) provider"}
	case providerMode == "start-plan":
		return providerAuthDecision{Why: "start plan requires an interactive captcha; the bridge cannot serve it"}
	case providerMode == "team-coding-plan", providerMode == "off-peak":
		return providerAuthDecision{Why: "team/off-peak credentials are store-encrypted and unreadable here"}
	default:
		return providerAuthDecision{Why: "no subscription credential for this provider"}
	}
}

// handleProviderRuntimeHeaders 应答 zcode 的 provider 鉴权询问。
//
// 没有可用 key 时回 {headersApplied:false, errorMessage} 而不是 -32601:
// 这两者对上游不是一回事 —— -32601 表示「没人会这个方法」, 于是 zcode 把它
// 当成宿主缺席并继续走失败路径; 而 headersApplied:false 带上一句原因, 是
// zcode 明确认得的「宿主在, 但拒绝」。
func (br *Bridge) handleProviderRuntimeHeaders(req *zcode.ServerRequest) {
	var p providerRuntimeHeadersRequest
	// Params 已是解出的 map, 直接编码回 JSON 再解进结构体比手写逐字段取更稳
	// (schema 带 reason/accountAccess, 字段增删时不会静默漏读)。
	if req.Params != nil {
		if raw, err := json.Marshal(req.Params); err == nil {
			_ = json.Unmarshal(raw, &p)
		}
	}

	ref := zcode.ModelRef{ProviderID: p.ProviderID, ModelID: p.ModelID}
	d := decideProviderAuth(p.ProviderID, p.AccountAccess.Mode)
	if d.APIKey != "" {
		br.logger("  ← %s provider=%s model=%s reason=%s → 提供订阅凭证",
			providerRuntimeHeadersMethod, p.ProviderID, p.ModelID, p.Reason)
		_ = br.backend.Respond(req.ID, map[string]any{
			"headersApplied": true,
			"requestAuth":    map[string]any{"apiKey": d.APIKey},
		}, nil)
		return
	}

	br.logger("  ← %s provider=%s model=%s → 无法提供凭证 (%s)",
		providerRuntimeHeadersMethod, p.ProviderID, p.ModelID, d.Why)
	_ = br.backend.Respond(req.ID, map[string]any{
		"headersApplied": false,
		"errorMessage":   d.Why + ": " + ref.String(),
	}, nil)
}
