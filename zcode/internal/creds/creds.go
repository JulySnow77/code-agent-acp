// Package creds 从 ~/.zcode/v2/config.json 动态读取 zcode 凭证并注入子进程环境。
//
// 移植自上游 shared/credentials.py (权威实现), 对应 PORTING_PLAN §10.3 的映射表。
// 为什么需要: 桥是独立进程, 不继承用户 shell 里 export 的 ZCODE_MODEL 等变量,
// 必须自己从配置文件读取后注入 zcode 子进程。
//
// 两条必须保留的行为:
//  1. 显式非空环境变量优先, 空串视为未设置 (避免 ZCODE_MODEL="" 把 config 值清空)。
//  2. baseURL 残留自愈: App 切换 plan 后旧 endpoint 会残留在 env, 若 env 值是
//     config 里「另一个 provider」的官方 endpoint 则改用 config 值; 用户自建
//     endpoint (不在 config 任何 provider 里) 则尊重 env。
package creds

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
)

// DefaultPath 是 zcode 桌面 App 的配置文件。
var DefaultPath = filepath.Join(mustHome(), ".zcode", "v2", "config.json")

// FallbackModel 是 config 里没有 enabled provider / 无 models 时的兜底模型名。
// 随上游 (7903e9e: GLM-5.2 → GLM-5.3)。
const FallbackModel = "GLM-5.3"

// CredKeys 是受凭证注入影响的 env key。
var CredKeys = []string{"ZCODE_MODEL", "ZCODE_BASE_URL", "ANTHROPIC_API_KEY"}

// Load 读取第一个 enabled provider 的 model / baseURL / apiKey。
// 文件不存在属正常 (走纯环境变量), 静默返回空 map; 文件损坏则通过 warn 报警,
// 让用户能区分「没配凭证」与「凭证文件损坏」。
func Load(path string, warn func(string)) map[string]string {
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}
		}
		if warn != nil {
			warn("凭证文件存在但读取失败 (" + path + "): " + err.Error() + "; 回退纯环境变量")
		}
		return map[string]string{}
	}
	var cfg struct {
		Provider map[string]struct {
			Enabled bool           `json:"enabled"`
			Options map[string]any `json:"options"`
			Models  map[string]any `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		if warn != nil {
			warn("凭证文件结构异常 (" + path + "): " + err.Error() + "; 回退纯环境变量")
		}
		return map[string]string{}
	}
	// map 遍历顺序不稳定, 按 provider id 排序保证「第一个 enabled」可复现。
	ids := make([]string, 0, len(cfg.Provider))
	for id := range cfg.Provider {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := cfg.Provider[id]
		if !p.Enabled {
			continue
		}
		model := FallbackModel
		for m := range p.Models {
			model = m
			break
		}
		return map[string]string{
			"ZCODE_MODEL":       model,
			"ZCODE_BASE_URL":    strVal(p.Options, "baseURL"),
			"ANTHROPIC_API_KEY": strVal(p.Options, "apiKey"),
		}
	}
	return map[string]string{}
}

// AllProviderBaseURLs 读 config 里所有 provider 的 baseURL (用于残留检测)。
func AllProviderBaseURLs(path string) []string {
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg struct {
		Provider map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Provider))
	for _, p := range cfg.Provider {
		if bu := strVal(p.Options, "baseURL"); bu != "" {
			out = append(out, bu)
		}
	}
	sort.Strings(out)
	return out
}

// IsStaleEnvBaseURL 判定 env 的 baseURL 是否是 App 注入的残留官方 endpoint。
//
// 精确区分两种「env 与 config 不同」:
//   - 残留: env 的 host 等于 config 里某个 provider 的 host, 但不等于 enabled 那个 → 自愈
//   - 自定义: env 的 host 不在 config 任何 provider 里 (用户自建/代理) → 尊重 env
//
// 用 host 匹配而非精确匹配: App 注入的可能是根域名, config 存的是完整路径。
func IsStaleEnvBaseURL(envBaseURL, configBaseURL string, allConfigURLs []string) bool {
	if envBaseURL == "" || configBaseURL == "" || envBaseURL == configBaseURL {
		return false
	}
	envHost := safeHost(envBaseURL)
	if envHost == "" {
		return false
	}
	inConfig := false
	for _, u := range allConfigURLs {
		if safeHost(u) == envHost {
			inConfig = true
			break
		}
	}
	return inConfig && envHost != safeHost(configBaseURL)
}

func safeHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	if u.Scheme != "" {
		return u.Scheme + "://" + u.Host
	}
	return "https://" + u.Host
}

func strVal(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	v, ok := m[k]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func mustHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
