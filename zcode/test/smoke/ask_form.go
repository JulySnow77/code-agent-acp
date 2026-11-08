//go:build real

package smoke

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 终端侧的 elicitation (form mode) 客户端 —— 让 AskUserQuestion 在终端里
// **真的能问人、也能多选**。
//
// 为什么要它: 降级路径 (session/request_permission) 表达不了 multiSelect,
// 桥会整组回 cancel, 于是模型被迫把多选题改问成单选 —— 终端用户既没被问到,
// 也永远见不到多选。本文件让交互式客户端声明 elicitation.form 能力, 于是
// 桥走 elicitation/create, 整组问题一次送达, 多选按 checkbox 语义作答。
//
// 协议契约 (与 internal/bridge/elicitation.go 对齐, 那边是生成方):
//
//	请求 elicitation/create {sessionId, mode:"form", message, requestedSchema}
//	  - 单选题: properties[key] = {type:"string", title, description, enum:[value…]}
//	    required 里有 key。应答 content.answers[key] = 选中的 value。
//	  - 多选题: properties["key[value]"] = {type:"boolean", title, description}
//	    每个选项一个 property, **不进 required**。应答时只对勾选的回 true
//	    (不回 = 不选)。
//	应答 {action:"accept"|"decline"|"cancel", content:{<property>: 值, …}}
//	  —— content 是**扁平**的属性表。桥的 readAnswer 直接在 reply.Content 上按
//	  property 名取值, 外面再套一层 {"answers": …} 它一个都取不到, 于是判
//	  「应答缺失」而整组 cancel (e2e 的 replyElicitation 是这个形状)。
//
// 关键不变量: 桥的 readAnswer 对多选要求**至少出现一个** boolean property
// (present==0 判为「这题没答」)。所以「一个都没选」不能靠全不写表达, 必须
// 显式回 false —— 见 belowAnswers。

// elicitationQuestion 是从 requestedSchema 还原出来的一道题。
type elicitationQuestion struct {
	// Key 是 schema property 名: 单选 = 问题原文; 多选 = 组前缀 (不含尾部
	// "[value]"), answers 的键就是 key / key[value]。
	Key string
	// Question 是人类可读的问题原文 (取自 property 的 description/title)。
	Question string
	// MultiSelect 为 true 时本题是 checkbox 组。
	MultiSelect bool
	// Values 按 schema 里的出现顺序排好的候选值 (多选即各 property 的 value)。
	Values []string
	// Labels 是 value → 展示文案 (取 property.title, 退回 value)。
	Labels map[string]string
	// Descriptions 是 value → 补充说明。
	Descriptions map[string]string
}

// propKeySplit 把多选的 property 名拆成 (组前缀, 选项值)。
// 单选的 property 名不含 "[" — 返回 (name, "", false)。
func propKeySplit(prop string) (prefix, value string, isMulti bool) {
	i := strings.LastIndex(prop, "[")
	if i <= 0 || !strings.HasSuffix(prop, "]") {
		return prop, "", false
	}
	return prop[:i], prop[i+1 : len(prop)-1], true
}

// parseElicitationSchema 把 requestedSchema 还原成有序的题目列表。
//
// 顺序: 单选按 required 的出现顺序; 多选组按其第一个 property 在 JSON 对象里
// 的出现顺序。Go 的 map 无序, 所以这里不依赖 map 迭代顺序 —— 而是从
// raw JSON 对象里按 key 顺序扫一遍 (encoding/json 保留对象内成员顺序, 用
// json.Decoder 的 Token 流可以拿到), 再按 required 补齐单选。这样终端上
// 摆出来的题序稳定, 不会每次跑都不一样。
func parseElicitationSchema(raw json.RawMessage) ([]elicitationQuestion, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("requestedSchema 缺失")
	}
	var doc struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string   `json:"type"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 requestedSchema 失败: %w", err)
	}
	if doc.Type != "" && doc.Type != "object" {
		return nil, fmt.Errorf("requestedSchema.type = %q, 期望 object", doc.Type)
	}

	// 按 JSON 对象里的原始顺序扫一遍, 保证多选组的顺序稳定。
	order, err := jsonObjectKeyOrder(raw)
	if err != nil {
		return nil, err
	}

	byKey := map[string]*elicitationQuestion{}
	var multiOrder []string
	for _, name := range order {
		p, ok := doc.Properties[name]
		if !ok {
			continue
		}
		prefix, value, isMulti := propKeySplit(name)
		if !isMulti {
			if len(p.Enum) == 0 {
				continue // 既不是多选成员又没有 enum: 摆到人面前也没法答
			}
			q := &elicitationQuestion{
				Key:          name,
				Question:     firstNonEmpty(p.Description, p.Title),
				Values:       append([]string{}, p.Enum...),
				Labels:       map[string]string{},
				Descriptions: map[string]string{},
			}
			for _, v := range q.Values {
				q.Labels[v] = firstNonEmpty(p.Title, v)
			}
			byKey[name] = q
			continue
		}
		q, ok := byKey[prefix]
		if !ok {
			q = &elicitationQuestion{
				Key:          prefix,
				Question:     firstNonEmpty(p.Description, p.Title),
				MultiSelect:  true,
				Labels:       map[string]string{},
				Descriptions: map[string]string{},
			}
			byKey[prefix] = q
			multiOrder = append(multiOrder, prefix)
		}
		if p.Description != "" && q.Question == "" {
			q.Question = p.Description
		}
		q.Values = append(q.Values, value)
		q.Labels[value] = firstNonEmpty(p.Title, value)
		if p.Description != "" {
			q.Descriptions[value] = p.Description
		}
	}

	out := make([]elicitationQuestion, 0, len(byKey))
	seen := map[string]bool{}
	for _, name := range doc.Required { // 单选严格按 required 顺序
		if q, ok := byKey[name]; ok && !seen[name] {
			out = append(out, *q)
			seen[name] = true
		}
	}
	for _, name := range multiOrder { // 再按出现顺序补多选组
		if !seen[name] {
			out = append(out, *byKey[name])
			seen[name] = true
		}
	}
	return out, nil
}

// jsonObjectKeyOrder 返回 requestedSchema.properties 的成员顺序。
//
// 必须走 Token 流而不是 map 遍历: 终端上题序要稳定, 而 map 顺序是随机的。
func jsonObjectKeyOrder(raw json.RawMessage) ([]string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("解析 requestedSchema 失败: %w", err)
	}
	propsRaw, ok := top["properties"]
	if !ok {
		return nil, fmt.Errorf("requestedSchema 没有 properties")
	}
	dec := json.NewDecoder(strings.NewReader(string(propsRaw)))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("properties 不是对象")
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := k.(string)
		if !ok {
			return nil, fmt.Errorf("properties 的成员名不是字符串")
		}
		keys = append(keys, name)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// parseSelectionIndices 解析人输入的选项编号, 支持 "1" / "1,3" / "1 3" / "1、3"。
//
// 返回 (选中下标集合, 是否解析成功)。空输入**不算错**: 调用方按题目的语义决定
// 「空 = 不选」(多选) 还是「空 = 作废」(单选)。越界编号直接失败, 宁可让人重输,
// 也不要把一个错位的编号当成真选择。
func parseSelectionIndices(input string, n int) (map[int]bool, bool) {
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ' ' || r == '\t'
	})
	out := map[int]bool{}
	if len(fields) == 0 {
		return out, true
	}
	for _, f := range fields {
		i, err := strconv.Atoi(f)
		if err != nil || i < 1 || i > n {
			return nil, false
		}
		out[i-1] = true
	}
	return out, true
}

// belowAnswers 把一组勾选结果落成 answers 里该题要回的那些 property。
//
// 多选必须至少回一个 boolean (见文件头的不变量): 未勾选的显式回 false,
// 而不是省略 —— 省略全部会被桥判成「这题没答」而整组 cancel。
func belowAnswers(answers map[string]any, q elicitationQuestion, picked map[int]bool) {
	if !q.MultiSelect {
		return
	}
	for i, v := range q.Values {
		answers[q.propValueName(v)] = picked[i]
	}
}

// propValueName 是多选答案里某个选项对应的 property 名 (与生成方 propName 同构)。
func (q elicitationQuestion) propValueName(value string) string {
	return q.Key + "[" + value + "]"
}

// renderElicitationForm 把一张 elicitation 表打到终端, 返回可交互的题目列表。
func renderElicitationForm(out io.Writer, params map[string]any) ([]elicitationQuestion, error) {
	raw, _ := json.Marshal(params["requestedSchema"])
	qs, err := parseElicitationSchema(raw)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "\n🔷 人工应答 (elicitation/create, form)\n")
	if msg := strings.TrimSpace(fmt.Sprint(params["message"])); msg != "" && msg != "<nil>" {
		fmt.Fprintf(out, "  说明: %s\n", msg)
	}
	if len(qs) == 0 {
		fmt.Fprintf(out, "  (schema 里没有可答的题目)\n")
		return qs, nil
	}
	for qi, q := range qs {
		tag := "单选"
		if q.MultiSelect {
			tag = "多选"
		}
		fmt.Fprintf(out, "\n  问题 %d/%d [%s]: %s\n", qi+1, len(qs), tag, q.Question)
		for i, v := range q.Values {
			desc := q.Descriptions[v]
			if desc == "" || desc == q.Question {
				desc = ""
			} else {
				desc = "  — " + desc
			}
			label := q.Labels[v]
			if label == "" || label == v {
				fmt.Fprintf(out, "    [%d] %s%s\n", i+1, v, desc)
				continue
			}
			fmt.Fprintf(out, "    [%d] %s%s  (value=%s)\n", i+1, label, desc, v)
		}
	}
	return qs, nil
}

// lineSource 是「读一行人输入」的来源。抽出来是为了让答题逻辑可单测:
// 交互时它接控制终端, 测试里它接脚本化的字符串序列 —— 于是「越界重输」
// 「连续两张表单」这类行为不必真开 pty 就能验。
type lineSource func() (string, bool)

// interactiveElicitationOutcome 在终端逐题收集答案, 产出 accept 应答。
//
// 返回 (action, content)。人输入空行的行为按题目分:
//   - 单选: 空 = 作废 (action=cancel) —— 单选没有「不选」这个合法答案, 而
//     硬塞一个枚举外的值会被桥拒掉, 整组 cancel。
//   - 多选: 空 = 一个都不选 (合法, answers 里显式回全 false)。
func interactiveElicitationOutcome(t *testing.T, params map[string]any) (string, map[string]any) {
	out, read := ttyIO()
	qs, err := renderElicitationForm(out, params)
	if err != nil {
		t.Logf("  ⚠ 渲染 elicitation 表失败 (%v), 回 decline", err)
		return "decline", nil
	}
	return collectElicitationAnswers(qs, read, out)
}

// collectElicitationAnswers 是答题逻辑本体: 逐题向 read 要输入, 解析、重试,
// 最后产出扁平的 content (桥按 property 名直接取, 见文件头协议契约)。
//
// 越界/认不出的输入一律**重新问**, 不当作选择 —— 一次输错不至于毁掉整轮提问。
// 单选要恰好一个; 多选任意个 (含零个)。
func collectElicitationAnswers(qs []elicitationQuestion, read lineSource, out io.Writer) (string, map[string]any) {
	if len(qs) == 0 {
		return "decline", nil
	}
	answers := map[string]any{}
	for qi, q := range qs {
		if q.MultiSelect {
			picked, ok := askMultiSelect(qs, qi, q, read, out)
			if !ok {
				return "cancel", nil
			}
			belowAnswers(answers, q, picked)
			continue
		}
		value, ok := askSingleSelect(qs, qi, q, read, out)
		if !ok {
			return "cancel", nil
		}
		answers[q.Key] = value
	}
	fmt.Fprintf(out, "  → 你的作答: %s\n", formatAnswers(answers))
	return "accept", answers
}

// askSingleSelect 反复问直到拿到**恰好一个**合法选择; 人留空 = 作废。
func askSingleSelect(qs []elicitationQuestion, qi int, q elicitationQuestion, read lineSource, out io.Writer) (string, bool) {
	for {
		fmt.Fprintf(out, "  问题 %d/%d 选一个, 输编号或 value (直接回车=作废): ", qi+1, len(qs))
		line, ok := read()
		if !ok {
			fmt.Fprintln(out, "  (拿不到终端输入, cancel)")
			return "", false
		}
		line = strings.TrimSpace(line)
		if line == "" {
			fmt.Fprintln(out, "  (留空 → 作废本次提问)")
			return "", false
		}
		// 编号: 必须恰好一个, 越界/多个都要重问。
		if picked, ok := parseSelectionIndices(line, len(q.Values)); ok {
			switch {
			case len(picked) == 0:
				fmt.Fprintf(out, "  ⚠ 没认出编号, 请重输: ")
			case len(picked) == 1:
				for i := range picked {
					return q.Values[i], true
				}
			default:
				fmt.Fprintf(out, "  ⚠ 单选只能选一个 (你选了 %d 个), 请重输: ", len(picked))
			}
			continue
		}
		// 认不出编号形态就当 value 处理 —— 但必须是候选之一。
		for _, v := range q.Values {
			if v == line {
				return v, true
			}
		}
		fmt.Fprintf(out, "  ⚠ %q 既不是合法编号也不在候选里, 请重输: ", line)
	}
}

// askMultiSelect 反复问直到拿到一组合法编号; 人留空 = 一个都不选。
func askMultiSelect(qs []elicitationQuestion, qi int, q elicitationQuestion, read lineSource, out io.Writer) (map[int]bool, bool) {
	for {
		fmt.Fprintf(out, "  问题 %d/%d 可多选, 输编号 (如 1,3; 留空=都不选): ", qi+1, len(qs))
		line, ok := read()
		if !ok {
			fmt.Fprintln(out, "  (拿不到终端输入, cancel)")
			return nil, false
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return map[int]bool{}, true // 都不选是合法答案 (显式回全 false)
		}
		picked, ok := parseSelectionIndices(line, len(q.Values))
		if !ok {
			fmt.Fprintf(out, "  ⚠ 编号认不出, 请重输: ")
			continue
		}
		return picked, true
	}
}

// autoElicitationOutcome 非交互模式: 每题选第一项, 让人也能在 CI 里跑通
// elicitation 路径 (与降级路径的自动应答对称)。
func autoElicitationOutcome(qs []elicitationQuestion) (string, map[string]any) {
	answers := map[string]any{}
	for _, q := range qs {
		if len(q.Values) == 0 {
			continue
		}
		if q.MultiSelect {
			belowAnswers(answers, q, map[int]bool{0: true}) // 勾第一项
			continue
		}
		answers[q.Key] = q.Values[0]
	}
	return "accept", answers // 扁平属性表, 不套 answers (见 interactiveElicitationOutcome)
}

// formatAnswers 把 answers 渲染成一行可读文本 (排序保证稳定)。
func formatAnswers(answers map[string]any) string {
	keys := make([]string, 0, len(answers))
	for k := range answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, answers[k]))
	}
	return strings.Join(parts, ", ")
}

// ttyIO 返回「写提示」与「读一行」两个端点, 都绑在**同一个进程级常驻**的终端
// 会话上。
//
// 为什么要常驻: 早先每次提问都 open+close 一次 /dev/tty, 而 bufio.Reader 挂在
// 进程级变量上 —— 第一次提问一关, 缓冲与句柄就悬空了, 第二次提问(模型追问)
// 立刻读不到东西, 现象是「第一张表答得好好的, 第二张直接作废」。句柄和 reader
// 必须同生共死, 所以两者都缓存在这里, 进程退出时由 OS 回收。
func ttyIO() (io.Writer, lineSource) {
	if sess := openTTY(); sess != nil {
		return sess, sess.readLine
	}
	// 没有控制终端 (后台任务/CI): 提示打到 stderr, 读 stdin。
	return os.Stderr, readStdinLine
}

// wantElicitationForm 报不报 elicitation.form 能力。
//
// 交互模式 (ZCODE_SMOKE_INTERACTIVE=1) 默认**报** —— 终端里应该能多选, 而降级
// 路径表达不了 multiSelect。设 ZCODE_SMOKE_ASK_FALLBACK=1 可强制走降级路径,
// 用来对照「客户端不支持多选时桥会 cancel 并让模型改问单选」这个行为。
func wantElicitationForm() bool {
	if smokeFlag("ZCODE_SMOKE_ASK_FALLBACK") {
		return false
	}
	return smokeFlag("ZCODE_SMOKE_INTERACTIVE")
}

// elicitationClientCapabilities 是要附加到 initialize/session/new 的
// clientCapabilities。报 form 就够 —— 桥只看 elicitation.form 是否显式存在。
func elicitationClientCapabilities() map[string]any {
	if !wantElicitationForm() {
		return nil
	}
	return map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}
}
