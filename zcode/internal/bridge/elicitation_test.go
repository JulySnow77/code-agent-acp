package bridge

import "testing"

// TestParseClientCaps: 哪些 clientCapabilities 形状算「声明了 elicitation.form」。
//
// 这条判定的坑在于 ACP **明确不沿用** MCP 的约定: MCP 里 `elicitation: {}` 表示
// 「支持, 且只支持 form」, 而 ACP 里每个模式必须显式存在且非 null 才算声明
// (agentclientprotocol.com/protocol/v1/elicitation, "Checking support")。
// 照 MCP 的直觉写会把「零模式」误判成「支持 form」, 于是对没声明能力的客户端
// 发出 elicitation/create —— 违反 "Agents MUST NOT request a mode the Client
// has not advertised"。
func TestParseClientCaps(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   bool
	}{
		{"完全没有 clientCapabilities", `{}`, false},
		{"clientCapabilities 为 null", `{"clientCapabilities":null}`, false},
		{"没有 elicitation", `{"clientCapabilities":{"fs":{"readTextFile":true}}}`, false},
		{"elicitation 为 null", `{"clientCapabilities":{"elicitation":null}}`, false},
		{"elicitation 是空对象 (零模式, 不是「仅 form」)", `{"clientCapabilities":{"elicitation":{}}}`, false},
		{"form 为 null", `{"clientCapabilities":{"elicitation":{"form":null}}}`, false},
		{"form 为 null, url 有值 —— url 不蕴含 form", `{"clientCapabilities":{"elicitation":{"form":null,"url":{}}}}`, false},
		{"form 显式为空对象", `{"clientCapabilities":{"elicitation":{"form":{}}}}`, true},
		{"form 与 url 都有", `{"clientCapabilities":{"elicitation":{"form":{},"url":{}}}}`, true},
		{"参数不是合法 JSON", `{`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseClientCaps([]byte(c.params))
			if got.elicitationForm != c.want {
				t.Errorf("elicitationForm = %v, want %v (%s)", got.elicitationForm, c.want, c.params)
			}
		})
	}
}

// TestAskOptionsUsesValueAsOptionID: 降级路径的 optionId 必须是 zcode 的 value。
//
// 这是 T03 修的 plan 审批 bug 的核心: 选项 {label:"Approve", value:"approve"} 若用
// label 当 optionId 并把 label 回传, zcode 拿到的答案是 "Approve", 与常量 "approve"
// 逐字比对失败 → 判为未批准。
func TestAskOptionsUsesValueAsOptionID(t *testing.T) {
	q := map[string]any{
		"options": []any{
			map[string]any{"label": "Approve", "value": "approve"},
			map[string]any{"label": "Reject", "value": "reject"},
		},
	}
	opts := askOptions(q)
	if len(opts) != 3 {
		t.Fatalf("选项数 = %d, want 3 (2 选项 + 取消项)", len(opts))
	}
	if opts[0]["optionId"] != "approve" || opts[0]["name"] != "Approve" {
		t.Errorf("option[0] = %v, want optionId=approve name=Approve", opts[0])
	}
	if opts[1]["optionId"] != "reject" {
		t.Errorf("option[1].optionId = %v, want reject", opts[1]["optionId"])
	}
	if opts[2]["optionId"] != askCancelOptionID {
		t.Errorf("最后一项应是取消哨兵 %q, got %v", askCancelOptionID, opts[2]["optionId"])
	}
}

// TestAskOptionsSkipsUnusableOptions: value 缺失 / 重复的选项不投放 ——
// value 才是应答契约, 缺失就没有合法 optionId, 重复则回包无法区分。
func TestAskOptionsSkipsUnusableOptions(t *testing.T) {
	q := map[string]any{
		"options": []any{
			map[string]any{"label": "只有 label", "value": ""},
			map[string]any{"label": "A", "value": "a"},
			map[string]any{"label": "A 重复", "value": "a"},
			map[string]any{"label": "B", "value": "b"},
			"不是对象",
		},
	}
	opts := askOptions(q)
	var ids []string
	for _, o := range opts {
		ids = append(ids, o["optionId"].(string))
	}
	want := []string{"a", "b", askCancelOptionID}
	if len(ids) != len(want) {
		t.Fatalf("optionId = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("optionId = %v, want %v", ids, want)
		}
	}
}

// TestAskOptionsNeverCollidesWithCancelSentinel: 取消哨兵不能与任何 zcode 的 value
// 撞车 —— 撞车等于把那个选项解释成取消。撞上时该选项被跳过, 绝不投放。
func TestAskOptionsNeverCollidesWithCancelSentinel(t *testing.T) {
	q := map[string]any{
		"options": []any{
			map[string]any{"label": "撞车", "value": askCancelOptionID},
			map[string]any{"label": "正常", "value": "ok"},
		},
	}
	opts := askOptions(q)
	if len(opts) != 2 {
		t.Fatalf("选项数 = %d, want 2 (撞车的被跳过 + 正常项 + 取消项)", len(opts))
	}
	if opts[0]["optionId"] != "ok" {
		t.Errorf("option[0].optionId = %v, want ok", opts[0]["optionId"])
	}
	if opts[1]["optionId"] != askCancelOptionID {
		t.Errorf("option[1] 应为取消哨兵, got %v", opts[1]["optionId"])
	}
}

// TestAnswerFromPermission: 降级路径只认投放过的 optionId。
//
// 自由文本与 JSON 数组 (旧实现的 omnigent 私有编码) 都不再被当答案 —— 把任意文本
// 解释成用户的选择是授权协议上的漏洞。
func TestAnswerFromPermission(t *testing.T) {
	q := map[string]any{
		"options": []any{
			map[string]any{"label": "React", "value": "react"},
			map[string]any{"label": "Vue", "value": "vue"},
		},
	}
	cases := []struct {
		name    string
		outcome string
		optID   string
		want    any
		wantOK  bool
	}{
		{"选中合法 value", "selected", "vue", "vue", true},
		{"取消 outcome", "cancelled", "", nil, false},
		{"空 optionId", "selected", "", nil, false},
		{"取消哨兵", "selected", askCancelOptionID, nil, false},
		{"自由文本", "selected", "Svelte 吧", nil, false},
		{"label 而非 value", "selected", "Vue", nil, false},
		{"JSON 数组 (旧私有编码)", "selected", `["react","vue"]`, nil, false},
		{"未知 id", "selected", "__cancel__", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := answerFromPermission(c.outcome, c.optID, q)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (答案 = %v)", ok, c.wantOK, got)
			}
			if got != c.want {
				t.Errorf("答案 = %v, want %v", got, c.want)
			}
		})
	}
}

// TestReadAnswerRejectsOffEnum: ACP 要求 agent 再校验一次客户端回的值。
// 这一步是「客户端回什么都不能让 zcode 收到枚举外的答案」的最后一道闸。
func TestReadAnswerRejectsOffEnum(t *testing.T) {
	single := askQuestion{key: "Which framework?", values: []string{"react", "vue"}}
	if v, ok := single.readAnswer(map[string]any{"Which framework?": "vue"}); !ok || v != "vue" {
		t.Errorf("合法值被拒: %v %v", v, ok)
	}
	for _, bad := range []any{
		"Vue",        // label, 不在 enum 内
		"",           // 空
		42,           // 类型不对
		[]any{"vue"}, // 单选不接受数组
	} {
		if _, ok := single.readAnswer(map[string]any{"Which framework?": bad}); ok {
			t.Errorf("枚举外的取值 %v 被接受", bad)
		}
	}
	if _, ok := single.readAnswer(map[string]any{}); ok {
		t.Errorf("缺答案应被拒")
	}

	// 多选: 一个选项一个 boolean property, 缺省 = 不选。
	multi := askQuestion{
		key: "Which platforms?", values: []string{"ios", "android"}, multiSelect: true,
	}
	got, ok := multi.readAnswer(map[string]any{
		"Which platforms?[ios]": true, "Which platforms?[android]": true,
	})
	if !ok {
		t.Fatalf("合法多选被拒")
	}
	arr, _ := got.([]any)
	if len(arr) != 2 || arr[0] != "ios" || arr[1] != "android" {
		t.Errorf("多选 = %v, want [ios android]", arr)
	}
	// 只勾一个 → 只回那一个。
	got, ok = multi.readAnswer(map[string]any{"Which platforms?[ios]": true})
	if !ok {
		t.Errorf("单勾一个应被接受")
	}
	if arr, _ := got.([]any); len(arr) != 1 || arr[0] != "ios" {
		t.Errorf("单选多选的结果 = %v, want [ios]", arr)
	}
	// 全 false 是合法答案 (用户一个都没选), 但一个 property 都不出现是「没答」。
	if v, ok := multi.readAnswer(map[string]any{
		"Which platforms?[ios]": false, "Which platforms?[android]": false,
	}); !ok {
		t.Errorf("全不选是合法答案")
	} else if arr, _ := v.([]any); len(arr) != 0 {
		t.Errorf("全不选应得空数组, got %v", arr)
	}
	if _, ok := multi.readAnswer(map[string]any{}); ok {
		t.Errorf("一个 property 都没出现应判为没答")
	}
	// boolean property 上回非 boolean → 整组放弃。
	if _, ok := multi.readAnswer(map[string]any{"Which platforms?[ios]": "yes"}); ok {
		t.Errorf("非 boolean 的多选取值被接受")
	}
}

// TestElicitationSchemaUsesOnlyPrimitives: form 模式的 property 只能是原始类型或
// enum (elicitation.md:101 "properties use the supported primitive and enum
// schemas"; schema.md 的 ElicitationSchema: "must be primitive types")。
//
// 多选**不能**用 `type:"array"` —— 数组两者都不是, 严格的客户端会按 -32602 拒掉整个
// 请求, 于是每一个多选题都被悄悄降级。这条测试把这个坑钉住。
func TestElicitationSchemaUsesOnlyPrimitives(t *testing.T) {
	questions := []askQuestion{
		{key: "Which framework?", question: "Which framework?", header: "Framework",
			values: []string{"react", "vue"}, labels: map[string]string{"react": "React", "vue": "Vue"}},
		{key: "Which platforms?", question: "Which platforms?", header: "Platforms",
			values: []string{"ios", "android"}, labels: map[string]string{"ios": "iOS"},
			multiSelect: true},
	}
	schema := elicitationSchema(questions)
	props, _ := schema["properties"].(map[string]any)
	primitive := map[string]bool{"string": true, "boolean": true, "number": true, "integer": true}

	for name, raw := range props {
		def, _ := raw.(map[string]any)
		typ, _ := def["type"].(string)
		if !primitive[typ] {
			t.Errorf("property %q 的 type=%q, form 模式只允许原始类型/enum (elicitation.md:101)", name, typ)
		}
	}
	// 单选 = string + enum。
	single, _ := props["Which framework?"].(map[string]any)
	if single["type"] != "string" {
		t.Errorf("单选 property.type = %v, want string", single["type"])
	}
	// 多选 = 每选项一个 boolean, 绝不能是 array。
	for name, raw := range props {
		def, _ := raw.(map[string]any)
		if name == "Which platforms?" {
			t.Errorf("多选题不该有聚合 property %q", name)
			continue
		}
		if def["type"] == "array" {
			t.Errorf("property %q 用了 array —— form 模式不接受数组", name)
		}
	}
	if _, ok := props["Which platforms?[ios]"]; !ok {
		t.Errorf("多选应展开为每选项一个 boolean property, 实际 keys=%v", keysOfMap(props))
	}
	// required 只列单选题: 强制客户端为未勾选项回 false 是不合理的要求。
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "Which framework?" {
		t.Errorf("required = %v, want 只含单选题", req)
	}
}

func keysOfMap(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
