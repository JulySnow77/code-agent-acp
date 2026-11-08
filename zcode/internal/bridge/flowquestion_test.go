package bridge

import (
	"reflect"
	"regexp"
	"testing"
)

// flow-questions v1 (T003) 的客户端能力与无损题型投影。
//
// 契约来源: docs/julyboard/flow-question-protocol.md (JulyBoard 仓), 桥侧按
// `_meta.multica.flowQuestions=v1` 协商; 未声明 flow 能力的客户端走既有 form
// 路径, 行为逐字不变 (既有 elicitation_test.go 的断言就是那边的护栏)。

// TestParseClientCapsFlowQuestions: 客户端在 clientCapabilities._meta.multica
// 声明 flowQuestions; 只有精确 "v1" 才算协商成功, 其余一律当不支持。
func TestParseClientCapsFlowQuestions(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   bool
	}{
		{"声明 v1", `{"clientCapabilities":{"elicitation":{"form":{}},"_meta":{"multica":{"flowQuestions":"v1"}}}}`, true},
		{"版本不匹配 v2", `{"clientCapabilities":{"_meta":{"multica":{"flowQuestions":"v2"}}}}`, false},
		{"版本为空", `{"clientCapabilities":{"_meta":{"multica":{"flowQuestions":""}}}}`, false},
		{"multica 缺失", `{"clientCapabilities":{"_meta":{"zcode":{"cancelRequest":true}}}}`, false},
		{"flowQuestions 非 string", `{"clientCapabilities":{"_meta":{"multica":{"flowQuestions":1}}}}`, false},
		{"_meta 缺失", `{"clientCapabilities":{"elicitation":{"form":{}}}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseClientCaps([]byte(c.params))
			if got.flowQuestions != c.want {
				t.Errorf("flowQuestions = %v, want %v (%s)", got.flowQuestions, c.want, c.params)
			}
		})
	}
}

// TestFlowQuestionsMetaDeclaration: initialize 结果里的 _meta.multica 声明必须与
// 跨仓契约文档逐字一致 —— daemon 侧凭它决定是否启用 Flow 问答路由。
func TestFlowQuestionsMetaDeclaration(t *testing.T) {
	got := flowQuestionsMeta()
	want := map[string]any{
		"flowQuestions":      "v1",
		"stableQuestionId":   true,
		"freeText":           true,
		"multiSelect":        map[string]any{"minSelections": true},
		"suspendProof":       "components",
		"consumptionReceipt": "native-call",
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Fatalf("声明缺 %q: %v", k, got)
		}
		if !reflect.DeepEqual(gv, v) {
			t.Errorf("声明[%q] = %v (%T), want %v (%T)", k, gv, gv, v, v)
		}
	}
}

// TestFlowModeTextQuestionSchema: 无选项题在 flow 模式投影为 string property
// 并列入 required (非 flow 模式在 askViaElicitation 层整组放弃, 由既有测试钉住)。
func TestFlowModeTextQuestionSchema(t *testing.T) {
	qs := parseAskQuestions([]any{map[string]any{
		"question": "需要保留哪些约束？",
	}}, true)
	if len(qs) != 1 {
		t.Fatalf("questions = %d, want 1", len(qs))
	}
	if qs[0].kind != "text" {
		t.Fatalf("kind = %q, want text", qs[0].kind)
	}
	schema := elicitationSchema(qs)
	props, _ := schema["properties"].(map[string]any)
	prop, ok := props["需要保留哪些约束？"].(map[string]any)
	if !ok {
		t.Fatalf("text property missing: %v", schema)
	}
	if prop["type"] != "string" {
		t.Errorf("text property type = %v, want string", prop["type"])
	}
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "需要保留哪些约束？" {
		t.Errorf("required = %v, want [question key]", required)
	}
}

// TestFlowModeMultiBoundsCarried: multi 题的 min/max 不进 boolean 展开
// (ACP form 只有原始类型), 桥在问题 meta 里携带, 由 daemon 侧投影成
// min_selections/max_selections。
func TestFlowModeMultiBoundsCarried(t *testing.T) {
	min, max := 1, 2
	qs := parseAskQuestions([]any{map[string]any{
		"question":      "支持哪些平台？",
		"multiSelect":   true,
		"minSelections": min,
		"maxSelections": max,
		"options": []any{
			map[string]any{"label": "iOS", "value": "ios"},
			map[string]any{"label": "Android", "value": "android"},
			map[string]any{"label": "Web", "value": "web"},
		},
	}}, true)
	if len(qs) != 1 || qs[0].kind != "multi" {
		t.Fatalf("questions = %+v, want one multi", qs)
	}
	if qs[0].minSelections == nil || *qs[0].minSelections != 1 {
		t.Errorf("minSelections = %v, want 1", qs[0].minSelections)
	}
	if qs[0].maxSelections == nil || *qs[0].maxSelections != 2 {
		t.Errorf("maxSelections = %v, want 2", qs[0].maxSelections)
	}
	meta := askRequestMeta(map[string]any{}, qs, true)
	entries, _ := meta["questions"].([]any)
	if len(entries) != 1 {
		t.Fatalf("meta questions = %d, want 1", len(entries))
	}
	entry, _ := entries[0].(map[string]any)
	if entry["minSelections"] != 1 || entry["maxSelections"] != 2 {
		t.Errorf("meta min/max = %v/%v, want 1/2", entry["minSelections"], entry["maxSelections"])
	}
	if entry["kind"] != "multi" {
		t.Errorf("meta kind = %v, want multi", entry["kind"])
	}
}

var stableIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// TestFlowModeStableQuestionID: 同题重报得到同一 id; 换选项得到不同 id;
// 服务端提供的 questionId 原样透传; 同请求内两道同文题的派生 id 必须被区分。
func TestFlowModeStableQuestionID(t *testing.T) {
	base := map[string]any{
		"question": "使用哪种存储？",
		"options": []any{
			map[string]any{"label": "数据库", "value": "db"},
			map[string]any{"label": "任务目录", "value": "files"},
		},
	}
	first := parseAskQuestions([]any{base}, true)
	second := parseAskQuestions([]any{base}, true)
	if first[0].id != second[0].id {
		t.Errorf("同题两次解析 id 不稳定: %q vs %q", first[0].id, second[0].id)
	}
	if !stableIDPattern.MatchString(first[0].id) {
		t.Errorf("id %q 不符合平台短串约束", first[0].id)
	}

	changed := map[string]any{
		"question": "使用哪种存储？",
		"options": []any{
			map[string]any{"label": "数据库", "value": "pg"},
			map[string]any{"label": "任务目录", "value": "files"},
		},
	}
	third := parseAskQuestions([]any{changed}, true)
	if third[0].id == first[0].id {
		t.Errorf("选项集不同 id 却相同: %q", first[0].id)
	}

	withID := map[string]any{
		"question":    "使用哪种存储？",
		"questionId":  "storage",
		"multiSelect": false,
		"options":     base["options"],
	}
	fourth := parseAskQuestions([]any{withID}, true)
	if fourth[0].id != "storage" {
		t.Errorf("服务端提供的 questionId 应透传, got %q", fourth[0].id)
	}

	twin := parseAskQuestions([]any{base, base}, true)
	if twin[0].id == twin[1].id {
		t.Errorf("同请求内两道同文题的派生 id 撞车: %q", twin[0].id)
	}
}

// TestFlowModeReadAnswerCustomText: 单选自定义文本按原文回传; 未声明
// allowCustomText 的自由文本仍然整组拒绝 (授权红线: 客户端说什么都不算答案);
// multi 的自定义文本与勾选互斥。
func TestFlowModeReadAnswerCustomText(t *testing.T) {
	custom := parseAskQuestions([]any{map[string]any{
		"question":        "使用哪种存储？",
		"allowCustomText": true,
		"options": []any{
			map[string]any{"label": "数据库", "value": "db"},
			map[string]any{"label": "任务目录", "value": "files"},
		},
	}}, true)[0]

	if v, ok := custom.readAnswer(map[string]any{"使用哪种存储？": "对象存储"}); !ok || v != "对象存储" {
		t.Errorf("自定义文本 = %v %v, want 对象存储", v, ok)
	}
	if v, ok := custom.readAnswer(map[string]any{"使用哪种存储？": "db"}); !ok || v != "db" {
		t.Errorf("选项答案 = %v %v, want db", v, ok)
	}
	if _, ok := custom.readAnswer(map[string]any{"使用哪种存储？": "  "}); ok {
		t.Error("空白自定义文本不能当答案")
	}

	strict := parseAskQuestions([]any{map[string]any{
		"question": "使用哪种存储？",
		"options":  []any{map[string]any{"label": "数据库", "value": "db"}},
	}}, true)[0]
	if _, ok := strict.readAnswer(map[string]any{"使用哪种存储？": "对象存储"}); ok {
		t.Error("未声明 allowCustomText 的自由文本必须被拒")
	}

	multiCustom := parseAskQuestions([]any{map[string]any{
		"question":        "支持哪些平台？",
		"multiSelect":     true,
		"allowCustomText": true,
		"options": []any{
			map[string]any{"label": "iOS", "value": "ios"},
			map[string]any{"label": "Android", "value": "android"},
		},
	}}, true)[0]
	mixed := map[string]any{
		"支持哪些平台？[ios]":        true,
		"支持哪些平台？[__custom__]": "只想要桌面端",
	}
	if _, ok := multiCustom.readAnswer(mixed); ok {
		t.Error("勾选与自定义文本混填必须被拒 (互斥)")
	}
	pureCustom := map[string]any{"支持哪些平台？[__custom__]": "只想要桌面端"}
	if v, ok := multiCustom.readAnswer(pureCustom); !ok || v != "只想要桌面端" {
		t.Errorf("纯自定义文本 = %v %v, want 只想要桌面端", v, ok)
	}
}

// TestFlowModeTextReadAnswer: 文本题收非空字符串, 空白不算已答。
func TestFlowModeTextReadAnswer(t *testing.T) {
	text := parseAskQuestions([]any{map[string]any{
		"question": "需要保留哪些约束？",
	}}, true)[0]
	if v, ok := text.readAnswer(map[string]any{"需要保留哪些约束？": "保留现有执行队列"}); !ok || v != "保留现有执行队列" {
		t.Errorf("文本答案 = %v %v", v, ok)
	}
	if _, ok := text.readAnswer(map[string]any{"需要保留哪些约束？": "  "}); ok {
		t.Error("空白文本不算已答")
	}
	if _, ok := text.readAnswer(map[string]any{}); ok {
		t.Error("缺键不算已答")
	}
}

// TestFlowModeQuestionIDMeta: _meta.multica.questionId 以 property 名为键 ——
// daemon 收到应答后按 content 键直接查 id, 多选题的每个 boolean property
// 都指向同一道题的 id。
func TestFlowModeQuestionIDMeta(t *testing.T) {
	qs := parseAskQuestions([]any{
		map[string]any{
			"question":        "使用哪种存储？",
			"allowCustomText": true,
			"options": []any{
				map[string]any{"label": "数据库", "value": "db"},
				map[string]any{"label": "任务目录", "value": "files"},
			},
		},
		map[string]any{
			"question":        "支持哪些平台？",
			"multiSelect":     true,
			"allowCustomText": true,
			"options": []any{
				map[string]any{"label": "iOS", "value": "ios"},
				map[string]any{"label": "Android", "value": "android"},
			},
		},
	}, true)
	meta := flowQuestionIDMeta(qs)
	// 单选的自定义文本走主属性 (readAnswer 接受非枚举字符串), 不设独立 property;
	// 多选的勾选是 boolean property, 自定义文本才有 __custom__。
	if len(meta) != 4 {
		t.Fatalf("questionId 键数 = %d, want 4 (单选 1 + 多选 boolean 2 + 多选自定义 1)", len(meta))
	}
	for _, key := range []string{"使用哪种存储？", "支持哪些平台？[ios]", "支持哪些平台？[android]", "支持哪些平台？[__custom__]"} {
		if id, ok := meta[key].(string); !ok || id == "" {
			t.Errorf("questionId[%q] = %v, want non-empty", key, meta[key])
		}
	}
	if meta["支持哪些平台？[ios]"] != meta["支持哪些平台？[android]"] {
		t.Error("同一多选题的两个 boolean property 必须指向同一 id")
	}
	if meta["使用哪种存储？"] == meta["支持哪些平台？[ios]"] {
		t.Error("两道不同题的 id 不应相同")
	}
}

// TestFlowMultiQuestionGetsSelectionBounds: 平台契约要求 multi 题必须带
// min/max_selections（缺任一即以 multi_without_bounds 拒收整份信封），而 CLI 的
// askUserQuestion schema 只有 multiSelect、没有边界字段。桥在投影时补默认边界：
// 缺下界按 1、缺上界按选项数，并夹紧到 1 <= min <= max <= 选项数。
func TestFlowMultiQuestionGetsSelectionBounds(t *testing.T) {
	raw := []any{
		map[string]any{
			"question":    "提醒渠道？",
			"multiSelect": true,
			"options": []any{
				map[string]any{"value": "inapp", "label": "站内"},
				map[string]any{"value": "email", "label": "邮件"},
				map[string]any{"value": "push", "label": "推送"},
			},
		},
	}
	questions := platformQuestions(parseAskQuestions(raw, true))
	if len(questions) != 1 {
		t.Fatalf("题数 = %d, want 1", len(questions))
	}
	entry, ok := questions[0].(map[string]any)
	if !ok {
		t.Fatalf("题形状 = %T, want map", questions[0])
	}
	min, hasMin := entry["min_selections"].(int)
	max, hasMax := entry["max_selections"].(int)
	if !hasMin || !hasMax {
		t.Fatalf("多选题缺边界: %v", entry)
	}
	if min != 1 || max != 3 {
		t.Errorf("边界 = [%d, %d], want [1, 3]（缺下界按 1、缺上界按选项数）", min, max)
	}
}

// TestFlowSingleQuestionHasNoSelectionBounds: 单选题不适用边界，不能被补上。
func TestFlowSingleQuestionHasNoSelectionBounds(t *testing.T) {
	raw := []any{
		map[string]any{
			"question": "存储方案？",
			"options": []any{
				map[string]any{"value": "local", "label": "本地优先"},
				map[string]any{"value": "server", "label": "服务端"},
			},
		},
	}
	questions := platformQuestions(parseAskQuestions(raw, true))
	entry, ok := questions[0].(map[string]any)
	if !ok {
		t.Fatalf("题形状 = %T, want map", questions[0])
	}
	if _, has := entry["min_selections"]; has {
		t.Errorf("单选题带了下界: %v", entry)
	}
	if _, has := entry["max_selections"]; has {
		t.Errorf("单选题带���上界: %v", entry)
	}
}

// TestFlowMultiQuestionKeepsExplicitBounds: CLI 明确给了边界时按原值投影，不被
// 默认值覆盖。
func TestFlowMultiQuestionKeepsExplicitBounds(t *testing.T) {
	raw := []any{
		map[string]any{
			"question":      "提醒渠道？",
			"multiSelect":   true,
			"minSelections": 2,
			"maxSelections": 2,
			"options": []any{
				map[string]any{"value": "inapp", "label": "站内"},
				map[string]any{"value": "email", "label": "邮件"},
				map[string]any{"value": "push", "label": "推送"},
			},
		},
	}
	questions := platformQuestions(parseAskQuestions(raw, true))
	entry, _ := questions[0].(map[string]any)
	min, _ := entry["min_selections"].(int)
	max, _ := entry["max_selections"].(int)
	if min != 2 || max != 2 {
		t.Errorf("边界 = [%d, %d], want [2, 2]（CLI 给了就按原值）", min, max)
	}
}
