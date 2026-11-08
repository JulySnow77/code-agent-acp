package zcode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeAccountFixture 在 dir 下铺一套 zcode v2 配置: 内置 provider 表 +
// config.json + (可选) coding-plan-cache.json / setting.json, 并把
// ZCODE_BUILTIN_PROVIDER_CONFIG_FILE 指向该内置表。
//
// 返回内置表路径与 config.json 路径。
func writeAccountFixture(t *testing.T, dir string, table map[string]any, config map[string]any, extra map[string]any) (string, string) {
	t.Helper()
	tablePath := filepath.Join(dir, "zcode-builtin.json")
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatalf("序列化内置表: %v", err)
	}
	if err := os.WriteFile(tablePath, raw, 0o600); err != nil {
		t.Fatalf("写内置表: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw, err = json.Marshal(config)
	if err != nil {
		t.Fatalf("序列化 config: %v", err)
	}
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("写 config: %v", err)
	}
	for name, body := range extra {
		p := filepath.Join(dir, name)
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化 %s: %v", name, err)
		}
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	t.Setenv(builtinProviderConfigEnv, tablePath)
	return tablePath, cfgPath
}

// accountFixtureTable 是最小可用的内置表: 一个有权、一个无权的 bigmodel plan。
func accountFixtureTable() map[string]any {
	return map[string]any{
		"revision": 42,
		"config": map[string]any{
			"providerConfigRules": map[string]any{
				"providerRules": []map[string]any{
					{
						"providerId":   "account:bigmodel-individual-coding-plan",
						"providerName": "BigModel Individual Coding Plan",
						"config": map[string]any{
							"group":           "bigmodel-family",
							"builtinModelIds": []string{"GLM-5.3", "GLM-5.3-Flash"},
							"access": map[string]any{
								"type":        "zhipu-account",
								"mode":        "individual-coding-plan",
								"accountType": "bigmodel",
							},
						},
					},
					{
						"providerId":   "account:zai-individual-coding-plan",
						"providerName": "Z.ai Individual Coding Plan",
						"config": map[string]any{
							"group":           "zai-family",
							"builtinModelIds": []string{"GLM-5.3"},
							"access": map[string]any{
								"type":        "zhipu-account",
								"mode":        "individual-coding-plan",
								"accountType": "zai",
							},
						},
					},
					{
						"providerId": "some-uuid",
						"config": map[string]any{
							"builtinModelIds": []string{"gpt-5.5"},
							"access":          map[string]any{"type": "api-key"},
						},
					},
				},
			},
		},
	}
}

// TestBuildAccountProviderConfig_EntitlesEnabledPlan 是这个包存在的原因:
// 有权的订阅 provider 带上 builtinModelIds 与 entitled:true, 无权的那条
// 明确 false —— 快照推上去之后 app-server 才知道该把哪些模型放进
// settings.model.available。
func TestBuildAccountProviderConfig_EntitlesEnabledPlan(t *testing.T) {
	dir := t.TempDir()
	_, cfg := writeAccountFixture(t, dir, accountFixtureTable(), map[string]any{
		"provider": map[string]any{
			"builtin:bigmodel-coding-plan": map[string]any{
				"enabled": true,
				"options": map[string]any{"apiKey": "key-bigmodel"},
			},
			"builtin:zai-coding-plan": map[string]any{
				"enabled": false,
				"options": map[string]any{"apiKey": ""},
			},
		},
	}, nil)

	payload := BuildAccountProviderConfig(cfg, nil)
	if payload == nil {
		t.Fatal("期望生成账号快照")
	}

	entitled, ok := payload.Providers["account:bigmodel-individual-coding-plan"]
	if !ok {
		t.Fatal("快照缺少 account:bigmodel-individual-coding-plan")
	}
	if !entitled.Access.Entitled {
		t.Error("已启用且有 apiKey 的 plan 应当 entitled")
	}
	if len(entitled.BuiltinModelIDs) != 2 {
		t.Errorf("模型清单应来自内置表, 实际 %v", entitled.BuiltinModelIDs)
	}
	if s := payload.States["account:bigmodel-individual-coding-plan"]; s.Availability != "available" || !s.Current {
		t.Errorf("有权 plan 的状态应为 available/current, 实际 %+v", s)
	}

	// 未启用的 zai 计划必须明确无权 —— 快照会覆盖后端自己算的结论。
	if payload.Providers["account:zai-individual-coding-plan"].Access.Entitled {
		t.Error("未启用的 plan 不得被标为 entitled")
	}

	// 非账号型 provider 不属于这份快照。
	if _, present := payload.Providers["some-uuid"]; present {
		t.Error("api-key 型 provider 不应出现在账号快照里")
	}
}

// TestBuildAccountProviderConfig_PrefersDesktopAvailabilityCache 验证权威性排序:
// 桌面缓存说 available 时, 即使 config.json 里没启用也应当有权。
func TestBuildAccountProviderConfig_PrefersDesktopAvailabilityCache(t *testing.T) {
	dir := t.TempDir()
	_, cfg := writeAccountFixture(t, dir, accountFixtureTable(), map[string]any{
		"provider": map[string]any{},
	}, map[string]any{
		"coding-plan-cache.json": map[string]any{
			"entryStatus": map[string]any{
				"items": map[string]any{
					"builtin:bigmodel-coding-plan": map[string]any{"status": "available"},
					"builtin:zai-coding-plan":      map[string]any{"status": "unavailable"},
				},
			},
		},
	})

	payload := BuildAccountProviderConfig(cfg, nil)
	if payload == nil {
		t.Fatal("期望生成账号快照")
	}
	if !payload.Providers["account:bigmodel-individual-coding-plan"].Access.Entitled {
		t.Error("桌面缓存裁决为 available 的 plan 应当有权")
	}
	if payload.Providers["account:zai-individual-coding-plan"].Access.Entitled {
		t.Error("桌面缓存裁决为 unavailable 的 plan 不应有权")
	}
}

// TestEntitledBuiltinProviders_SettingSelectionIsLastResort 守住一条易错的规则:
// 设置界面的「选择」不等于授权, 绝不能与权威来源取并集 —— 否则用户只是打开过
// 某个 plan 的选择器就会被当成持有该订阅 (实测这会把未授权的 zai plan 也带进来)。
func TestEntitledBuiltinProviders_SettingSelectionIsLastResort(t *testing.T) {
	dir := t.TempDir()
	_, cfg := writeAccountFixture(t, dir, accountFixtureTable(), map[string]any{
		"provider": map[string]any{
			"builtin:bigmodel-coding-plan": map[string]any{
				"enabled": true,
				"options": map[string]any{"apiKey": "key"},
			},
		},
	}, map[string]any{
		// 权威来源已非空 → setting.json 的选择不得参与。
		"setting.json": map[string]any{
			"modelProviderFamilySelectedKeys": map[string]any{
				"zai": "coding-plan:builtin:zai-coding-plan",
			},
		},
	})

	got := entitledBuiltinProviders(cfg)
	if !got["builtin:bigmodel-coding-plan"] {
		t.Error("config.json 启用的 plan 应被推导为有权")
	}
	if got["builtin:zai-coding-plan"] {
		t.Error("设置界面的选择不得被当作授权")
	}
}

// TestEntitledBuiltinProviders_SettingOnlyWhenAuthoritativeEmpty 覆盖兜底分支:
// 两个权威来源都为空时, 设置界面记下的选择是唯一可用信号。
func TestEntitledBuiltinProviders_SettingOnlyWhenAuthoritativeEmpty(t *testing.T) {
	dir := t.TempDir()
	_, cfg := writeAccountFixture(t, dir, accountFixtureTable(), map[string]any{
		"provider": map[string]any{},
	}, map[string]any{
		"setting.json": map[string]any{
			"modelProviderFamilySelectedKeys": map[string]any{
				"bigmodel": "coding-plan:builtin:bigmodel-coding-plan",
			},
		},
	})

	if !entitledBuiltinProviders(cfg)["builtin:bigmodel-coding-plan"] {
		t.Error("权威来源为空时, 设置界面的选择应作为最后手段")
	}
}

// TestConfigProviderIDFor_RoundTrip 锁定两种拼写的映射:
// 内置表用 account:<family>-individual-coding-plan, config.json 用
// builtin:<family>-coding-plan。授权推导全部以后者为准, 弄反会让每个订阅都读成无权。
func TestConfigProviderIDFor_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, cfg := writeAccountFixture(t, dir, accountFixtureTable(), map[string]any{"provider": map[string]any{}}, nil)

	if got := ConfigProviderIDFor("account:bigmodel-individual-coding-plan"); got != "builtin:bigmodel-coding-plan" {
		t.Errorf("account→builtin 映射错误: %q", got)
	}
	if got := AccountProviderIDFor("builtin:bigmodel-coding-plan"); got != "account:bigmodel-individual-coding-plan" {
		t.Errorf("builtin→account 映射错误: %q", got)
	}
	if got := ConfigProviderIDFor("account:zai-start-plan"); got != "builtin:zai-start-plan" {
		t.Errorf("start-plan 映射错误: %q", got)
	}
	// 自定义 (UUID) provider 不该被任何映射改写。
	if got := ConfigProviderIDFor("fd529cb0-df58-4ca1-a11d-816bb6c08494"); got != "fd529cb0-df58-4ca1-a11d-816bb6c08494" {
		t.Errorf("自定义 provider id 不应被改写: %q", got)
	}
	_ = cfg
}

// TestBuiltinRevision_HashesPath 守住推送的关键契约:
// basedOnZCodeBuiltinRevision 的哈希覆盖的是「解析后的路径」而非文件内容,
// 且必须用交给后端的同一个路径 —— 桌面宿主场景下读环境变量会指向一份
// 按版本编号的拷贝, 对它取哈希得到的 revision 会被后端拒绝 (实测: 推送报告
// 已接收但 entitlement 从未生效)。
func TestBuiltinRevision_HashesPath(t *testing.T) {
	dir := t.TempDir()
	tablePath, _ := writeAccountFixture(t, dir, accountFixtureTable(), map[string]any{"provider": map[string]any{}}, nil)

	rev := builtinRevision(tablePath, 42)
	if rev == builtinRevision(tablePath, 43) {
		t.Error("revision 不同, 哈希段应随之不同")
	}
	abs, err := filepath.Abs(tablePath)
	if err != nil {
		t.Fatal(err)
	}
	if rev != builtinRevision(abs, 42) {
		t.Error("相对路径与绝对路径应得出同一 revision")
	}
}

// TestCodingPlanAPIKeyFor_OnlyServesIndividualPlan 守住一条安全边界:
// 只服务 individual coding plan。start-plan 需要阿里云验证码, team/off-peak 的
// key 是存储加密的 —— 读到别的 key 只会让请求带着无效凭证打出去。
func TestCodingPlanAPIKeyFor_OnlyServesIndividualPlan(t *testing.T) {
	dir := t.TempDir()
	_, cfg := writeAccountFixture(t, dir, accountFixtureTable(), map[string]any{
		"provider": map[string]any{
			"builtin:bigmodel-coding-plan": map[string]any{
				"enabled": true,
				"options": map[string]any{"apiKey": "secret-key"},
			},
		},
	}, nil)

	if got := CodingPlanAPIKeyFor("account:bigmodel-individual-coding-plan", cfg); got != "secret-key" {
		t.Errorf("individual plan 应返回 config 里的 key, 实际 %q", got)
	}
	if got := CodingPlanAPIKeyFor("account:bigmodel-start-plan", cfg); got != "" {
		t.Errorf("start-plan 不应被服务 (需验证码), 实际 %q", got)
	}
	if got := CodingPlanAPIKeyFor("account:bigmodel-team-coding-plan", cfg); got != "" {
		t.Errorf("team plan 的 key 是加密的, 不应被服务, 实际 %q", got)
	}
	if got := CodingPlanAPIKeyFor("fd529cb0-df58-4ca1-a11d-816bb6c08494", cfg); got != "" {
		t.Errorf("非 account provider 不应返回 key, 实际 %q", got)
	}
}
