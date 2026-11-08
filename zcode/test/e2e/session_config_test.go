package e2e

import (
	"testing"
	"time"
)

// T04 契约测试: 模式与模型配置的标准 ACP 展示, 以及专有方法的扩展声明。
//
// 规范事实见 internal/bridge/session_config.go 头注释。这组用例钉住的是
// 「声明与实现一致」: sessionCapabilities 声明的 list/resume、new/resume
// 响应投放的 modes/configOptions、标准 set_mode/set_config_option 的往返。

// fakeSettings 构造一份 session/create|resume result 的 settings: 当前模式
// build, 当前模型 prov-1/glm-9, 目录两项 (带 reasoning 等级与上下文窗口)。
func fakeSettings(mode string) map[string]any {
	return map[string]any{
		"mode":  map[string]any{"current": mode},
		"model": fakeModelSettings("prov-1", "glm-9"),
	}
}

// fakeModelSettings 构造 settings.model: current 指向 prov/model, 目录两项。
func fakeModelSettings(prov, model string) map[string]any {
	return map[string]any{
		"current": map[string]any{
			"providerId": prov, "modelId": model,
			"options": map[string]any{"reasoningLevel": "max"},
		},
		"available": []any{
			map[string]any{
				"ref":   map[string]any{"providerId": "prov-1", "modelId": "glm-9"},
				"label": "GLM Nine", "providerLabel": "Z.ai",
				"contextWindow": 200000,
				"reasoning": map[string]any{"defaultLevel": "max", "levels": []any{
					map[string]any{"label": "max", "value": "max"},
				}},
			},
			map[string]any{
				"ref":   map[string]any{"providerId": "prov-2", "modelId": "gpt-5"},
				"label": "GPT Five", "providerLabel": "Other",
				"contextWindow": 400000,
				"reasoning": map[string]any{"defaultLevel": "high", "levels": []any{
					map[string]any{"label": "high", "value": "high"},
					map[string]any{"label": "low", "value": "low"},
				}},
			},
		},
	}
}

// TestInitializeDeclaresExtensionBoundary: initialize 响应必须声明扩展清单,
// 且 sessionCapabilities 与已实现的方法一致 (list/resume/load 都有, P2 起
// loadSession 由 session/load 承接)。
func TestInitializeDeclaresExtensionBoundary(t *testing.T) {
	c := startBridge(t)
	resp := c.request(1, "initialize", map[string]any{"protocolVersion": 1})
	if resp["error"] != nil {
		t.Fatalf("initialize 失败: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)

	caps, _ := result["agentCapabilities"].(map[string]any)
	if caps == nil {
		t.Fatalf("agentCapabilities 缺失: %v", result)
	}
	if caps["loadSession"] != true {
		t.Errorf("loadSession 应为 true (P2 已实现 session/load): %v", caps)
	}
	sessCaps, _ := caps["sessionCapabilities"].(map[string]any)
	if sessCaps == nil || sessCaps["list"] == nil || sessCaps["resume"] == nil {
		t.Errorf("sessionCapabilities 应声明 list 与 resume (均已实现): %v", sessCaps)
	}

	meta, _ := result["_meta"].(map[string]any)
	zmeta, _ := meta["_zcode"].(map[string]any)
	if zmeta == nil {
		t.Fatalf("_meta._zcode 扩展声明缺失: %v", result)
	}
	methods, _ := zmeta["methods"].([]any)
	if len(methods) == 0 {
		t.Fatalf("_meta._zcode.methods 为空: %v", zmeta)
	}
	have := map[string]bool{}
	for _, m := range methods {
		have[m.(string)] = true
	}
	for _, want := range []string{
		"_zcode/session/setModel", "_zcode/session/setMode", "_zcode/session/fork",
		"_zcode/prompt/enhance",
	} {
		if !have[want] {
			t.Errorf("扩展清单缺 %q: %v", want, methods)
		}
	}
}

// TestSessionNewAdvertisesModesAndModel: new 响应按后端 settings 投放 modes
// 与 configOptions (select 类, category=model, 值是 providerId/modelId)。
func TestSessionNewAdvertisesModesAndModel(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{
				"session":  map[string]any{"sessionId": "sess-cap", "mode": "build"},
				"settings": fakeSettings("build"),
			}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "session/new", map[string]any{"cwd": "/tmp", "mode": "build"})
	if resp["error"] != nil {
		t.Fatalf("session/new 失败: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)

	modes, _ := result["modes"].(map[string]any)
	if modes == nil {
		t.Fatalf("modes 缺失: %v", result)
	}
	if modes["currentModeId"] != "build" {
		t.Errorf("currentModeId = %v, want build (来自 settings.mode.current)", modes["currentModeId"])
	}
	avail, _ := modes["availableModes"].([]any)
	ids := map[string]bool{}
	for _, raw := range avail {
		m, _ := raw.(map[string]any)
		ids[m["id"].(string)] = true
		if m["name"] == nil {
			t.Errorf("投放的模式缺 name: %v", m)
		}
	}
	for _, want := range []string{"build", "edit", "plan", "yolo"} {
		if !ids[want] {
			t.Errorf("availableModes 缺 %q: %v", want, avail)
		}
	}

	co, _ := result["configOptions"].([]any)
	if len(co) != 1 {
		t.Fatalf("configOptions 应有 1 项: %v", result["configOptions"])
	}
	cfg, _ := co[0].(map[string]any)
	if cfg["id"] != "model" || cfg["category"] != "model" {
		t.Errorf("配置项 id/category 不符: %v", cfg)
	}
	if cfg["currentValue"] != "prov-1/glm-9" {
		t.Errorf("currentValue = %v, want prov-1/glm-9 (来自 settings.model.current)", cfg["currentValue"])
	}
	opts, _ := cfg["options"].([]any)
	gotIDs := map[string]bool{}
	for _, raw := range opts {
		o, _ := raw.(map[string]any)
		gotIDs[o["id"].(string)] = true
		if o["name"] == nil {
			t.Errorf("模型选项缺 name (展示名): %v", o)
		}
	}
	if !gotIDs["prov-1/glm-9"] || !gotIDs["prov-2/gpt-5"] {
		t.Errorf("模型选项不全: %v", opts)
	}
	// valueId 之外的事实 (上下文窗口/推理档) 走 _meta.zcode, 不能挤进原始形状。
	first, _ := opts[0].(map[string]any)
	zmeta, _ := first["_meta"].(map[string]any)["zcode"].(map[string]any)
	if zmeta == nil || zmeta["contextWindow"] != float64(200000) || zmeta["defaultReasoningLevel"] != "max" {
		t.Errorf("选项 _meta.zcode 缺展示事实: %v", first)
	}
}

// TestSessionNewFallbackModeWithoutSettings: 后端不报 settings 时,
// currentModeId 用请求的 mode 兜底; 没法诚实展示就不投 configOptions。
func TestSessionNewFallbackModeWithoutSettings(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{
				"session": map[string]any{"sessionId": "sess-bare"},
			}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "session/new", map[string]any{"cwd": "/tmp", "mode": "plan"})
	result, _ := resp["result"].(map[string]any)
	modes, _ := result["modes"].(map[string]any)
	if modes["currentModeId"] != "plan" {
		t.Errorf("currentModeId = %v, want plan (请求值兜底)", modes["currentModeId"])
	}
	if _, has := result["configOptions"]; has {
		t.Errorf("没有模型目录就不应投 configOptions: %v", result["configOptions"])
	}
}

// TestSessionSetModeStandard: 标准方法 {sessionId, modeId} → zcode
// session/setMode, 且推 current_mode_update; 未知模式 -32602 不触达后端。
func TestSessionSetModeStandard(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{
				"session":  map[string]any{"sessionId": "sess-mode"},
				"settings": fakeSettings("build"),
			}},
			"session/setMode": {Result: map[string]any{"ok": true}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	f.client.request(2, "session/new", map[string]any{"cwd": "/tmp"})

	resp := f.client.request(3, "session/set_mode", map[string]any{
		"sessionId": "sess-mode", "modeId": "plan",
	})
	if resp["error"] != nil {
		t.Fatalf("set_mode 失败: %v", resp["error"])
	}
	req := f.waitForLog(t, "session/setMode")
	params, _ := req["params"].(map[string]any)
	if params["mode"] != "plan" || params["sessionId"] != "sess-mode" {
		t.Fatalf("zcode 收到的 setMode 参数异常: %v", params)
	}
	upd := f.client.waitUpdate(t, 5*time.Second, func(u map[string]any) bool {
		return u["sessionUpdate"] == "current_mode_update"
	})
	if upd["currentModeId"] != "plan" {
		t.Errorf("current_mode_update.currentModeId = %v, want plan", upd["currentModeId"])
	}

	// 未知模式: -32602, 且不触达后端 (第二次 setMode)。
	resp = f.client.request(4, "session/set_mode", map[string]any{
		"sessionId": "sess-mode", "modeId": "auto",
	})
	e, _ := resp["error"].(map[string]any)
	if e == nil || e["code"] != float64(-32602) {
		t.Fatalf("未知模式应 -32602: %v", resp)
	}
	n := 0
	for _, r := range f.loggedRequests(t) {
		if r["method"] == "session/setMode" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("未知模式不应触达后端 (setMode 日志 %d 条, want 1)", n)
	}
}

// TestSessionSetConfigOptionModel: 标准方法把 valueId 解析成 zcode 的
// setModel (缺省 reasoningLevel 由目录注入), 返回并推送刷新后的 configOptions。
func TestSessionSetConfigOptionModel(t *testing.T) {
	switched := fakeSettings("build")
	switched["model"] = fakeModelSettings("prov-2", "gpt-5")
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/create": {Result: map[string]any{
				"session":  map[string]any{"sessionId": "sess-cfg"},
				"settings": fakeSettings("build"),
			}},
			"session/setModel": {Result: map[string]any{"settings": switched}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	f.client.request(2, "session/new", map[string]any{"cwd": "/tmp"})

	resp := f.client.request(3, "session/set_config_option", map[string]any{
		"sessionId": "sess-cfg", "configId": "model", "value": "prov-2/gpt-5",
	})
	if resp["error"] != nil {
		t.Fatalf("set_config_option 失败: %v", resp["error"])
	}
	req := f.waitForLog(t, "session/setModel")
	params, _ := req["params"].(map[string]any)
	model, _ := params["model"].(map[string]any)
	if model == nil || model["providerId"] != "prov-2" || model["modelId"] != "gpt-5" {
		t.Fatalf("zcode 收到的 setModel 参数异常: %v", params)
	}
	if opts, _ := model["options"].(map[string]any); opts["reasoningLevel"] != "high" {
		t.Errorf("缺省推理等级未按目录注入: %v", model)
	}

	result, _ := resp["result"].(map[string]any)
	co, _ := result["configOptions"].([]any)
	if len(co) != 1 {
		t.Fatalf("响应应带刷新后的 configOptions: %v", result)
	}
	cfg, _ := co[0].(map[string]any)
	if cfg["currentValue"] != "prov-2/gpt-5" {
		t.Errorf("刷新后 currentValue = %v, want prov-2/gpt-5", cfg["currentValue"])
	}
	upd := f.client.waitUpdate(t, 5*time.Second, func(u map[string]any) bool {
		return u["sessionUpdate"] == "config_option_update"
	})
	updCo, _ := upd["configOptions"].([]any)
	if len(updCo) != 1 {
		t.Fatalf("config_option_update 应带 configOptions: %v", upd)
	}

	// 非法输入: 未知 configId / boolean 变体 / 坏 value 都是 -32602。
	for _, c := range []struct {
		name   string
		params map[string]any
	}{
		{"未知 configId", map[string]any{"sessionId": "sess-cfg", "configId": "thought", "value": "x"}},
		{"boolean 变体", map[string]any{"sessionId": "sess-cfg", "configId": "model", "type": "boolean", "value": true}},
		{"模型不在目录", map[string]any{"sessionId": "sess-cfg", "configId": "model", "value": "nope/none"}},
	} {
		resp := f.client.request(4, "session/set_config_option", c.params)
		e, _ := resp["error"].(map[string]any)
		if e == nil || e["code"] != float64(-32602) {
			t.Errorf("%s 应 -32602: %v", c.name, resp)
		}
	}
}

// TestResumeAdvertisesModesAndModel: resume 响应同样投放 modes 与
// configOptions (数据源与 session/new 相同, 都是后端 settings)。
func TestResumeAdvertisesModesAndModel(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/resume": {Result: map[string]any{
				"session":  map[string]any{"sessionId": "res-1", "mode": "edit"},
				"settings": fakeSettings("edit"),
			}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "session/resume", map[string]any{"sessionId": "res-1", "cwd": "/tmp"})
	if resp["error"] != nil {
		t.Fatalf("resume 失败: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	modes, _ := result["modes"].(map[string]any)
	if modes == nil || modes["currentModeId"] != "edit" {
		t.Errorf("resume 的 modes 异常: %v", result["modes"])
	}
	co, _ := result["configOptions"].([]any)
	if len(co) != 1 || co[0].(map[string]any)["currentValue"] != "prov-1/glm-9" {
		t.Errorf("resume 的 configOptions 异常: %v", result["configOptions"])
	}
}

// TestSessionLoadResumesByID: session/load (loadSession 能力, P2) 按 id 装载会话,
// 与 session/resume 同一条 zcode session/resume 路径; mcpServers 参数被显式
// 忽略 (zcode 不支持按会话注入 MCP), 不应导致请求失败。
func TestSessionLoadResumesByID(t *testing.T) {
	f := startScripted(t, fakeScript{
		Responses: map[string]fakeAction{
			"session/resume": {Result: map[string]any{
				"session":  map[string]any{"sessionId": "ld-1", "mode": "build"},
				"settings": fakeSettings("build"),
			}},
		},
	})
	f.client.request(1, "initialize", map[string]any{})
	resp := f.client.request(2, "session/load", map[string]any{
		"sessionId":  "ld-1",
		"cwd":        "/tmp",
		"mcpServers": map[string]any{"fs": map[string]any{"command": "mcp-fs"}},
	})
	if resp["error"] != nil {
		t.Fatalf("session/load 失败: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	modes, _ := result["modes"].(map[string]any)
	if modes == nil || modes["currentModeId"] != "build" {
		t.Errorf("load 的 modes 异常: %v", result["modes"])
	}
	// 空 sessionId 必须 -32602。
	resp = f.client.request(3, "session/load", map[string]any{})
	e, _ := resp["error"].(map[string]any)
	if e == nil || e["code"] != float64(-32602) {
		t.Errorf("缺 sessionId 的 load 应 -32602: %v", resp)
	}
}
