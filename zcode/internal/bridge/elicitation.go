package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// boolOf 取一个 bool 字段, 非 bool 一律当 false (zcode 的 multiSelect 就是这么用的)。
func boolOf(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// ACP v1 的结构化提问 (elicitation) 支持。
//
// 契约事实 (agentclientprotocol.com/protocol/v1/elicitation, 2026-09 核对):
//   - 客户端在 initialize 的 `clientCapabilities.elicitation` 声明。**每个模式必须显式
//     存在且非 null** 才算支持 —— ACP 明确写了这一点与 MCP 的「空对象=仅 form」不同:
//     `{}` 与 `{"form":null,"url":null}` 都表示零模式。
//   - `elicitation/create` 的 form 变体: {sessionId, toolCallId?, mode:"form",
//     message, requestedSchema}。requestedSchema 是**扁平对象**, property 限原始类型
//     与 enum (ElicitationSchema)。
//   - 应答 CreateElicitationResponse: {action: "accept"|"decline"|"cancel", content?}。
//   - 「Agents MUST NOT request a mode the Client has not advertised」。

// clientCaps 是桥从 initialize 里记下的客户端能力。只记现在真正用得上的。
type clientCaps struct {
	// elicitationForm 为真表示客户端显式声明了 `elicitation.form`。
	elicitationForm bool
	// Opt-in extension: cancel abandoned inbound SDK callbacks using their
	// request context, instead of the legacy reverse result frame.
	cancelRequest bool
	// flowQuestions 为真表示客户端 (JulyBoard daemon) 在
	// clientCapabilities._meta.multica 显式声明了 flowQuestions="v1":
	// 无损题型投影、稳定问题 id 与自定义文本开始生效。未声明时本文件
	// 的所有输出与 T03 基线逐字一致。
	flowQuestions bool
}

// elicitationCancelled 是 elicitation/create 的「什么都没答」应答
// (CreateElicitationResponse 的 cancel 变体)。放弃等待时补它, 与 permission 路径
// 的 permCancelled 同一件事、两种说法。
var elicitationCancelled = map[string]any{"action": "cancel"}

// parseClientCaps 解析 initialize 的 clientCapabilities。
//
// 严格按规范: elicitation 必须是对象, 且 `form` 键存在且值非 null。缺省、`{}`、
// `{"form":null}`、整个 `elicitation` 为 null, 一律视为不支持。
func parseClientCaps(params json.RawMessage) clientCaps {
	var p struct {
		ClientCapabilities struct {
			Elicitation map[string]json.RawMessage `json:"elicitation"`
			Meta        struct {
				Zcode struct {
					CancelRequest bool `json:"cancelRequest"`
				} `json:"_zcode"`
				Multica struct {
					FlowQuestions string `json:"flowQuestions"`
				} `json:"multica"`
			} `json:"_meta"`
		} `json:"clientCapabilities"`
	}
	if json.Unmarshal(params, &p) != nil {
		return clientCaps{}
	}
	el := p.ClientCapabilities.Elicitation["form"]
	return clientCaps{
		elicitationForm: !isJSONNull(el),
		cancelRequest:   p.ClientCapabilities.Meta.Zcode.CancelRequest,
		flowQuestions:   p.ClientCapabilities.Meta.Multica.FlowQuestions == "v1",
	}
}

// supportsFlowQuestions 读 flow 问答协商结果 (initialize 之后才有意义)。
func (br *Bridge) supportsFlowQuestions() bool {
	br.mu.Lock()
	defer br.mu.Unlock()
	return br.clientCaps.flowQuestions
}

// flowQuestionsMeta 是 initialize 结果 _meta.multica 的 Flow 问答能力声明,
// 与跨仓契约 (JulyBoard docs/julyboard/flow-question-protocol.md) 逐字一致。
// suspendProof/consumptionReceipt 两个方法由 flow.go (Task 3) 注册, 声明与
// 方法同批交付, daemon 只在自身也声明 v1 时才启用这条路由。
func flowQuestionsMeta() map[string]any {
	return map[string]any{
		"flowQuestions":      "v1",
		"stableQuestionId":   true,
		"freeText":           true,
		"multiSelect":        map[string]any{"minSelections": true},
		"suspendProof":       "components",
		"consumptionReceipt": "native-call",
	}
}

// isJSONNull 判断一个 raw 是不是 JSON null。规范里 `form: null` 与缺省等价, 都算不支持。
func isJSONNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// supportsElicitationForm 读当前客户端能力 (initialize 之后才有意义)。
func (br *Bridge) supportsElicitationForm() bool {
	br.mu.Lock()
	defer br.mu.Unlock()
	return br.clientCaps.elicitationForm
}

// askQuestion 是 zcode 一道题经过校验后的内部表示。
//
// question 与 values 是**应答契约** (回 zcode 的答案键/取值, 见 sanitize.go 的
// 红线: 绝不截断清洗); display/displayHeader/labels/previews 是**展示副本**
// (已按 P0 规则截断、去控制字符), 只进标题/描述/message/_meta。
type askQuestion struct {
	key           string // 投影到 requestedSchema 的 property 名
	question      string // zcode 的问题原文 (回 answers 时作为键, 不清洗)
	display       string // question 的展示副本 (sanitizeDisplay)
	header        string // zcode 的小标题原文
	displayHeader string // header 的展示副本
	multiSelect   bool
	values        []string // option.value, 顺序与 zcode 一致
	labels        map[string]string
	previews      map[string]string // option.preview: 展开预览, 表单里放不下

	// 以下字段只在 flow-questions v1 协商成功时由 parseAskQuestions 填充;
	// 非 flow 路径全部零值, 输出与基线逐字一致。
	kind            string // single | multi | text
	allowCustomText bool   // 单选/多选允许自由文本 (与选项互斥)
	minSelections   *int   // multi 下界 (仅在 meta 携带, form schema 表达不了)
	maxSelections   *int   // multi 上界
	id              string // 稳定问题 id (服务端提供则透传, 否则派生)
}

// customTextProp 是 allowCustomText 题在 requestedSchema 里的自由文本
// property 名。zcode 侧的 value 空间不可能出现 `[__custom__]` 形态的值
// (它是我们合成的前缀), 与 boolean 展开天然不撞车。
const customTextProp = "[__custom__]"

// questionKindOf 判定平台题型: multiSelect → multi; 无选项 → text; 其余 single。
func questionKindOf(multiSelect bool, values []string) string {
	switch {
	case multiSelect:
		return "multi"
	case len(values) == 0:
		return "text"
	default:
		return "single"
	}
}

// deriveQuestionID 生成稳定问题 id: sha256(问题原文 + "\n" + 选项 value 列表)
// 的十六进制前 16 位。同题重报 (reannounce 或新 requestId 复问) 得到同一 id,
// 换任一选项 value 即变化。碰撞时追加 -2/-3… 保持请求内唯一 (平台短串允许)。
func deriveQuestionID(question string, values []string) string {
	sum := sha256.Sum256([]byte(question + "\n" + strings.Join(values, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// intPtrOf 取一个数字字段为 *int (flow 专用)。JSON 解码路径给 float64,
// 进程内构造 (测试/直调) 给 int/int64 —— 两种都收, 其余形态 nil。
func intPtrOf(m map[string]any, k string) *int {
	switch v := m[k].(type) {
	case float64:
		i := int(v)
		return &i
	case int:
		i := v
		return &i
	case int64:
		i := int(v)
		return &i
	default:
		return nil
	}
}

// parseAskQuestions 解析 zcode 的 questions[] 并生成 requestedSchema。
//
// property 名的选择: 优先用问题原文 (zcode 的 normalizeAskUserQuestionAnswers 首先按它取值,
// 见 interaction-broker.ts:485), 空、本请求内重复、或**超长**时退化为 `answer_<i>` ——
// 那两条回退路径 zcode 同样认 (content[`answer_${index}`])。超长必须退: 原文会变成
// requestedSchema 的 property 名连 required[] 一起发给客户端, 多 KB 的 JSON 键就是
// 资源攻击 (P0)。answers 仍按原文回键, zcode 按原文查找, 契约不受影响。
func parseAskQuestions(raw any, flow bool) []askQuestion {
	qs := filterQuestionDicts(raw)
	out := make([]askQuestion, 0, len(qs))
	used := map[string]bool{}
	usedIDs := map[string]bool{} // flow: 同请求内派生 id 去重
	for i, q := range qs {
		question := zcode.Str(q, "question")
		key := question
		if key == "" || used[key] || len([]rune(key)) > maxQuestionLen {
			key = "answer_" + strconv.Itoa(i)
		}
		used[key] = true

		aq := askQuestion{
			key:           key,
			question:      question,
			display:       sanitizeDisplay(question, maxQuestionLen),
			header:        zcode.Str(q, "header"),
			displayHeader: sanitizeDisplay(zcode.Str(q, "header"), maxOptionLabelLen),
			multiSelect:   boolOf(q, "multiSelect"),
			labels:        map[string]string{},
			previews:      map[string]string{},
		}
		seen := map[string]bool{}
		for _, rawOpt := range zcode.Slice(q, "options") {
			if len(aq.values) >= maxOptionsPerQuestion {
				break // P0: 选项数上限 (wmux), 防巨型表单
			}
			o, ok := rawOpt.(map[string]any)
			if !ok {
				continue
			}
			// value 才是应答契约 (label 只是给人看的, 且 plan 审批里
			// label "Approve" ≠ value "approve")。value 缺失/重复/超长的选项不投放
			// (超长 value 撑爆 enum 载荷与多选 property 名, 见 sanitize.go)。
			value := zcode.Str(o, "value")
			if value == "" || seen[value] || len([]rune(value)) > maxOptionValueLen {
				continue
			}
			seen[value] = true
			aq.values = append(aq.values, value)
			aq.labels[value] = firstNonEmpty(sanitizeDisplay(zcode.Str(o, "label"), maxOptionLabelLen), value)
			if pv := sanitizeDisplay(zcode.Str(o, "preview"), maxRawFieldLen); pv != "" {
				aq.previews[value] = pv
			}
		}
		if flow {
			// flow-questions v1: 题型、边界与稳定 id。id 优先透传服务端提供的
			// questionId; 派生 id 在请求内撞车时追加 -N 保持唯一。
			aq.kind = questionKindOf(aq.multiSelect, aq.values)
			aq.allowCustomText = boolOf(q, "allowCustomText") && aq.kind != "text"
			aq.minSelections = intPtrOf(q, "minSelections")
			aq.maxSelections = intPtrOf(q, "maxSelections")
			// 平台契约要求 multi 题必须带 min/max_selections（缺任一即以
			// multi_without_bounds 拒收整份信封），而 CLI 的 askUserQuestion
			// schema 只有 multiSelect、没有边界字段。所以边界由桥在投影时补：
			// 缺下界按 1，缺上界按选项数，并夹紧到 1 <= min <= max <= 选项数。
			// 不补的话每一轮真实多选提问都会被平台拒掉，交互悬在桥里等不到答案。
			if aq.kind == "multi" && len(aq.values) > 0 {
				if aq.minSelections == nil {
					lower := 1
					aq.minSelections = &lower
				}
				if aq.maxSelections == nil {
					upper := len(aq.values)
					aq.maxSelections = &upper
				}
				if *aq.maxSelections > len(aq.values) {
					*aq.maxSelections = len(aq.values)
				}
				if *aq.minSelections < 1 {
					*aq.minSelections = 1
				}
				if *aq.minSelections > *aq.maxSelections {
					*aq.minSelections = *aq.maxSelections
				}
			}
			base := zcode.Str(q, "questionId")
			if base == "" {
				base = deriveQuestionID(aq.question, aq.values)
			}
			id := base
			for n := 2; usedIDs[id]; n++ {
				id = base + "-" + strconv.Itoa(n)
			}
			aq.id = id
			usedIDs[id] = true
			// text 题没有选项集, 互斥/边界字段不适用。
			if aq.kind == "text" {
				aq.allowCustomText = false
				aq.minSelections, aq.maxSelections = nil, nil
			}
		}
		out = append(out, aq)
	}
	return out
}

// schemaProp 是 requestedSchema 里的一条 property 定义。
type schemaProp struct {
	Name string
	Def  map[string]any
}

// propName 是某个选项在 requestedSchema 里的 property 名。
//
// 单选: 就是 key (问题原文), 与 zcode 取答案的键一致。多选: `key[value]` ——
// 一个选项一个 boolean property, 见 properties 的说明。
func (q askQuestion) propName(value string) string {
	if !q.multiSelect {
		return q.key
	}
	return q.key + "[" + value + "]"
}

// properties 把一道题展开成 requestedSchema 的 property 定义。
//
// **多选为什么不用 `type: "array"`**: ACP 的 form 模式只接受**原始类型**与
// enum —— protocol/v1/elicitation.md:101 "Form schemas are flat objects whose
// properties use the supported primitive and enum schemas", schema.md 的
// ElicitationSchema 也写着 "Property definitions (must be primitive types)"。
// 数组两者都不是, 严格的客户端会按 -32602 拒掉整个请求, 于是**每一个**多选题都被
// 悄悄降级。改成「一个选项一个 boolean」: 合法、无歧义, 勾选语义也与 checkbox
// 一致 (缺省 = 不选)。
func (q askQuestion) properties() []schemaProp {
	title := firstNonEmpty(q.displayHeader, q.display)
	if q.kind == "text" {
		// flow 模式的文本题: 没有选项集, 一条 string property 即全部。
		return []schemaProp{{q.key, map[string]any{
			"type":        "string",
			"title":       title,
			"description": q.display,
		}}}
	}
	if !q.multiSelect {
		enum := make([]any, 0, len(q.values))
		for _, v := range q.values {
			enum = append(enum, v)
		}
		return []schemaProp{{q.key, map[string]any{
			"type":        "string",
			"title":       title,
			"description": q.display,
			"enum":        enum,
		}}}
	}
	out := make([]schemaProp, 0, len(q.values))
	for _, v := range q.values {
		out = append(out, schemaProp{q.propName(v), map[string]any{
			"type":        "boolean",
			"title":       q.labels[v],
			"description": q.display,
		}})
	}
	if q.allowCustomText {
		out = append(out, schemaProp{q.key + customTextProp, map[string]any{
			"type":        "string",
			"title":       title,
			"description": q.display,
		}})
	}
	return out
}

// elicitationSchema 合成整份 requestedSchema (扁平对象)。
//
// required 只列**单选**题: 多选的 boolean 不列 —— 强制客户端为每个未勾选项回一个
// false 是不合理的要求, 而且省略即「不选」正是 checkbox 的常规语义。
func elicitationSchema(questions []askQuestion) map[string]any {
	props := make(map[string]any, len(questions))
	required := make([]any, 0, len(questions))
	for _, q := range questions {
		if q.multiSelect {
			for _, p := range q.properties() {
				props[p.Name] = p.Def
			}
			continue
		}
		for _, p := range q.properties() {
			props[p.Name] = p.Def
			// 文本题与单选一样是单值 property, 列入 required; 自定义文本
			// (customTextProp) 例外 —— 与选项互斥, 不能强制两边都填。
			if !q.allowCustomText || p.Name != q.key+customTextProp {
				required = append(required, p.Name)
			}
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// elicitationMessage 造人类可读的 message。zcode 的 prompt 是可选的, 缺省用问题原文
// (展示副本)。两者都是 agent-authored, 过 sanitizeDisplay。
func elicitationMessage(params map[string]any, questions []askQuestion) string {
	if prompt := sanitizeDisplay(strings.TrimSpace(zcode.Str(params, "prompt")), maxRawFieldLen); prompt != "" {
		return prompt
	}
	texts := make([]string, 0, len(questions))
	for _, q := range questions {
		if q.display != "" {
			texts = append(texts, q.display)
		}
	}
	return strings.Join(texts, "\n")
}

// readAnswer 从客户端回的 content 里取出这道题的答案并校验。
//
// ACP 要求 agent 再校验一次 (elicitation.md:102 "Agents SHOULD validate them
// again"), 而且这一步不能省: 它是「客户端回什么都不能让 zcode 收到枚举外的答案」
// 的最后一道闸。
//
// 单选: 必须命中 values 之一; flow 模式下声明了 allowCustomText 的题也接受
// 非空自由文本 (与选项互斥: 同一份 content 里两者都出现按枚举优先、纯文本次之,
// 绝不合并)。文本题 (无选项) 收非空字符串。多选: 逐个 boolean property 取,
// 只认 true; 一个都没出现时判为「这题没答」(而不是「一个都没选」) —— 两者不该
// 混同, 后者是要照收的合法答案; allowCustomText 的多选另有 __custom__ 文本
// property, 与勾选互斥, 混填整组拒绝。
func (q askQuestion) readAnswer(content map[string]any) (any, bool) {
	allowed := make(map[string]bool, len(q.values))
	for _, v := range q.values {
		allowed[v] = true
	}
	if !q.multiSelect {
		raw, present := content[q.key]
		if !present {
			return nil, false
		}
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, false
		}
		if allowed[s] {
			return s, true
		}
		// flow: 文本题天然自由文本; 选项题只有声明了 allowCustomText 才收。
		if q.kind == "text" || q.allowCustomText {
			return s, true
		}
		return nil, false
	}
	present := 0
	sel := make([]any, 0, len(q.values))
	for _, v := range q.values {
		raw, has := content[q.propName(v)]
		if !has {
			continue
		}
		b, ok := raw.(bool)
		if !ok {
			return nil, false
		}
		present++
		if b {
			sel = append(sel, v)
		}
	}
	if q.allowCustomText {
		if raw, has := content[q.key+customTextProp]; has {
			custom, ok := raw.(string)
			if !ok {
				return nil, false
			}
			present++
			if strings.TrimSpace(custom) != "" {
				if len(sel) > 0 {
					return nil, false // 勾选与自定义文本互斥
				}
				return custom, true
			}
		}
	}
	if present == 0 {
		return nil, false
	}
	return sel, true
}

// askViaElicitation 用一条 elicitation/create 问完整组问题 (form mode)。
//
// 一次发全部问题而不是逐题一轮: ACP 的 form 模式本来就是一张表, 逐题发等于把
// 客户端的多问题能力退化成单选循环。返回 answers (按 zcode 问题原文为键);
// ok=false 表示放弃 (取消/超时/拒绝/取值非法), 调用方应回 cancel。
//
// 有一题非法就整组放弃, 而不是「丢弃那题、其余照收」: 丢掉一题之后 zcode 拿到的
// answers 是不完整的, 而应答是 accept —— 模型会当成用户确实只答了其余几题, 然后
// 基于缺的那题往下编。整组 cancel 至少是诚实的「没答成」。
func (br *Bridge) askViaElicitation(ctx context.Context, acpSID string, req *zcode.ServerRequest) (map[string]any, bool) {
	flow := br.supportsFlowQuestions()
	questions := parseAskQuestions(req.Params["questions"], flow)
	if len(questions) == 0 {
		br.logger("  [ask] 没有可投影的问题, 放弃 (调用方回 cancel)")
		return nil, false
	}
	// 没有任何选项的题在非 flow 模式投影不出合法 enum —— ACP 的 ElicitationSchema
	// 允许空 enum, 但那样的题用户根本答不了。与其在客户端面前摆一张废表, 不如明确
	// 失败。flow 模式下文本题是一条 string property, 合法; 但多选题仍然必须带选项。
	for _, q := range questions {
		if len(q.values) == 0 && !(flow && q.kind == "text") {
			br.logger("  [ask] 问题 %q 没有可用选项, 无法投影成 elicitation", q.key)
			return nil, false
		}
	}

	params := map[string]any{
		"sessionId":       acpSID,
		"mode":            "form",
		"message":         elicitationMessage(req.Params, questions),
		"requestedSchema": elicitationSchema(questions),
	}
	if toolCallID := zcode.Str(req.Params, "toolCallId"); toolCallID != "" {
		params["toolCallId"] = toolCallID
	}
	meta := map[string]any{"zcode": askRequestMeta(req.Params, questions, flow)}
	if flow {
		meta["multica"] = map[string]any{"questionId": flowQuestionIDMeta(questions)}
	}
	params["_meta"] = meta

	var reply struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
	}
	rerr := br.askClientRaw(ctx, "elicitation/create", params, &reply, elicitationCancelled)
	if rerr != nil {
		br.logger("  [ask] elicitation/create 失败 (%v), 放弃", rerr)
		return nil, false
	}
	switch reply.Action {
	case "accept":
	case "decline", "cancel":
		br.logger("  [ask] 客户端 action=%s", reply.Action)
		return nil, false
	default:
		// 规范里的 `other` 变体 (`_` 前缀为实现私有) 以及任何未知 action: 不能猜。
		br.logger("  [ask] 客户端回了未知 action=%q, 不猜", reply.Action)
		return nil, false
	}

	answers := make(map[string]any, len(questions))
	for _, q := range questions {
		if q.question == "" {
			// 没有问题原文就没法按原文回键; zcode 的 schema 要求 question 非空,
			// 走到这里说明后端发了不合规数据。
			br.logger("  [ask] 问题 %q 无原文, 无法回填 answers, 整组放弃", q.key)
			return nil, false
		}
		value, ok := q.readAnswer(reply.Content)
		if !ok {
			br.logger("  [ask] 客户端对 %q 的应答缺失或不在允许取值内 (content=%v), 整组放弃",
				q.key, reply.Content)
			return nil, false
		}
		answers[q.question] = value
	}
	return answers, true
}

// askRequestMeta 把 ACP 表单装不下的 zcode 事实放进 _meta: 展示名、preview、
// multiSelect、requestId。客户端按 label 渲染、agent 按 value 回传, 两边不打架。
func askRequestMeta(params map[string]any, questions []askQuestion, flow bool) map[string]any {
	opts := make([]any, 0, len(questions))
	for _, q := range questions {
		choices := make([]any, 0, len(q.values))
		for _, v := range q.values {
			entry := map[string]any{"value": v, "label": q.labels[v]}
			if pv := q.previews[v]; pv != "" {
				entry["preview"] = pv
			}
			choices = append(choices, entry)
		}
		entry := map[string]any{
			"question":    q.display,
			"header":      q.displayHeader,
			"multiSelect": q.multiSelect,
			"options":     choices,
		}
		if flow {
			// flow-questions v1: daemon 侧凭 id/kind/bounds 把这题投影成平台
			// 问题信封 (question_id/kind/min_selections/max_selections/
			// allow_custom_text)。
			entry["id"] = q.id
			entry["kind"] = q.kind
			if q.allowCustomText {
				entry["allowCustomText"] = true
			}
			if q.minSelections != nil {
				entry["minSelections"] = *q.minSelections
			}
			if q.maxSelections != nil {
				entry["maxSelections"] = *q.maxSelections
			}
		}
		opts = append(opts, entry)
	}
	meta := map[string]any{
		"requestId": zcode.Str(params, "requestId"),
		"toolName":  zcode.Str(params, "toolName"),
		"questions": opts,
	}
	if origin := zcode.Sub(params, "origin"); origin != nil {
		meta["origin"] = origin
	}
	return meta
}

// flowQuestionIDMeta 构造 _meta.multica.questionId: requestedSchema 的每个
// property 名 → 所属问题的稳定 id。daemon 收到应答 content 后按键直接查到
// question_id, 多选题的全部 boolean property 与自定义文本 property 指向同一 id。
func flowQuestionIDMeta(questions []askQuestion) map[string]any {
	out := map[string]any{}
	for _, q := range questions {
		for _, p := range q.properties() {
			out[p.Name] = q.id
		}
	}
	return out
}
