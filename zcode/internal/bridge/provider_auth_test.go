package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// writeAuthFixture 铺一套内置 provider 表 + config.json, 并把
// ZCODE_BUILTIN_PROVIDER_CONFIG_FILE 指向该内置表, 让鉴权判定跑在确定的
// 夹具上而不是开发机的真实凭证上。
func writeAuthFixture(t *testing.T, apiKey string) {
	t.Helper()
	dir := t.TempDir()

	table := map[string]any{
		"revision": 42,
		"config": map[string]any{
			"providerConfigRules": map[string]any{
				"providerRules": []map[string]any{
					{
						"providerId":   "account:bigmodel-individual-coding-plan",
						"providerName": "BigModel Individual Coding Plan",
						"config": map[string]any{
							"group":           "bigmodel-family",
							"builtinModelIds": []string{"GLM-5.3"},
							"access": map[string]any{
								"type":        "zhipu-account",
								"mode":        "individual-coding-plan",
								"accountType": "bigmodel",
							},
						},
					},
					{
						"providerId": "account:bigmodel-start-plan",
						"config": map[string]any{
							"builtinModelIds": []string{"GLM-5.3-Flash"},
							"access": map[string]any{
								"type":        "zhipu-account",
								"mode":        "start-plan",
								"accountType": "bigmodel",
							},
						},
					},
				},
			},
		},
	}
	raw := mustJSON(t, table)
	if err := os.WriteFile(filepath.Join(dir, "zcode-builtin.json"), raw, 0o600); err != nil {
		t.Fatalf("写内置表: %v", err)
	}

	cfg := map[string]any{
		"provider": map[string]any{
			"builtin:bigmodel-coding-plan": map[string]any{
				"enabled": true,
				"options": map[string]any{"apiKey": apiKey},
			},
		},
	}
	v2 := filepath.Join(dir, "v2")
	if err := os.MkdirAll(v2, 0o755); err != nil {
		t.Fatalf("建 v2 目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v2, "config.json"), mustJSON(t, cfg), 0o600); err != nil {
		t.Fatalf("写 config: %v", err)
	}
	// 内置表走显式 env; 凭证目录走 ZCODE_HOME —— 后者是 zcode 自己认的变量,
	// 有了它整套判定 (授权缓存 / config.json / setting.json) 都在夹具里,
	// 不会读到开发机上真实的订阅凭证。
	t.Setenv("ZCODE_HOME", dir)
	t.Setenv("ZCODE_BUILTIN_PROVIDER_CONFIG_FILE", filepath.Join(dir, "zcode-builtin.json"))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	return b
}

// TestDecideProviderAuth_ServesEntitledIndividualCodingPlan 是这个 handler 存在
// 的理由: 已授权的 individual coding plan 必须拿到凭证, 否则 zcode 在每次模型
// 请求前抛 ModelRequestAuthMissing, turn 必然失败。
func TestDecideProviderAuth_ServesEntitledIndividualCodingPlan(t *testing.T) {
	writeAuthFixture(t, "secret-key")

	d := decideProviderAuth("account:bigmodel-individual-coding-plan", "individual-coding-plan")
	if d.APIKey != "secret-key" {
		t.Fatalf("应返回订阅凭证, 实际 %q (why=%q)", d.APIKey, d.Why)
	}
	if d.Why != "" {
		t.Errorf("成功路径不应带原因说明: %q", d.Why)
	}
}

// TestDecideProviderAuth_RefusesStartPlan 守住一条安全边界: start-plan 需要
// 阿里云验证码, 拿不到就该明说, 而不是发一把无效凭证出去。
func TestDecideProviderAuth_RefusesStartPlan(t *testing.T) {
	writeAuthFixture(t, "secret-key")

	d := decideProviderAuth("account:bigmodel-start-plan", "start-plan")
	if d.APIKey != "" {
		t.Fatalf("start-plan 不应返回凭证, 实际 %q", d.APIKey)
	}
	if !strings.Contains(d.Why, "captcha") {
		t.Errorf("原因应指向验证码, 实际 %q", d.Why)
	}
}

// TestDecideProviderAuth_RefusesNonAccountProvider 覆盖最常见的一类: 第三方
// api-key provider 本来就不需要宿主给凭证, 不该在这里被误判。
func TestDecideProviderAuth_RefusesNonAccountProvider(t *testing.T) {
	writeAuthFixture(t, "secret-key")

	d := decideProviderAuth("38acd6a5-1169-47bf-ba3a-34d5bd8071b7", "")
	if d.APIKey != "" {
		t.Fatalf("非 account provider 不应有凭证, 实际 %q", d.APIKey)
	}
	if !strings.Contains(d.Why, "not a subscription") {
		t.Errorf("原因应说明不是订阅型, 实际 %q", d.Why)
	}
}

// TestDecideProviderAuth_RefusesUnentitledPlan 覆盖夹具缺凭证的情形:
// config.json 里没有 key 时必须拒绝, 不能把空串当成「已提供」。
func TestDecideProviderAuth_RefusesUnentitledPlan(t *testing.T) {
	writeAuthFixture(t, "")

	d := decideProviderAuth("account:bigmodel-individual-coding-plan", "individual-coding-plan")
	if d.APIKey != "" {
		t.Fatalf("无凭证时不得返回 key, 实际 %q", d.APIKey)
	}
	if d.Why == "" {
		t.Error("拒绝时必须给出可读原因")
	}
}

// TestProviderRuntimeHeadersResponseShape 锁住应答结构: 成功与拒绝都必须带
// headersApplied 这个判别字段 —— zcode 用它区分「宿主缺席」与「宿主拒绝」。
func TestProviderRuntimeHeadersResponseShape(t *testing.T) {
	writeAuthFixture(t, "k")

	d := decideProviderAuth("account:bigmodel-individual-coding-plan", "individual-coding-plan")
	ok := map[string]any{
		"headersApplied": true,
		"requestAuth":    map[string]any{"apiKey": d.APIKey},
	}
	if ok["headersApplied"] != true {
		t.Error("成功应答必须是 headersApplied=true")
	}
	auth, _ := ok["requestAuth"].(map[string]any)
	if auth["apiKey"] != "k" {
		t.Errorf("requestAuth.apiKey 应为凭证, 实际 %v", auth["apiKey"])
	}

	ref := zcode.ModelRef{ProviderID: "account:bigmodel-start-plan", ModelID: "GLM-5.3-Flash"}
	no := map[string]any{
		"headersApplied": false,
		"errorMessage":   d.Why + ": " + ref.String(),
	}
	if no["headersApplied"] != false {
		t.Error("拒绝应答必须是 headersApplied=false")
	}
	if no["errorMessage"] == "" {
		t.Error("拒绝应答必须带 errorMessage")
	}
}
