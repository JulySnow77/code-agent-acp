package bridge

import (
	"strings"
	"unicode/utf8"
)

// P0 安全加固: 把提问/权限请求里的展示文本当 **agent-authored 不可信输入** 处理。
//
// 做法直接来自多 agent 编排调研纪要 §6.1 (wmux src/daemon/approvals/askUserQuestion.ts
// 的文件头问题定义, 纪要原话「值得直接抄进自家代码」):
//
//	审批被编码成「按第一个选项键」, 一个不告诉人「在问什么」的待决请求, 会让那
//	一下按键变盲。Approve 对同意型问题是安全的, 对「该删哪个文件?」是危险的。
//
// 三条规则:
//  1. **先截断再处理** —— 任何清洗/正则之前, 原始字段先硬截到 maxRawFieldLen,
//     防止多 MB 字段让后续处理变成 O(n)。
//  2. **清洗控制字符** —— agent 能在题面/选项标签里塞 ANSI 转义序列 (\x1b[…),
//     污染客户端终端渲染; \r 还能回卷已显示内容。只保留 \n 与 \t。
//  3. **shape 守卫** —— 缺失/类型异常的字段按空值处理, 不 panic、不阻断请求
//     创建: 「没题干的请求也远胜于没有请求」(wmux)。本仓的 zcode.Str/Slice/Sub
//     类型安全读取 + filterQuestionDicts 已覆盖大部分, 这里不再重复。
//
// 一条红线: **应答契约字段 (option.value / question 原文作为 zcode 答案键) 绝不
// 截断或清洗** —— 改了契约, 桥回给 zcode 的答案就对不上它的查找键。展示副本
// 与契约原文在 askQuestion 里分开携带 (display vs question)。
const (
	// maxRawFieldLen 是任何清洗/截断之前的原始字段硬上限 (wmux 的 MAX_RAW_LEN)。
	maxRawFieldLen = 1024
	// maxQuestionLen 题面/描述的展示截断上限 (wmux: 题面 200)。
	maxQuestionLen = 200
	// maxOptionLabelLen 选项 label / 权限选项名的展示截断上限 (wmux: 选项 80)。
	maxOptionLabelLen = 80
	// maxOptionsPerQuestion 每题投放的选项数上限 (wmux: 选项数 8)。
	maxOptionsPerQuestion = 8
	// maxOptionValueLen 选项 value 的长度上限。value 是应答契约不能截断, 但超长
	// value 会撑爆 enum 载荷 / 多选 property 名 (key[value]), 这种选项投放出去
	// 本就不可用 —— 整项跳过, 不投放。
	maxOptionValueLen = 256
)

// sanitizeDisplay 把一段 agent-authored 文本整成可安全展示的形态:
// 先按 rune 硬截到 maxRawFieldLen, 去控制字符, 再截到 limit。limit ≤ 0 表示只
// 做前两级处理。空串原样返回。
func sanitizeDisplay(s string, limit int) string {
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) > maxRawFieldLen {
		s = truncateRunes(s, maxRawFieldLen)
	}
	s = stripControl(s)
	if limit > 0 {
		s = truncateRunes(s, limit)
	}
	return s
}

// truncateRunes 按 rune 截断 (JSON 里按字符展示, 按 byte 截会切碎多字节字符)。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// stripControl 去掉 C0 控制字符 (保留 \n \t) 与 DEL。ANSI 转义以 \x1b 开头,
// 在这里整类消掉; \r 单独禁止 (回卷攻击)。
func stripControl(s string) string {
	if strings.IndexFunc(s, isControlRune) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isControlRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isControlRune(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f
}
