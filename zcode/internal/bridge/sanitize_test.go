package bridge

import (
	"strings"
	"testing"
)

// P0 敌意 fixture 套件 (调研纪要 §6.1 / §6.2): 走协议层解决渲染层同类问题, 用
// 协议层fixture 把 agent-authored 不可信输入的攻击面钉住。wmux 的文件头问题
// 定义 —— 「一个不告诉人在问什么的待决请求会让按键变盲」 —— 这里的反方向是:
// 一个塞满控制字符/巨型文本的请求会污染客户端渲染, 所以展示副本必须截断+清洗,
// 而应答契约 (value/question 原文键) 绝不许被动。

// TestSanitizeDisplayTruncatesBeforeProcessing: 清洗必须发生在截断之后 —— 多 MB
// 字段若先清洗就是 O(n) 攻击面 (wmux: 在任何正则处理之前先截断, MAX_RAW_LEN)。
func TestSanitizeDisplayTruncatesBeforeProcessing(t *testing.T) {
	huge := strings.Repeat("a", 1<<20) // 1 MB
	got := sanitizeDisplay(huge, maxQuestionLen)
	if n := len([]rune(got)); n != maxQuestionLen {
		t.Errorf("超长字段截断后 = %d runes, want %d", n, maxQuestionLen)
	}
	// limit ≤ 0: 只做原始截断 + 清洗, 不做展示截断。
	got = sanitizeDisplay(huge, 0)
	if n := len([]rune(got)); n != maxRawFieldLen {
		t.Errorf("limit=0 时应截到 maxRawFieldLen=%d, got %d", maxRawFieldLen, n)
	}
}

// TestSanitizeDisplayStripsControlChars: ANSI 转义 (\x1b[31m) 与 \r 回卷攻击
// 必须整类消掉; 展示换行 \n 与制表 \t 保留。
func TestSanitizeDisplayStripsControlChars(t *testing.T) {
	got := sanitizeDisplay("normal \x1b[31mred\x1b[0m\r text\n\tkeep", 0)
	if strings.ContainsAny(got, "\x1b\r") {
		t.Errorf("控制字符残留: %q", got)
	}
	// ESC 前导被剥掉后, "[31m" 剩下的是惰性文本 —— 终端不会把它解释成颜色序列。
	want := "normal [31mred[0m text\n\tkeep"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestParseAskQuestionsHostileShapes: shape 守卫 —— 字段缺失/类型异常不 panic,
// 不阻断其余问题的创建 (wmux: 没题干的请求也远胜于没有请求)。
func TestParseAskQuestionsHostileShapes(t *testing.T) {
	qs := parseAskQuestions([]any{
		"不是对象",
		map[string]any{"question": 42.0, "options": "不是数组"},         // 数字题面 widen 成字符串, options 类型错
		map[string]any{"question": "", "options": []any{42.0, nil}}, // 空题面 + 非对象选项
		map[string]any{"question": "正常题", "multiSelect": "yes", // 非 bool multiSelect 当 false
			"options": []any{map[string]any{"value": "a"}}},
	}, false)
	if len(qs) != 3 {
		t.Fatalf("解析出 %d 题, want 3 (非对象项被丢弃)", len(qs))
	}
	if qs[0].question != "42" {
		t.Errorf("数字题面 widen = %q, want %q", qs[0].question, "42")
	}
	if len(qs[0].values) != 0 {
		t.Errorf("options 类型错时应无选项, got %v", qs[0].values)
	}
	if qs[1].key == "" || len(qs[1].values) != 0 {
		t.Errorf("空题面应退 answer_<i> 且无选项, got key=%q values=%v", qs[1].key, qs[1].values)
	}
	if qs[2].multiSelect {
		t.Errorf("字符串 multiSelect 应当 false")
	}
	if len(qs[2].values) != 1 || qs[2].values[0] != "a" {
		t.Errorf("正常题的选项应保留, got %v", qs[2].values)
	}
	// 空题面 fallback 键唯一。
	if qs[1].key != "answer_1" {
		t.Errorf("空题面 fallback 键 = %q, want answer_1", qs[1].key)
	}
}

// TestParseAskQuestionsCapsOptions: 选项数上限 (wmux: 8)。第 9 个起丢弃,
// 展示巨型表单无从谈起; 应答契约不受影响 —— 投放的都是合法 value。
func TestParseAskQuestionsCapsOptions(t *testing.T) {
	var raw []any
	for i := 0; i < 20; i++ {
		raw = append(raw, map[string]any{
			"label": strings.Repeat("L", 100), // 超长 label 也要截断
			"value": string(rune('a' + i)),
		})
	}
	qs := parseAskQuestions([]any{map[string]any{"question": "q", "options": raw}}, false)
	if len(qs) != 1 {
		t.Fatalf("题数 = %d, want 1", len(qs))
	}
	if len(qs[0].values) != maxOptionsPerQuestion {
		t.Errorf("选项数 = %d, want 上限 %d", len(qs[0].values), maxOptionsPerQuestion)
	}
	if n := len([]rune(qs[0].labels["a"])); n != maxOptionLabelLen {
		t.Errorf("label 截断 = %d runes, want %d", n, maxOptionLabelLen)
	}
}

// TestParseAskQuestionsSkipsOversizedValues: value 是应答契约不能截断, 超长 value
// 的选项整项不投放 (它撑爆 enum 载荷与多选 property 名, 本就不可用)。
func TestParseAskQuestionsSkipsOversizedValues(t *testing.T) {
	huge := strings.Repeat("v", maxOptionValueLen+1)
	qs := parseAskQuestions([]any{map[string]any{"question": "q", "options": []any{
		map[string]any{"label": "超大 value", "value": huge},
		map[string]any{"label": "正常", "value": "ok"},
	}}}, false)
	if len(qs[0].values) != 1 || qs[0].values[0] != "ok" {
		t.Errorf("超长 value 的选项应被跳过, got %v", qs[0].values)
	}
}

// TestParseAskQuestionsLongQuestionFallsBack: 超长题面不得成为 requestedSchema 的
// property 名 (多 KB JSON 键 = 资源攻击), 退 answer_<i>; 应答键仍是 zcode 原文,
// 契约不破。短题面维持原行为 (property 名 = 题面)。
func TestParseAskQuestionsLongQuestionFallsBack(t *testing.T) {
	long := strings.Repeat("问", maxQuestionLen+1)
	qs := parseAskQuestions([]any{
		map[string]any{"question": long, "options": []any{map[string]any{"value": "x"}}},
		map[string]any{"question": "短题", "options": []any{map[string]any{"value": "y"}}},
	}, false)
	if qs[0].key != "answer_0" {
		t.Errorf("超长题面的 property 键 = %q, want answer_0", qs[0].key)
	}
	if qs[0].question != long {
		t.Errorf("契约原文被改写: %d runes, want %d", len([]rune(qs[0].question)), maxQuestionLen+1)
	}
	if n := len([]rune(qs[0].display)); n != maxQuestionLen {
		t.Errorf("展示副本 = %d runes, want 截到 %d", n, maxQuestionLen)
	}
	if qs[1].key != "短题" {
		t.Errorf("短题面的 property 键应变原文, got %q", qs[1].key)
	}
}

// TestAskOptionsSanitizesAndCaps: 降级路径同样吃 P0 规则: label 清洗截断、
// 选项数封顶、超长 value 跳过, 取消哨兵始终追加在最后。
func TestAskOptionsSanitizesAndCaps(t *testing.T) {
	var raw []any
	for i := 0; i < 12; i++ {
		raw = append(raw, map[string]any{"label": "\x1b[1m选项\x1b[0m" + strings.Repeat("x", 200), "value": string(rune('a' + i))})
	}
	opts := askOptions(map[string]any{"options": raw})
	if len(opts) != maxOptionsPerQuestion+1 {
		t.Fatalf("降级选项数 = %d, want %d (8 投放 + 取消)", len(opts), maxOptionsPerQuestion+1)
	}
	if n := len([]rune(opts[0]["name"].(string))); n != maxOptionLabelLen {
		t.Errorf("降级 label 截断 = %d runes, want %d", n, maxOptionLabelLen)
	}
	if strings.ContainsRune(opts[0]["name"].(string), 0x1b) {
		t.Errorf("降级 label 残留 ANSI 转义: %q", opts[0]["name"])
	}
	last := opts[len(opts)-1]
	if last["optionId"] != askCancelOptionID {
		t.Errorf("末项应为取消哨兵, got %v", last["optionId"])
	}
}

// TestSanitizedQuestionView: rawInput 进客户端的展示视图必须已清洗 —— 契约
// (value/optionId 匹配) 不动, 展示 (question/label) 截断且不含控制字符。
func TestSanitizedQuestionView(t *testing.T) {
	q := map[string]any{
		"question": "\x1b[31m" + strings.Repeat("恶", maxQuestionLen+50),
		"header":   "hdr\x00",
		"options": []any{
			map[string]any{"label": "\x1b]8;;http://x\x07链接", "value": "v1"},
		},
	}
	view := sanitizedQuestionView(q)
	if n := len([]rune(view["question"].(string))); n != maxQuestionLen {
		t.Errorf("rawInput 题面 = %d runes, want 截到 %d", n, maxQuestionLen)
	}
	if strings.ContainsAny(view["question"].(string), "\x1b\x00") {
		t.Errorf("rawInput 题面残留控制字符")
	}
	if view["header"] != "hdr" {
		t.Errorf("rawInput header = %q, want 清洗后 hdr", view["header"])
	}
	opts := view["options"].([]any)
	o := opts[0].(map[string]any)
	if o["value"] != "v1" {
		t.Errorf("rawInput 契约 value = %v, want v1 (不许清洗)", o["value"])
	}
	if strings.ContainsRune(o["label"].(string), 0x1b) || strings.ContainsRune(o["label"].(string), 0x07) {
		t.Errorf("rawInput label 残留 OSC/控制字符: %q", o["label"])
	}
}

// TestParsePermissionOptionsSanitizesDisplay: 权限选项的 name/description 是
// 模型可影响的展示文本; optionId/response 是 zcode broker 的契约, 不动。
func TestParsePermissionOptionsSanitizesDisplay(t *testing.T) {
	params := map[string]any{"options": []any{
		map[string]any{
			"optionId":    "allow-once",
			"kind":        "allow_once",
			"name":        "\x1b[32m允许\x1b[0m" + strings.Repeat("n", 200),
			"description": strings.Repeat("d", maxQuestionLen+100),
			"response":    map[string]any{"decision": "allow"},
		},
	}}
	opts := parsePermissionOptions(params)
	if len(opts) != 1 {
		t.Fatalf("权限选项数 = %d, want 1", len(opts))
	}
	if n := len([]rune(opts[0].name)); n != maxOptionLabelLen {
		t.Errorf("name 截断 = %d runes, want %d", n, maxOptionLabelLen)
	}
	if strings.ContainsRune(opts[0].name, 0x1b) {
		t.Errorf("name 残留 ANSI 转义: %q", opts[0].name)
	}
	if n := len([]rune(opts[0].desc)); n != maxQuestionLen {
		t.Errorf("desc 截断 = %d runes, want %d", n, maxQuestionLen)
	}
	if opts[0].optionID != "allow-once" || opts[0].response["decision"] != "allow" {
		t.Errorf("契约字段被改写: id=%v response=%v", opts[0].optionID, opts[0].response)
	}
}
