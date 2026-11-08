package bridge

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// askSeq 生成问答的请求 id 字符串 (uq_N, 与 client 自己的整数 id 空间天然不冲突)。
var askSeq atomic.Int64

// askDedup 记住本 turn 内每个 zcode requestId 已经回过的应答, 用来吸收 zcode 的
// **重发**。
//
// zcode 的 requestClient 在等人应答时会按 reannounceIntervalMs 重发同一个请求
// (requestUserInput 走 uRn 拿到 1000ms, 之后翻倍, 上限 10s)。重发用的是**新的**
// JSON-RPC id (server-N), 但 method 与 params 完全一致 —— 其中的 requestId 是
// 同一次交互的注册键, 跨重发不变。所以光看 id 会把一次提问当成两次, 人被问两遍。
//
// 更糟的是第二份答案到不了模型: zcode 的 resolveClientRequest 按 id 查
// pendingClientRequests, 而第一条被应答时 cleanupClientRequest 会把这个 f 上
// 登记的**所有** id 一起注销, 后到的应答查无记录, 静默丢弃。所以第二遍不只是
// 烦人, 是用户的选择被吞掉。
//
// 命中在飞记录时直接把上一份应答重放给新 id, 不再问客户端第二次。
//
// 生命周期跟着 turn 走 (runTurn 里新建), 所以不会跨 turn 串味 —— zcode 的
// requestId 只在一次交互内有效, 跨 turn 复用同一个键去重会误杀真实的新提问。
type askDedup struct {
	mu      sync.Mutex
	replies map[string]map[string]any // zcode requestId → 已回的应答
}

func newAskDedup() *askDedup {
	return &askDedup{replies: map[string]map[string]any{}}
}

// key 取这次提问的去重键。requestId 是交互注册键, 最稳; 缺了退到 toolCallId
// (同样跨重发不变)。两个都没有就没有可靠的键 —— 此时宁可多问一次, 也不能把
// 两道**不同**的题误认成一道。
func askDedupKey(req *zcode.ServerRequest) string {
	return firstNonEmpty(
		zcode.Str(req.Params, "requestId"),
		zcode.Str(req.Params, "toolCallId"),
	)
}

// seen 查这道题是不是已经答过了。命中时第二个返回值是上一份应答, 拿来重放。
func (d *askDedup) seen(key string) (map[string]any, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.replies[key]
	return r, ok
}

func (d *askDedup) put(key string, reply map[string]any) {
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.replies[key] = reply
}

// askCancelOptionID 是降级路径里「取消」这一项的 optionId。
//
// 它必须**不可能**与某个 zcode 选项的 value 撞车 —— 因为 answerFromPermission 现在
// 只认投放过的 optionId, 撞车就等于把一个真实选项解释成取消。zcode 的
// zcodeUserInputOptionSchema 对 value 只要求非空字符串 (理论上任何字符串都合法),
// 所以这里不能只靠前缀「看起来不像」: askOptions 会显式跳过与它同值的 zcode 选项
// 并记日志, 撞车在构造期就被消掉了。
const askCancelOptionID = "\x00zcode:ask-cancel"

// handleUserInput 把 zcode 的 interaction/requestUserInput 转成 ACP 问答并
// 答复 zcode。
//
// 两条路 (T03):
//   - 客户端声明了 `elicitation.form` → 一条 elicitation/create 问完整组问题,
//     带回结构化答案。普通问答**不再伪装成工具授权**。
//   - 没声明 → 退回逐题 session/request_permission, 但收紧成「只接受投放过的
//     optionId」: 自由文本与 JSON 数组一律不认 (旧实现把任意 optionId 当自由文本
//     透传, 等于允许把任意文本解释成用户的选择)。
//
// 任何一条路上拿不到答案都回 cancel, 绝不让 zcode 挂死等待。
//
// 返回值: 问答总耗时 (调用方据此重置 turn 预算 —— 人在回路不占模型 turn 窗口)。
func (br *Bridge) handleUserInput(ctx context.Context, acpSID string, req *zcode.ServerRequest, dedup *askDedup) time.Duration {
	start := time.Now()

	// zcode 会重发没等到应答的提问 (见 askDedup)。重发的那条已经有答案了, 直接
	// 重放, 不要再问客户端一遍 —— 人被问第二次已经够烦, 而且第二份答案还会被
	// zcode 静默丢弃 (见 askDedup 的注释)。
	key := askDedupKey(req)
	if key != "" {
		if prev, ok := dedup.seen(key); ok {
			br.logger("  [ask] 同一提问的重发 (requestId=%s, rpcId=%s), 重放已有答案, 不再问客户端",
				key, req.ID)
			_ = br.backend.Respond(req.ID, prev, nil)
			return time.Since(start)
		}
	}

	// 回 zcode 的唯一出口。记进 dedup, 于是重发的那条能拿到同一份应答。
	respond := func(result map[string]any) {
		dedup.put(key, result)
		_ = br.backend.Respond(req.ID, result, nil)
	}

	questions := filterQuestionDicts(req.Params["questions"])
	if len(questions) == 0 {
		br.logger("  [ask] 无问题, 回 cancel (rpcId=%s)", req.ID)
		respond(map[string]any{"action": "cancel", "reason": "no questions in request"})
		return time.Since(start)
	}
	br.logger("  [ask] %d 个问题待用户回答 (rpcId=%s)", len(questions), req.ID)

	// T003: flow-questions v1 优先于 elicitation —— daemon 声明了 flowQuestions
	// 就不再发 elicitation/create, 改发 _multica/flow/question 并停泊原生调用。
	// 挂起收尾不回 zcode (调用留在停泊表等 resume), 其余按既有 accept/cancel。
	if br.supportsFlowQuestions() {
		outcome, answers, fi := br.flowAskFull(ctx, acpSID, req)
		switch outcome {
		case flowOutcomeAnswered:
			respond(map[string]any{
				"action":  "accept",
				"content": map[string]any{"answers": answers},
			})
			br.flow.markConsumedLive(fi)
			return time.Since(start)
		case flowOutcomeSuspended:
			br.logger("  [ask] 交互挂起 (rpcId=%s), 不回 zcode, 等待恢复路径", req.ID)
			return time.Since(start)
		default:
			br.logger("  [ask] flow 路径未取得答案, 回 cancel (rpcId=%s)", req.ID)
			respond(map[string]any{
				"action": "cancel",
				"reason": "the flow interaction did not produce an answer",
			})
			return time.Since(start)
		}
	}

	if br.supportsElicitationForm() {
		br.logger("  [ask] 客户端声明了 elicitation.form, 走 elicitation/create")
		answers, ok := br.askViaElicitation(ctx, acpSID, req)
		if !ok {
			br.logger("  [ask] elicitation 未拿到完整答案, 向 zcode 回 cancel (rpcId=%s)", req.ID)
			respond(map[string]any{
				"action": "cancel",
				"reason": "the client did not provide a complete, valid answer set",
			})
			return time.Since(start)
		}
		br.logger("  [ask] 收到全部答案, 回 accept")
		respond(map[string]any{
			"action":  "accept",
			"content": map[string]any{"answers": answers},
		})
		return time.Since(start)
	}

	// 降级路径的前置检查。**先全部检查完再问任何一题** —— 问了三题才发现第四题
	// 表达不了, 用户已经答了三个白答, 而且那三个答案被整组丢弃。
	for i, q := range questions {
		if boolOf(q, "multiSelect") {
			br.logger("  [ask] 降级路径无法表达第 %d 题的多选, 一题都不问就回 cancel", i+1)
			respond(map[string]any{
				"action": "cancel",
				"reason": "the client does not support structured elicitation, and this question needs multiple selections",
			})
			return time.Since(start)
		}
		if len(askOptions(q)) <= 1 {
			br.logger("  [ask] 降级路径下第 %d 题没有可投放的选项, 一题都不问就回 cancel", i+1)
			respond(map[string]any{
				"action": "cancel",
				"reason": "a question in this request has no selectable option",
			})
			return time.Since(start)
		}
	}

	answers := map[string]any{}
	for _, q := range questions {
		answer := br.askOne(ctx, acpSID, q, req.Params)
		if answer == nil {
			br.logger("  [ask] 问题未拿到答案, 向 zcode 回 cancel (rpcId=%s)", req.ID)
			respond(map[string]any{"action": "cancel", "reason": "the client did not answer"})
			return time.Since(start)
		}
		answers[zcode.Str(q, "question")] = answer
	}
	br.logger("  [ask] 收到全部答案, 回 accept")
	respond(map[string]any{
		"action":  "accept",
		"content": map[string]any{"answers": answers},
	})
	return time.Since(start)
}

// askOne 发一条 session/request_permission 问一题, 阻塞等答案 (Question 超时)。
//
// 降级路径专用。返回该选项的 **value**; nil = 取消/拒绝/超时/客户端错误/非法应答。
func (br *Bridge) askOne(ctx context.Context, acpSID string, question, payload map[string]any) any {
	id := fmt.Sprintf("uq_%d", askSeq.Add(1))
	options := askOptions(question)
	if len(options) <= 1 { // 只剩取消项: 无合法选项, 无法问
		br.logger("  [ask] %s 问题无合法选项, 当作取消", id)
		return nil
	}

	// 超时上限由 askClient 统一施加; 取消时它会替客户端收尾 (回 cancelled)。
	var reply permOutcome
	rerr := br.askClient(ctx, map[string]any{
		"sessionId": acpSID,
		"toolCall": map[string]any{
			"toolCallId": firstNonEmpty(
				zcode.Str(payload, "toolCallId"),
				zcode.Str(payload, "requestId"),
				"ask",
			),
			"title":    "AskUserQuestion",
			"kind":     "other",
			"rawInput": map[string]any{"questions": []any{sanitizedQuestionView(question)}},
		},
		"options": options,
	}, &reply)
	if rerr != nil {
		br.logger("  [ask] %s 失败 (%v), 当作取消", id, rerr)
		return nil
	}
	answer, ok := answerFromPermission(reply.Outcome.Outcome, reply.Outcome.OptionID, question)
	if !ok && reply.Outcome.Outcome == "selected" {
		// 这里必须显式记一笔: 客户端回了一个我们没投放过的 optionId。旧实现会把它
		// 当自由文本原样交给 zcode, 等于「客户端说什么就算用户答了什么」。
		br.logger("  [ask] %s 客户端回了未投放的 optionId=%q, 拒绝当答案 (不按自由文本透传)",
			id, reply.Outcome.OptionID)
	}
	return answer
}

// askOptions 从问题构造 ACP 权限选项: 每个 zcode 选项一条 allow_once, 追加一条
// 取消项。
//
// **optionId 取 option.value 而不是 label** —— 这是 T03 修的 plan 审批 bug:
// ZCode 的审批选项是 {label:"Approve", value:"approve"}
// (createExitPlanModeApprovalQuestion, interaction-broker.ts:326), 而
// planApprovalResponseToBrokerResult (:399) 拿答案与常量 "approve" 逐字比对。
// 旧实现 optionId=label 且回传 label, 于是用户点「批准」, zcode 收到 "Approve",
// 判为不是批准, 并把字面量当用户反馈喂回模型。
//
// value 为空、重复或超长的选项不投放 (value 才是应答契约, 重复会让回包无法区分,
// 超长会撑爆 optionId 载荷, 见 sanitize.go); 与 askCancelOptionID 撞车的也不投放
// —— 撞车等于把那个选项解释成取消。label 是展示文本, 过 sanitizeDisplay;
// 选项数过 P0 上限后截断 (取消项不受限, 必须始终在)。
func askOptions(question map[string]any) []map[string]any {
	var options []map[string]any
	seen := map[string]bool{}
	for _, raw := range zcode.Slice(question, "options") {
		if len(options) >= maxOptionsPerQuestion {
			break
		}
		o, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		value := zcode.Str(o, "value")
		if value == "" || seen[value] {
			continue
		}
		if value == askCancelOptionID {
			continue
		}
		if len([]rune(value)) > maxOptionValueLen {
			continue
		}
		seen[value] = true
		options = append(options, map[string]any{
			"optionId": value,
			"name":     firstNonEmpty(sanitizeDisplay(zcode.Str(o, "label"), maxOptionLabelLen), value),
			"kind":     "allow_once",
		})
	}
	options = append(options, map[string]any{
		"optionId": askCancelOptionID, "name": "Cancel", "kind": "reject_once",
	})
	return options
}

// sanitizedQuestionView 把一道 zcode 问题的**展示视图**放进 rawInput: 契约字段
// (value) 原样, 展示字段 (question/header/label) 用 sanitize 后的副本。客户端从
// rawInput 渲染题面, 不能让它成为 agent 塞 ANSI 转义/巨型文本进客户端的通道
// (P0)。原题面文本若被截断, 截断副本只影响展示, 不影响应答契约 (optionId 仍按
// value 匹配, answers 键仍是 zcode 的原文)。
func sanitizedQuestionView(question map[string]any) map[string]any {
	qs := parseAskQuestions([]any{question}, false)
	if len(qs) == 0 {
		return map[string]any{}
	}
	q := qs[0]
	view := map[string]any{
		"question":    q.display,
		"multiSelect": q.multiSelect,
	}
	if q.displayHeader != "" {
		view["header"] = q.displayHeader
	}
	var opts []any
	for _, v := range q.values {
		opts = append(opts, map[string]any{"label": q.labels[v], "value": v})
	}
	if opts != nil {
		view["options"] = opts
	}
	return view
}

// answerFromPermission 从 ACP permission 响应提取答案 (降级路径)。
//
// **只认投放过的 optionId**, 返回它的 value (= optionId 本身):
//   - outcome != "selected" / 空 id / 取消项 → nil
//   - 命中某个选项的 value → 该 value
//   - 其余 (自由文本、JSON 数组、未知 id) → nil + false
//
// 旧实现在最后一条上把任意 optionId 当自由文本原样透传, 并且用 `strings.HasPrefix
// (optionId, "[")` 识别塞进 optionId 的 JSON 数组 (omnigent 私有编码)。两者都已
// 移除: zcode 的 normalizeAnswerValue (interaction-broker.ts:617) 本来就接受数组,
// 不需要这层编码; 而把任意文本解释成答案是授权协议上的漏洞。
func answerFromPermission(outcome, optionID string, question map[string]any) (any, bool) {
	if outcome != "selected" {
		return nil, false
	}
	if optionID == "" || optionID == askCancelOptionID {
		return nil, false
	}
	for _, raw := range zcode.Slice(question, "options") {
		o, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if v := zcode.Str(o, "value"); v != "" && v == optionID {
			return v, true
		}
	}
	return nil, false
}

// filterQuestionDicts 取 questions 里的合法对象项。
func filterQuestionDicts(raw any) []map[string]any {
	items, _ := raw.([]any)
	var out []map[string]any
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
