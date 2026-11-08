// Package compat 是「ZCode 协议漂移」的唯一拦截点。
//
// 设计规则 (PORTING_PLAN §3.2 规则 2): 所有发往 ZCode 的请求都经过
// compat.Rewrite(version, method, params)。版本相关的改写规则是一张表,
// 每条规则带「引入版本 / 对应实录样本 / 上游 commit」。新增规则必须同时新增
// 一份 testdata 样本、一条表驱动测试、一段 docs/zcode-protocol.md 说明。
//
// 重要前提: CLI 版本号不可全信。App 3.12.3 与更早的构建报同一个 0.16.5,
// 而方法集不同 (workspace/* 删 7/8, 上游 2d449ff 实测), 故版本号只用于
// 「引入版本」型规则; 能力是否真存在一律以后端 -32601 为准。
package compat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// Rule 是兼容层规则表的一条记录。
type Rule struct {
	ID       string        // 规则标识
	Since    zcode.Version // 引入版本 (零值表示全版本生效)
	Method   string        // 作用方法 ("*" 表示全部)
	Desc     string        // 行为描述
	Sample   string        // 对应实录样本 (PORTING_PLAN §2.2 的 C 编号或 testdata 路径)
	Upstream string        // 上游来源 commit / 说明
}

// Rules 是初始规则表 (PORTING_PLAN §5.2)。
var Rules = []Rule{
	{
		ID: "provider-config-path", Since: ver(0, 16, 0), Method: "*",
		Desc: "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE 未设时, 按 ZCODE_BIN 真实路径推导 " +
			"<Resources>/config/provider/zcode-builtin.json 并注入",
		Sample:   "C1",
		Upstream: "Go 版主动改进 (上游仍走 symlink 方案, 0.16.5 已失效); 建议回推上游",
	},
	{
		ID: "create-timeout", Method: "session/create",
		Desc:     "建会话超时默认 120s (上游硬编码 15s, 本机 1.2GB 会话库下实测 15~20s 必超时)",
		Sample:   "C2",
		Upstream: "Go 版主动改进; 建议回推上游",
	},
	{
		ID: "setmodel-upgrade", Since: ver(0, 16, 0), Method: "session/setModel",
		Desc: "旧格式 {modelId} → {model:{providerId,modelId,options:{reasoningLevel}}}; " +
			"providerId 由模型目录解析, reasoningLevel 缺省取目录中第一个可用等级",
		Sample:   "C4",
		Upstream: "上游 0.16.1 适配 (832d647) 已部分覆盖; 目录解析为 Go 版新增",
	},
	{
		ID: "send-content-flatten", Method: "session/send",
		Desc:     "ACP prompt 的 content blocks 拼成字符串 (0.16.5 的 content 必须是 string)",
		Sample:   "C5",
		Upstream: "上游已如此",
	},
	{
		ID: "subscribe-required-fields", Since: ver(0, 14, 8), Method: "session/subscribe",
		Desc:     "固定传全 {sessionId, deliveryKind, includeSnapshot, afterSeq}; 缺字段报 ZodError",
		Sample:   "C6",
		Upstream: "上游已如此",
	},
	{
		ID: "unknown-server-request", Method: "*",
		Desc:     "未识别的 server→client 反向调用统一回 -32601 (已知四种各有 handler)",
		Sample:   "C8",
		Upstream: "上游 832d647",
	},
	{
		ID: "unknown-notification", Method: "*",
		Desc:     "未知通知静默丢弃 (不入通用队列 —— 那里无消费者, 只会无界积压)",
		Sample:   "C9",
		Upstream: "上游 832d647",
	},
	{
		ID: "passthrough-32601-removed", Method: "passthrough",
		Desc: "透传/扩展方法后端返 -32601 → 「当前 ZCode 版本已移除该能力」文案 " +
			"(错误码不变)。不硬编码方法清单: 同号构建会漂移",
		Sample:   "docs/recheck-3.12.3.md",
		Upstream: "上游 2d449ff",
	},
	{
		ID: "runtime-preferences-reply", Since: ver(0, 16, 0), Method: "session/requestRuntimePreferences",
		Desc:     "必须应答, 否则 session/create|send 永久挂起",
		Sample:   "C8",
		Upstream: "上游 832d647 (旧版桥无此代码路径, 即死于此)",
	},
}

func ver(major, minor, patch int) zcode.Version {
	return zcode.Version{Major: major, Minor: minor, Patch: patch}
}

// ModelResolver 抽象模型目录查询, 供 setModel 参数升格使用 (规则 C4)。
type ModelResolver interface {
	Resolve(spec string) (zcode.ModelRef, error)
	DefaultReasoningLevel(zcode.ModelRef) string
}

// Rewriter 按版本改写发往 ZCode 的请求参数并给出超时。
type Rewriter struct {
	Version zcode.Version
	Models  ModelResolver // 可为 nil (无目录时 setModel 原样透传)
	Timeout Timeouts
}

// Timeouts 集中所有对 ZCode 请求的超时 (§3.2 规则 2: 超时集中在一处)。
type Timeouts struct {
	Create    time.Duration // session/create, 默认 120s (C2)
	Send      time.Duration // session/send
	Read      time.Duration // session/read
	Messages  time.Duration // session/messages
	Subscribe time.Duration // session/subscribe
	Compact   time.Duration // session/compact
	Enhance   time.Duration // prompt/enhance
	Generate  time.Duration // workspace/generateText
	Default   time.Duration // 其余方法
	Turn      time.Duration // 单个 turn 的总预算 (与上游一致 30m)
	Question  time.Duration // AskUserQuestion 等待上限 (与上游一致 10m)
}

// DefaultTimeouts 返回默认超时; 环境变量可覆盖。
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Create:    120 * time.Second,
		Send:      30 * time.Second,
		Read:      5 * time.Second,
		Messages:  10 * time.Second,
		Subscribe: 30 * time.Second,
		Compact:   30 * time.Second,
		Enhance:   90 * time.Second,
		Generate:  60 * time.Second,
		Default:   15 * time.Second,
		Turn:      30 * time.Minute,
		Question:  10 * time.Minute,
	}
}

// TimeoutsFromEnv 读取环境变量覆盖 (§3.3)。
func TimeoutsFromEnv() Timeouts {
	t := DefaultTimeouts()
	if v := envDur("ZCODE_ACP_CREATE_TIMEOUT"); v > 0 {
		t.Create = v
	}
	if v := envDur("ZCODE_ACP_TURN_TIMEOUT"); v > 0 {
		t.Turn = v
	}
	if v := envDur("ZCODE_ACP_QUESTION_TIMEOUT"); v > 0 {
		t.Question = v
	}
	return t
}

func envDur(key string) time.Duration {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		// 裸数字按秒解释, 与上游的 ZCODE_ACP_TURN_TIMEOUT 语义一致。
		return time.Duration(n * float64(time.Second))
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// For 返回某方法的超时。
func (t Timeouts) For(method string) time.Duration {
	switch method {
	case "session/create":
		return t.Create
	case "session/send":
		return t.Send
	case "session/read":
		return t.Read
	case "session/messages":
		return t.Messages
	case "session/subscribe":
		return t.Subscribe
	case "session/compact":
		return t.Compact
	case "prompt/enhance":
		return t.Enhance
	case "workspace/generateText":
		return t.Generate
	}
	return t.Default
}

// Rewrite 是唯一的请求改写入口。返回改写后的 params 与该方法应使用的超时。
//
// 入参 params 会被浅拷贝后修改, 调用方的 map 不会被就地改动。
func (r Rewriter) Rewrite(method string, params map[string]any) (map[string]any, time.Duration, error) {
	out := make(map[string]any, len(params)+2)
	for k, v := range params {
		out[k] = v
	}
	var err error
	switch method {
	case "session/setModel":
		out, err = r.upgradeSetModel(out)
	case "session/subscribe":
		out = r.fillSubscribe(out)
	}
	return out, r.Timeout.For(method), err
}

// upgradeSetModel 把旧格式 {modelId} 升格为 0.16.5 要求的嵌套结构。
//
// 0.16.5 实测: 传旧的 {modelId} 报 Unrecognized key "modelId"; 部分模型
// reasoningLevel 必填 (Reasoning level is required); 模型不在目录里报
// Provider Registry 中不存在 Model (§2.2 C4)。
func (r Rewriter) upgradeSetModel(params map[string]any) (map[string]any, error) {
	existing, hasNested := params["model"].(map[string]any)
	if hasNested {
		// 已是新格式: 只补缺省的 reasoningLevel (不覆盖调用方显式给的)。
		if r.Models != nil {
			opts, _ := existing["options"].(map[string]any)
			if opts == nil {
				opts = map[string]any{}
			}
			// 注意键不存在时取到的是 nil interface, fmt.Sprint(nil) 是
			// "<nil>" 而非空串 —— 必须区分「没给」与「给了空串」。
			lvl, given := opts["reasoningLevel"]
			if !given || lvl == nil || strings.TrimSpace(fmt.Sprint(lvl)) == "" {
				ref := zcode.ModelRef{
					ProviderID: str(existing, "providerId"),
					ModelID:    str(existing, "modelId"),
				}
				if lvl := r.Models.DefaultReasoningLevel(ref); lvl != "" {
					opts["reasoningLevel"] = lvl
					existing["options"] = opts
				}
			}
		}
		return params, nil
	}
	rawModelID, ok := params["modelId"]
	if !ok {
		return params, nil // 调用方没给模型, 交给 zcode 判定
	}
	spec := strings.TrimSpace(fmt.Sprint(rawModelID))
	delete(params, "modelId")
	ref, err := resolveModel(r.Models, spec)
	if err != nil {
		return params, err
	}
	model := map[string]any{
		"providerId": ref.ProviderID,
		"modelId":    ref.ModelID,
	}
	if r.Models != nil {
		if lvl := r.Models.DefaultReasoningLevel(ref); lvl != "" {
			model["options"] = map[string]any{"reasoningLevel": lvl}
		}
	}
	// 调用方显式指定的 reasoningLevel (ZCODE_ACP_MODEL 的第三段) 优先。
	if lvl, ok := params["reasoningLevel"]; ok {
		delete(params, "reasoningLevel")
		opts, _ := model["options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["reasoningLevel"] = lvl
		model["options"] = opts
	}
	params["model"] = model
	return params, nil
}

func resolveModel(m ModelResolver, spec string) (zcode.ModelRef, error) {
	if m == nil {
		// 无目录: 尽量按 "providerId/modelId" 拆, 拆不出就整体当 modelId。
		if i := strings.Index(spec, "/"); i >= 0 {
			return zcode.ModelRef{ProviderID: spec[:i], ModelID: spec[i+1:]}, nil
		}
		return zcode.ModelRef{ModelID: spec}, nil
	}
	return m.Resolve(spec)
}

// fillSubscribe 补齐 session/subscribe 的必填字段 (缺字段报 ZodError, §2.2 C6)。
func (r Rewriter) fillSubscribe(params map[string]any) map[string]any {
	if _, ok := params["deliveryKind"]; !ok {
		params["deliveryKind"] = "desktop-continuous"
	}
	if _, ok := params["includeSnapshot"]; !ok {
		params["includeSnapshot"] = true
	}
	if _, ok := params["afterSeq"]; !ok {
		params["afterSeq"] = 0
	}
	return params
}

func str(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// RemovedCapabilityMessage 生成透传方法被后端移除时的统一文案 (规则
// passthrough-32601-removed)。文案不硬编码版本号: 「哪个版本移除」是历史事实,
// 对调用方重要的是「当前版本不支持」。
func RemovedCapabilityMessage(zcodeMethod string) string {
	return fmt.Sprintf("当前 ZCode 版本已移除该能力 (%s); 该 ZCode 版本不支持此能力", zcodeMethod)
}

// IsMethodNotFound 判定后端错误是否是「方法不存在」(-32601)。
func IsMethodNotFound(code int) bool { return code == -32601 }
