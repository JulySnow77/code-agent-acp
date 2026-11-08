// Start Plan 订阅事实的实时来源: zcode.z.ai 的 billing/balance 接口。
//
// 背景 (移植自上游 ZCode packages/services/src/model-provider/
// zaiStartPlanBilling.ts + codingPlanProviderAvailability.ts):
//
// 3.14+ 的 Start Plan 模型清单**不在磁盘上**。内置表 zcode-builtin.json
// 给 account:bigmodel-start-plan 声明的 builtinModelIds 只是上限
// (GLM-5.3-Flash / GLM-5.2 / GLM-5-Turbo); 上游
// account-provider-resolution.ts 是唯一会用运行时结果覆盖该清单的模式:
//
//	if (access.mode === "start-plan") {
//	    builtinModelIds: normalizeModelIds(connection.models)
//	}
//
// 而 connection.models 来自 billing/balance 响应里每个余额桶的
// capabilities 字段 (形如 "model:glm-5.3-flash")。所以「Start Plan
// 到底能用哪些模型」只有问服务端才知道 —— 磁盘上的任何文件都是过期快照。
//
// 接口:
//
//	GET https://zcode.z.ai/api/v1/zcode-plan/billing/balance?app_version=<ver>
//	Authorization: Bearer <zcodejwttoken>
//
// 必须带 NodeApiClient 注入的那组来源头 (sourceHeaders.ts +
// nodeApiClient.ts:withZCodeEndpointHeaders), 否则后端返回
// 400 code=3001 "parameter error" —— 凭证是对的, 是请求指纹不对。
package zcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// StartPlan 的服务端事实。origin 与上游
// shared/src/zcodeEndpoint.ts 的 DEFAULT_ZCODE_ENDPOINT_ORIGIN 一致。
const (
	defaultZcodeEndpointOrigin = "https://zcode.z.ai"
	startPlanBalancePath       = "/api/v1/zcode-plan/billing/balance"
	startPlanRequestTimeout    = 15 * time.Second
)

// appVersionHeader 与 query 参数用同一个值。上游取自构建期注入的
// ZCODE_VERSION; 无头进程没有它, 故回落到 ZCODE_APP_VERSION 环境变量,
// 再回落到 CLI 版本 —— 后端按真实版本判定能力, 固定值会绕过版本策略。
func startPlanAppVersion() string {
	if v := strings.TrimSpace(os.Getenv("ZCODE_APP_VERSION")); v != "" {
		return v
	}
	return "0.16.9"
}

// startPlanSourceHeaders 复现上游 buildZCodeSourceHeadersFromContext。
// 少任何一个都会拿到 3001; X-Device-Mid 是可选项 (缺失时上游直接不发)。
func startPlanSourceHeaders(origin, jwt string) map[string]string {
	h := map[string]string{
		"User-Agent":          "ZCode/" + startPlanAppVersion(),
		"HTTP-Referer":        origin,
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": startPlanAppVersion(),
		"X-Platform":          runtime.GOOS + "-" + runtime.GOARCH,
		"Authorization":       "Bearer " + jwt,
	}
	if v, err := os.ReadFile(filepath.Join(zcodeV2Dir(""), "telemetry-state.json")); err == nil {
		var state struct {
			DeviceMid string `json:"deviceMid"`
		}
		if json.Unmarshal(v, &state) == nil && state.DeviceMid != "" {
			h["X-Device-Mid"] = state.DeviceMid
		}
	}
	return h
}

// startPlanBalanceEnvelope 是 billing/balance 的响应形状 (只取用得到的字段)。
type startPlanBalanceEnvelope struct {
	Code any    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		Plans []struct {
			PlanID string `json:"plan_id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			EndsAt any    `json:"ends_at"`
		} `json:"plans"`
		Balances []struct {
			ShowName     string   `json:"show_name"`
			Capabilities []string `json:"capabilities"`
		} `json:"balances"`
	} `json:"data"`
}

// startPlanIdentity 判定一条 plan 是否是 Start Plan。实测服务端对同一
// 产品返回 plan_id="zcode-v3-start-plan-trust-1007" 与
// name="ZCode Trust Build", 只认字面 "start plan" 会漏判。
var startPlanIdentity = regexp.MustCompile(`(?i)start[- ]plan`)

// hasActiveStartPlan 报告是否存在生效中的 Start Plan。
func hasActiveStartPlan(payload *startPlanBalanceEnvelope) bool {
	if payload == nil || payload.Data == nil {
		return false
	}
	for _, p := range payload.Data.Plans {
		if strings.EqualFold(strings.TrimSpace(p.Status), "active") && startPlanIdentity.MatchString(p.PlanID+" "+p.Name) {
			return true
		}
	}
	return false
}

// officialGLMModelIDs 是上游 shared/src/official-glm-model-id.ts 的
// canonicalIds 原样拷贝。服务端下发的是小写 ("model:glm-5.3-flash"),
// 而 ZCode 注册表与请求里用的是规范大小写, 两边靠这张表对齐。
var officialGLMModelIDs = []string{
	"GLM-5.3", "GLM-5.3-Flash", "GLM-5V-Turbo", "GLM-5.2", "GLM-5.1",
	"GLM-5.1-Highspeed", "GLM-5", "GLM-5-Turbo", "GLM-4.7", "GLM-4.7-FlashX",
	"GLM-4.7-Flash", "GLM-4.6", "GLM-4.5-Air", "GLM-4.5", "GLM-4.6V",
	"GLM-4.6V-Flash", "GLM-4.6V-FlashX", "GLM-4.1V-Thinking-FlashX",
	"GLM-4.1V-Thinking-Flash", "GLM-4-FlashX-250414", "GLM-4-Flash-250414",
	"GLM-4V-Flash",
}

var officialGLMByLower = func() map[string]string {
	m := make(map[string]string, len(officialGLMModelIDs))
	for _, id := range officialGLMModelIDs {
		m[strings.ToLower(id)] = id
	}
	return m
}()

// normalizeOfficialGLMModelId 与上游同名函数一致: 命中名单则返回规范
// 大小写, 未命中原样返回 (不猜、不裁剪)。
func normalizeOfficialGLMModelId(id string) string {
	if canonical, ok := officialGLMByLower[strings.ToLower(id)]; ok {
		return canonical
	}
	return id
}

// startPlanModelIDs 从余额桶的 capabilities 里抽模型, 复现上游
// resolveZaiStartPlanBalanceModelIds: 优先 "model:<id>" 前缀, 没有就退回
// show_name, 再统一归一化大小写并去重。
func startPlanModelIDs(payload *startPlanBalanceEnvelope) []string {
	if payload == nil || payload.Data == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range payload.Data.Balances {
		candidates := make([]string, 0, len(b.Capabilities))
		for _, cap := range b.Capabilities {
			if after, ok := strings.CutPrefix(strings.TrimSpace(cap), "model:"); ok {
				if after = strings.TrimSpace(after); after != "" {
					candidates = append(candidates, after)
				}
			}
		}
		if len(candidates) == 0 && strings.TrimSpace(b.ShowName) != "" {
			candidates = append(candidates, strings.TrimSpace(b.ShowName))
		}
		for _, c := range candidates {
			id := normalizeOfficialGLMModelId(c)
			key := strings.ToLower(id)
			if id == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, id)
		}
	}
	return out
}

// StartPlanState 是一次 Start Plan 查询的结论。
type StartPlanState struct {
	// Entitled 报告账号当前是否有生效中的 Start Plan。
	Entitled bool
	// Models 是服务端下发的、该账号实际可用的模型 (规范大小写)。
	Models []string
	// Detail 是用于诊断的可读原因, 例如 "未登录" / "无 Start Plan 权益"。
	Detail string
}

// startPlanCache 让一次桥进程内的重复 session/new 不必反复打网络。
// 缓存期与 TTL 由 startPlanTTL 决定。
type startPlanCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	expires time.Time
	state   StartPlanState
	fetched bool
}

var startPlanState = &startPlanCache{ttl: 2 * time.Minute}

// FetchStartPlanModels 查询 Start Plan 订阅事实。
//
// 失败不致命: 返回 Entitled=false 且 Detail 写明原因, 调用方据此不发布
// Start Plan 模型 —— 与「没有这个套餐」的表现一致, 但日志能区分二者。
func FetchStartPlanModels(credsPath string, warn func(string)) StartPlanState {
	startPlanState.mu.Lock()
	if startPlanState.fetched && time.Now().Before(startPlanState.expires) {
		state := startPlanState.state
		startPlanState.mu.Unlock()
		return state
	}
	startPlanState.mu.Unlock()

	state := fetchStartPlanModels(credsPath, warn)

	startPlanState.mu.Lock()
	startPlanState.state, startPlanState.fetched, startPlanState.expires = state, true, time.Now().Add(startPlanState.ttl)
	startPlanState.mu.Unlock()
	return state
}

func fetchStartPlanModels(credsPath string, warn func(string)) StartPlanState {
	jwt, err := LoadZcodeJWTToken(credsPath)
	if err != nil || jwt == "" {
		// 没有 zcodejwttoken 就无法问服务端 —— 与桌面未登录同义。
		if warn != nil && err != nil {
			warn("start-plan: 读取 zcodejwttoken 失败: " + err.Error())
		}
		return StartPlanState{Detail: "no zcode jwt"}
	}

	origin := defaultZcodeEndpointOrigin
	if v := strings.TrimSpace(os.Getenv("ZCODE_ENDPOINT_ORIGIN")); v != "" {
		origin = strings.TrimSuffix(v, "/")
	} else if v := strings.TrimSpace(os.Getenv("ZCODE_BASE_URL")); v != "" {
		origin = strings.TrimSuffix(v, "/")
	}

	endpoint := origin + startPlanBalancePath + "?app_version=" + url.QueryEscape(startPlanAppVersion())
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return StartPlanState{Detail: "request build failed: " + err.Error()}
	}
	for k, v := range startPlanSourceHeaders(origin, jwt) {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: startPlanRequestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		if warn != nil {
			warn("start-plan: billing/balance 请求失败: " + err.Error())
		}
		return StartPlanState{Detail: "balance request failed"}
	}
	defer resp.Body.Close()

	var payload startPlanBalanceEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return StartPlanState{Detail: "balance decode failed"}
	}
	if resp.StatusCode != http.StatusOK {
		return StartPlanState{Detail: fmt.Sprintf("balance http %d code=%v msg=%s", resp.StatusCode, payload.Code, payload.Msg)}
	}
	if !hasActiveStartPlan(&payload) {
		return StartPlanState{Detail: "no active start plan"}
	}
	models := startPlanModelIDs(&payload)
	if len(models) == 0 {
		return StartPlanState{Entitled: true, Detail: "active start plan without model buckets"}
	}
	return StartPlanState{Entitled: true, Models: models}
}
