//go:build real

package smoke

import (
	"encoding/json"
	"io"
	"testing"
)

// TestParseElicitationSchemaSingleAndMulti 锁住「从 requestedSchema 还原出题」
// 这件事: 单选是 enum, 多选是「一个选项一个 boolean property」, 两者要能被区分
// 出来并保持顺序稳定 (终端上题序每次都变的话没法用)。
func TestParseElicitationSchemaSingleAndMulti(t *testing.T) {
	// 与 internal/bridge/elicitation.go 的生成形状一致。
	raw := `{
	  "type": "object",
	  "properties": {
	    "风格":            {"type":"string","title":"风格","description":"你要哪种风格","enum":["简洁","详细"]},
	    "模块[a]":         {"type":"boolean","title":"点单","description":"要覆盖哪些模块"},
	    "模块[b]":         {"type":"boolean","title":"收银","description":"要覆盖哪些模块"},
	    "模块[c]":         {"type":"boolean","title":"后厨","description":"要覆盖哪些模块"}
	  },
	  "required": ["风格"]
	}`
	qs, err := parseElicitationSchema(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("题数 = %d, want 2 (1 单选 + 1 多选组)", len(qs))
	}
	// 单选按 required 顺序排在最前。
	if qs[0].Key != "风格" || qs[0].MultiSelect {
		t.Errorf("第 1 题 = %+v, want 单选 key=风格", qs[0])
	}
	if len(qs[0].Values) != 2 || qs[0].Values[0] != "简洁" {
		t.Errorf("单选候选 = %v, want [简洁 详细]", qs[0].Values)
	}
	// 多选组的选项顺序必须跟 schema 里的成员顺序一致。
	multi := qs[1]
	if !multi.MultiSelect || multi.Key != "模块" {
		t.Fatalf("第 2 题 = %+v, want 多选组 key=模块", multi)
	}
	if len(multi.Values) != 3 || multi.Values[0] != "a" || multi.Values[2] != "c" {
		t.Errorf("多选候选 = %v, want [a b c]", multi.Values)
	}
	if multi.Labels["b"] != "收银" {
		t.Errorf("选项 b 的展示名 = %q, want 收银", multi.Labels["b"])
	}
}

// TestParseElicitationSchemaRejectsJunk 挡掉会让人没法作答的 schema。
func TestParseElicitationSchemaRejectsJunk(t *testing.T) {
	for name, raw := range map[string]string{
		"空":            ``,
		"不是对象":         `{"type":"string"}`,
		"缺 properties": `{"type":"object"}`,
	} {
		if _, err := parseElicitationSchema(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: 期望报错, 实际解析通过", name)
		}
	}
}

// TestParseElicitationSchemaDropsUnanswerable 没有任何 enum 的单选 property
// 摆到人面前也答不了, 应当被丢掉而不是当成一道题渲染。
func TestParseElicitationSchemaDropsUnanswerable(t *testing.T) {
	raw := `{"type":"object","properties":{
	  "空题":{"type":"string","title":"空题"},
	  "正常题":{"type":"string","title":"正常题","enum":["x","y"]}},
	  "required":["空题","正常题"]}`
	qs, err := parseElicitationSchema(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(qs) != 1 || qs[0].Key != "正常题" {
		t.Fatalf("题 = %+v, want 只保留 正常题", qs)
	}
}

// TestParseSelectionIndices 覆盖多选输入的几种写法与越界拒绝。
func TestParseSelectionIndices(t *testing.T) {
	for _, tc := range []struct {
		in    string
		n     int
		want  []int // 期望选中的下标
		valid bool
	}{
		{"1", 3, []int{0}, true},
		{"1,3", 3, []int{0, 2}, true},
		{"1 3", 3, []int{0, 2}, true},
		{"1、3", 3, []int{0, 2}, true},
		{"1, 2, 3", 3, []int{0, 1, 2}, true},
		{"", 3, nil, true}, // 空 = 一个都不选, 不是错
		{"4", 3, nil, false},
		{"0", 3, nil, false},
		{"abc", 3, nil, false},
		{"1,abc", 3, nil, false},
	} {
		got, ok := parseSelectionIndices(tc.in, tc.n)
		if ok != tc.valid {
			t.Errorf("parseSelectionIndices(%q, %d) ok = %v, want %v", tc.in, tc.n, ok, tc.valid)
			continue
		}
		if !tc.valid {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseSelectionIndices(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("parseSelectionIndices(%q) = %v, 缺下标 %d", tc.in, got, w)
			}
		}
	}
}

// TestBelowAnswersAlwaysEmitsBooleans 是那个不变量: 多选**必须**至少回一个
// boolean property。桥的 readAnswer 见到一个都没出现会判「这题没答」而整组
// cancel, 所以「一个都不选」也要显式回全 false, 不能靠省略表达。
func TestBelowAnswersAlwaysEmitsBooleans(t *testing.T) {
	q := elicitationQuestion{
		Key: "模块", Question: "覆盖哪些模块", MultiSelect: true,
		Values: []string{"a", "b", "c"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}

	// 全不选: 三个 boolean 都要出现, 值都是 false。
	ans := map[string]any{}
	belowAnswers(ans, q, map[int]bool{})
	if len(ans) != 3 {
		t.Fatalf("全不选时 answers = %v, want 3 个 boolean property", ans)
	}
	for _, k := range []string{"模块[a]", "模块[b]", "模块[c]"} {
		v, ok := ans[k]
		if !ok {
			t.Fatalf("全不选时缺 property %s: %v", k, ans)
		}
		if b, _ := v.(bool); b {
			t.Errorf("%s 应为 false, got %v", k, v)
		}
	}

	// 勾选第 2 项: 只有它是 true。
	ans = map[string]any{}
	belowAnswers(ans, q, map[int]bool{1: true})
	if ans["模块[b]"] != true {
		t.Errorf("模块[b] = %v, want true", ans["模块[b]"])
	}
	if ans["模块[a]"] != false || ans["模块[c]"] != false {
		t.Errorf("未勾选项应为 false: %v", ans)
	}
}

// TestAutoElicitationOutcome 锁住非交互模式的自动应答形状: 每题选第一项。
func TestAutoElicitationOutcome(t *testing.T) {
	qs := []elicitationQuestion{
		{Key: "风格", Values: []string{"简洁", "详细"}, Labels: map[string]string{}, Descriptions: map[string]string{}},
		{Key: "模块", MultiSelect: true, Values: []string{"a", "b"},
			Labels: map[string]string{}, Descriptions: map[string]string{}},
	}
	action, content := autoElicitationOutcome(qs)
	if action != "accept" {
		t.Fatalf("action = %q, want accept", action)
	}
	// content 必须是**扁平**属性表: 桥的 readAnswer 直接按 property 名取。
	if _, wrapped := content["answers"]; wrapped {
		t.Fatalf("content 不该套 answers 包装: %v", content)
	}
	ans := content
	if ans["风格"] != "简洁" {
		t.Errorf("风格 = %v, want 简洁 (第一项)", ans["风格"])
	}
	if ans["模块[a]"] != true {
		t.Errorf("模块[a] = %v, want true (勾第一项)", ans["模块[a]"])
	}
	if len(ans) != 3 {
		t.Errorf("answers = %v, want 3 个 property (单选 1 + 多选 2)", ans)
	}
}

// TestElicitationRoundTripAgainstBridgeShape 端到端看一眼: 把解析出来的题作答
// 后, 产出的 answers 能被桥的 readAnswer 认下 (靠 required/enum/propName 的
// 命名约定对齐)。这里复刻 test/e2e/elicitation_test.go 里的真实形状。
func TestElicitationRoundTripAgainstBridgeShape(t *testing.T) {
	raw := `{"type":"object","properties":{
	  "你选哪个":{"type":"string","title":"你选哪个","description":"你选哪个","enum":["A","B"]},
	  "多选[甲]":{"type":"boolean","title":"甲","description":"多选"},
	  "多选[乙]":{"type":"boolean","title":"乙","description":"多选"}},
	  "required":["你选哪个"]}`
	qs, err := parseElicitationSchema(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	ans := map[string]any{}
	ans[qs[0].Key] = qs[0].Values[1]                // 单选 B
	belowAnswers(ans, qs[1], map[int]bool{1: true}) // 多选只勾乙

	if ans["你选哪个"] != "B" {
		t.Errorf("单选答案 = %v, want B", ans["你选哪个"])
	}
	if ans["多选[甲]"] != false || ans["多选[乙]"] != true {
		t.Errorf("多选答案 = %v, want 甲=false 乙=true", ans)
	}
}

// TestElicitationContentIsFlatTable 是踩过的坑的回归锁: content 一旦套上
// {"answers": …}, 桥的 readAnswer 一个 property 都取不到, 会判「应答缺失或不在
// 允许取值内」并**整组 cancel** —— 现象是终端答完了却什么都没发生, 而模型会
// 改问一遍。真机首跑就是这么失败的。
func TestElicitationContentIsFlatTable(t *testing.T) {
	q := elicitationQuestion{
		Key: "风格", Question: "风格", Values: []string{"简洁", "详细"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}
	_, content := autoElicitationOutcome([]elicitationQuestion{q})
	if _, ok := content["风格"]; !ok {
		t.Errorf("单选答案应直接挂在 content 顶层, got %v", content)
	}
	if _, ok := content["answers"]; ok {
		t.Errorf("不该有 answers 包装层, got %v", content)
	}
}

// scriptedLines 把一串输入变成 lineSource, 用完即止 (取完返回 ok=false, 模拟
// 终端 EOF)。
func scriptedLines(lines ...string) lineSource {
	i := 0
	return func() (string, bool) {
		if i >= len(lines) {
			return "", false
		}
		l := lines[i]
		i++
		return l, true
	}
}

// TestCollectAnswersTwoFormsInOneProcess 覆盖「同一个进程里连着来两张表」。
//
// 模型被拒后会立刻改问一遍, 所以一次 turn 里出现多张表是常态而不是边角; 而且
// 早先 /dev/tty 是每张表开关一次、带缓冲的 reader 却存在进程级变量里, 于是第一
// 张表答完一关句柄, 第二张表立刻读不到输入直接作废。答题逻辑必须扛住这个形状。
func TestCollectAnswersTwoFormsInOneProcess(t *testing.T) {
	qs1 := []elicitationQuestion{{
		Key: "第一题", Question: "第一题", Values: []string{"甲", "乙"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}}
	qs2 := []elicitationQuestion{{
		Key: "第二题", Question: "第二题", MultiSelect: true, Values: []string{"a", "b", "c"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}}

	action1, c1 := collectElicitationAnswers(qs1, scriptedLines("1"), io.Discard)
	if action1 != "accept" || c1["第一题"] != "甲" {
		t.Fatalf("第一张表 = (%q, %v), want (accept, 甲)", action1, c1)
	}

	// 第二张表用**另一个**输入源 (等价于新的终端会话): 必须照样能答。
	action2, c2 := collectElicitationAnswers(qs2, scriptedLines("1,3"), io.Discard)
	if action2 != "accept" {
		t.Fatalf("第二张表 action = %q, want accept", action2)
	}
	if c2["第二题[a]"] != true || c2["第二题[c]"] != true || c2["第二题[b]"] != false {
		t.Errorf("第二张表 answers = %v, want a/c = true", c2)
	}
}

// TestCollectAnswersSingleSelectOutOfRangeThenRetry: 单选输了越界编号时要**重新问**,
// 而不是作废整轮提问。越界的编号绝不能被当成真选择。
func TestCollectAnswersSingleSelectOutOfRangeThenRetry(t *testing.T) {
	qs := []elicitationQuestion{{
		Key: "选一个", Question: "选一个", Values: []string{"简洁", "详细", "适中"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}}
	action, content := collectElicitationAnswers(qs, scriptedLines("9", "2"), io.Discard)
	if action != "accept" {
		t.Fatalf("action = %q, want accept (越界后应重问, 不该作废)", action)
	}
	if content["选一个"] != "详细" {
		t.Errorf("答案 = %v, want 详细 (第 2 项)", content["选一个"])
	}
}

// TestCollectAnswersSingleSelectRejectsMultipleNumbers 单选一次输多个也要重问。
func TestCollectAnswersSingleSelectRejectsMultipleNumbers(t *testing.T) {
	qs := []elicitationQuestion{{
		Key: "选一个", Question: "选一个", Values: []string{"a", "b", "c"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}}
	action, content := collectElicitationAnswers(qs, scriptedLines("1,2", "3"), io.Discard)
	if action != "accept" || content["选一个"] != "c" {
		t.Fatalf("(%q, %v), want (accept, c)", action, content)
	}
}

// TestCollectAnswersSingleSelectBlankCancels 单选留空 = 作废; 多选留空 = 都不选。
// 两者语义不同, 不能混同。
func TestCollectAnswersSingleSelectBlankCancels(t *testing.T) {
	single := []elicitationQuestion{{
		Key: "选一个", Question: "选一个", Values: []string{"a", "b"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}}
	if action, _ := collectElicitationAnswers(single, scriptedLines(""), io.Discard); action != "cancel" {
		t.Errorf("单选留空 action = %q, want cancel", action)
	}

	multi := []elicitationQuestion{{
		Key: "多选", Question: "多选", MultiSelect: true, Values: []string{"a", "b"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}}
	action, content := collectElicitationAnswers(multi, scriptedLines(""), io.Discard)
	if action != "accept" {
		t.Fatalf("多选留空 action = %q, want accept (都不选是合法答案)", action)
	}
	// 关键: 必须显式回全 false, 否则桥判「这题没答」而整组 cancel。
	for _, k := range []string{"多选[a]", "多选[b]"} {
		if content[k] != false {
			t.Errorf("%s = %v, want false (都要出现)", k, content[k])
		}
	}
}

// TestCollectAnswersEOFOnSecondForm 输入耗尽 (模拟没控制终端) 时应 cancel 而不是 panic。
func TestCollectAnswersEOFOnSecondForm(t *testing.T) {
	qs := []elicitationQuestion{{
		Key: "选一个", Question: "选一个", Values: []string{"a", "b"},
		Labels: map[string]string{}, Descriptions: map[string]string{},
	}}
	action, _ := collectElicitationAnswers(qs, scriptedLines(), io.Discard)
	if action != "cancel" {
		t.Errorf("输入耗尽时 action = %q, want cancel", action)
	}
}

// TestTTYSessionIsProcessResident 钉住那个生命周期不匹配: 句柄与 reader 必须同生
// 共死。若哪天有人改回「每次提问 open+close /dev/tty」, 这里会先炸。
func TestTTYSessionIsProcessResident(t *testing.T) {
	a := openTTY()
	if a == nil {
		t.Skip("没有控制终端 (后台任务/CI), 跳过")
	}
	b := openTTY()
	if a != b {
		t.Fatalf("openTTY 两次返回了不同会话 (%p vs %p): 句柄与 reader 会生命周期错配",
			a, b)
	}
	// 连续两次提问之间不关句柄: 读一行应当仍然可用。
	if _, ok := a.readLine(); ok {
		t.Log("  ✓ 常驻会话仍可读")
	}
}
