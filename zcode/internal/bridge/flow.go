package bridge

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// flow-questions v1 的等待、受控挂起与消费回执 (T003)。
//
// 契约来源: JulyBoard 仓 docs/julyboard/flow-question-protocol.md。桥在
// flowQuestions=v1 协商成功后:
//   - interaction/requestUserInput 不再走本地 elicitation/create, 改发
//     _multica/flow/question 通知, 原生调用就地停泊 (无用户期限 —— 用户等待
//     不占 Question 10m 预算, compat 的该超时只管非 flow 路径);
//   - daemon 的 _multica/flow/answer 就地回填停泊调用 (活路径) 或经
//     _zcode/interaction/resume 恢复 (挂起后路径), 两条路径互斥, 只消费一次;
//   - _multica/flow/suspend 在 2s stop ACK 之外收集后端四类停止组件, 有界
//     30s, 任何组件缺失只能 unknown, 绝不报 stopped;
//   - 消费结果经 _multica/flow/receipt 如实上报, 后端实报什么就发什么。

// flowOutcome 是停泊调用的收尾方式。
type flowOutcome int

const (
	// flowOutcomeAnswered: 活路径拿到答案, 调用方应回 accept。
	flowOutcomeAnswered flowOutcome = iota
	// flowOutcomeCancelled: 等待被取消 (session/cancel 等), 调用方按既有
	// 语义回 cancel。
	flowOutcomeCancelled
	// flowOutcomeSuspended: 受控挂起完成, 调用方**不**答复 zcode —— 原生
	// 调用留在停泊表, 答案到达时走 _zcode/interaction/resume。
	flowOutcomeSuspended
)

// 交互状态 (桥侧停泊表, 与平台交互状态是两回事; 平台状态由 C 仓持久层拥有)。
const (
	flowStatePending    = "pending"   // 已发问题通知, 原生调用停泊在活路径
	flowStateAnswered   = "answered"  // 活路径已领取答案 (消费回执待调用方确认)
	flowStateSuspended  = "suspended" // 受控挂起完成, 等答案走 resume
	flowStateConsumed   = "consumed"  // 已按实报发出 consumed 回执
	flowStateNotExec    = "not_executed"
	flowStateUnknown    = "unknown"
	flowStateInvalidate = "invalidated" // 取消/非法内容, 后续答案一律拒绝
)

// flowHubDeps 是 flowHub 的注入面 (生产由 Bridge 提供, 测试给假件)。
type flowHubDeps struct {
	Notify        func(method string, params map[string]any) error
	Request       func(ctx context.Context, method string, params map[string]any) (map[string]any, error)
	StopTurn      func(zcodeSID string)
	Logger        func(format string, args ...any)
	SuspendBudget time.Duration // 受控挂起总预算, 默认 30s
}

// flowInteraction 是一次停泊的问答: 原生调用身份 + 状态机。
type flowInteraction struct {
	id        string // interaction_id (= zcode requestId, 可跨进程持久)
	zcodeSID  string
	promptID  string
	connID    string
	callID    string
	questions []askQuestion

	mu             sync.Mutex
	state          string
	content        map[string]any // daemon 交付的原始 content
	dispatchID     string
	consumptionID  string
	answerRevision string

	answered  chan struct{} // 关闭 = 活路径已领取答案
	suspended chan struct{} // 关闭 = 受控挂起完成, 停泊调用应返回
}

func newFlowInteraction(id, zcodeSID, promptID, connID, callID string, questions []askQuestion) *flowInteraction {
	return &flowInteraction{
		id:        id,
		zcodeSID:  zcodeSID,
		promptID:  promptID,
		connID:    connID,
		callID:    callID,
		questions: questions,
		state:     flowStatePending,
		answered:  make(chan struct{}),
		suspended: make(chan struct{}),
	}
}

// nativeRef 是这题的原生调用绑定 —— 问题通知与活路径回执必须逐字段一致,
// daemon 据此建立 dispatch 与原生调用的绑定。
func (fi *flowInteraction) nativeRef() map[string]any {
	return map[string]any{
		"session_id":    fi.zcodeSID,
		"turn_id":       fi.promptID,
		"connection_id": fi.connID,
		"call_id":       fi.callID,
	}
}

func (fi *flowInteraction) getState() string {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return fi.state
}

func (fi *flowInteraction) setState(state string) {
	fi.mu.Lock()
	fi.state = state
	fi.mu.Unlock()
}

// flowHub 管理全部停泊交互与 _multica/flow/* 的收发。
type flowHub struct {
	deps flowHubDeps

	mu           sync.Mutex
	interactions map[string]*flowInteraction
}

func newFlowHub(deps flowHubDeps) *flowHub {
	if deps.SuspendBudget <= 0 {
		deps.SuspendBudget = 30 * time.Second
	}
	if deps.Logger == nil {
		deps.Logger = func(string, ...any) {}
	}
	return &flowHub{
		deps:         deps,
		interactions: map[string]*flowInteraction{},
	}
}

// platformQuestions 把桥侧问题投影成平台问题信封的题目形状 (与跨仓契约文档
// 一致: question_id/kind/prompt/required/allow_empty/options/allow_custom_text/
// min_selections/max_selections)。
func platformQuestions(questions []askQuestion) []any {
	out := make([]any, 0, len(questions))
	for _, q := range questions {
		entry := map[string]any{
			"question_id": q.id,
			"kind":        q.kind,
			"prompt":      q.display,
			"required":    true, // 桥侧 required 列表与平台必答语义一致 (文本/单选)
			"allow_empty": false,
		}
		if len(q.values) > 0 {
			opts := make([]any, 0, len(q.values))
			for _, v := range q.values {
				opts = append(opts, map[string]any{"id": v, "label": q.labels[v]})
			}
			entry["options"] = opts
		}
		if q.allowCustomText {
			entry["allow_custom_text"] = true
		}
		if q.minSelections != nil {
			entry["min_selections"] = *q.minSelections
		}
		if q.maxSelections != nil {
			entry["max_selections"] = *q.maxSelections
		}
		out = append(out, entry)
	}
	return out
}

// start 登记交互并向 daemon 发 _multica/flow/question 通知。
func (h *flowHub) start(fi *flowInteraction) error {
	h.mu.Lock()
	h.interactions[fi.id] = fi
	h.mu.Unlock()
	return h.deps.Notify("_multica/flow/question", map[string]any{
		"interaction_id":  fi.id,
		"native_call_ref": fi.nativeRef(),
		"questions":       platformQuestions(fi.questions),
	})
}

// await 停泊等待: 答案 / 挂起 / 取消三选一。用户等待无期限 —— 不套 Question
// 预算; ctx 只承载取消 (interactCtx)。
func (h *flowHub) await(ctx context.Context, fi *flowInteraction) flowOutcome {
	select {
	case <-fi.answered:
		return flowOutcomeAnswered
	case <-fi.suspended:
		return flowOutcomeSuspended
	case <-ctx.Done():
		fi.setState(flowStateInvalidate)
		return flowOutcomeCancelled
	}
}

// answerContent 把 daemon 送来的答案投影成 zcode 原生答案键。
//
// 跨仓契约（docs/julyboard/flow-question-protocol.md「桥↔daemon 线载荷」）：
// daemon 送的是**平台答案数组**——每题一个 {question_id, selected_option_ids,
// text}，选项 id 就是 zcode 的 option value。桥按 question_id 对应回自己的
// 题目，再投影成 zcode 认识的原生键（单选/文本题用题面作键，多选题用
// "题面[value]" 作布尔键）。原生键形态是桥内部实现细节，不是跨仓契约。
//
// 任何一题对不上、选项越界、必答为空，整组拒绝：绝不让 zcode 收到枚举外
// 的答案。
func (h *flowHub) answerContent(fi *flowInteraction, params map[string]any) (map[string]any, bool) {
	raw, present := params["answers"]
	if !present {
		// 原生答案键形态是桥内部实现细节（桥自己的 elicit 路径用），不是
		// 跨仓契约；daemon 一律送平台答案数组。
		content, ok := params["content"].(map[string]any)
		return content, ok
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	byID := make(map[string]map[string]any, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		qid, _ := entry["question_id"].(string)
		if qid == "" {
			return nil, false
		}
		if _, dup := byID[qid]; dup {
			return nil, false
		}
		byID[qid] = entry
	}
	if len(byID) != len(fi.questions) {
		return nil, false
	}
	content := make(map[string]any, len(fi.questions))
	for _, q := range fi.questions {
		entry, ok := byID[q.id]
		if !ok {
			return nil, false
		}
		selected, _ := entry["selected_option_ids"].([]any)
		text, _ := entry["text"].(string)
		if q.multiSelect {
			for _, raw := range selected {
				value, ok := raw.(string)
				if !ok || !q.hasValue(value) {
					return nil, false
				}
				content[q.propName(value)] = true
			}
			if q.allowCustomText && text != "" {
				content[q.key+customTextProp] = text
			}
			continue
		}
		if len(selected) > 1 {
			return nil, false
		}
		if len(selected) == 1 {
			value, ok := selected[0].(string)
			if !ok || !q.hasValue(value) {
				return nil, false
			}
			content[q.key] = value
			continue
		}
		// 文本题 (或允许自由文本的选项题) 走自由文本。
		if text == "" || (q.kind != "text" && !q.allowCustomText) {
			return nil, false
		}
		content[q.key] = text
	}
	return content, true
}

// applyAnswer 处理 _multica/flow/answer: 活路径唤醒停泊调用, 挂起路径走
// _zcode/interaction/resume 并如实发回执。两条路径互斥, 只消费一次。
func (h *flowHub) applyAnswer(params map[string]any) (map[string]any, *jsonrpc.Error) {
	id, _ := params["interaction_id"].(string)
	if id == "" {
		return nil, jsonrpc.Errf(-32602, "interaction_id is required")
	}
	h.mu.Lock()
	fi := h.interactions[id]
	h.mu.Unlock()
	if fi == nil {
		return nil, jsonrpc.Errf(-32604, "unknown interaction %s", id)
	}

	content, ok := h.answerContent(fi, params)
	if !ok {
		// 载荷不合法与投影校验失败同义: 整组拒绝, 交互作废, 后续任何答案
		// 都不得再消费这一轮。
		fi.setState(flowStateInvalidate)
		h.deps.Logger("  [flow] 交互 %s 的答案载荷不合法, 拒绝", id)
		return map[string]any{"accepted": false, "interaction_id": id,
			"reason": "answer content failed question validation"}, nil
	}
	if !h.claimAnswer(fi, params, content) {
		return map[string]any{"accepted": false, "interaction_id": id,
			"reason": "interaction is not waiting for an answer"}, nil
	}

	// 校验投影: 活路径与 resume 路径共用 readAnswer, 非法内容整组拒绝,
	// 绝不让 zcode 收到枚举外的答案。
	answers, ok := h.projectAnswers(fi, content)
	if !ok {
		fi.setState(flowStateInvalidate)
		h.deps.Logger("  [flow] 交互 %s 的答案未通过投影校验, 拒绝", id)
		return map[string]any{"accepted": false, "interaction_id": id,
			"reason": "answer content failed question validation"}, nil
	}

	state := fi.getState()
	switch state {
	case flowStateAnswered:
		// 活路径: 唤醒停泊调用, 由调用方回 accept 后 markConsumedLive 出回执。
		close(fi.answered)
		return map[string]any{"accepted": true, "interaction_id": id, "dispatch_id": fi.dispatchID}, nil
	case flowStateSuspended:
		// 挂起路径: 原生调用已不在进程内, 按持久 interaction_id 恢复。
		h.resume(fi, answers)
		return map[string]any{"accepted": true, "interaction_id": id, "dispatch_id": fi.dispatchID}, nil
	default:
		// claim 之后状态只剩这两种; 防御性拒绝。
		return map[string]any{"accepted": false, "interaction_id": id, "reason": "unexpected state"}, nil
	}
}

// hasValue 报告该题是否含这个选项值。
func (q askQuestion) hasValue(value string) bool {
	for _, v := range q.values {
		if v == value {
			return true
		}
	}
	return false
}

// claimAnswer 在状态机上原子领取答案: 只有 pending (活) 与 suspended (恢复)
// 可领取, 每个交互最多一次。领取时把 dispatch 身份固化到交互上。
func (h *flowHub) claimAnswer(fi *flowInteraction, params, content map[string]any) bool {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	switch fi.state {
	case flowStatePending:
		fi.state = flowStateAnswered
	case flowStateSuspended:
		// 保持 suspended 标记, resume 完成后迁移终态。
	default:
		return false
	}
	fi.content = content
	fi.dispatchID, _ = params["dispatch_id"].(string)
	fi.consumptionID, _ = params["consumption_id"].(string)
	fi.answerRevision, _ = params["answer_revision"].(string)
	return true
}

// projectAnswers 把 daemon 的 content 逐题过 readAnswer, 产出 zcode 形状的
// answers (问题原文为键)。任一题非法则整组拒绝。
func (h *flowHub) projectAnswers(fi *flowInteraction, content map[string]any) (map[string]any, bool) {
	answers := make(map[string]any, len(fi.questions))
	for _, q := range fi.questions {
		if q.question == "" {
			return nil, false
		}
		value, ok := q.readAnswer(content)
		if !ok {
			return nil, false
		}
		answers[q.question] = value
	}
	return answers, true
}

// resume 走 _zcode/interaction/resume 恢复挂起交互, 按后端实报结果发回执。
func (h *flowHub) resume(fi *flowInteraction, answers map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), h.deps.SuspendBudget)
	defer cancel()
	result, err := h.deps.Request(ctx, "_zcode/interaction/resume", map[string]any{
		"session_id":     fi.zcodeSID,
		"interaction_id": fi.id,
		"answers":        answers,
	})
	if err != nil {
		fi.setState(flowStateUnknown)
		h.deps.Logger("  [flow] resume %s 失败 (%v), 交互转 unknown", fi.id, err)
		return
	}
	consumed, _ := result["consumed"].(bool)
	callID, _ := result["call_id"].(string)
	sessionID, _ := result["session_id"].(string)
	if sessionID == "" {
		sessionID = fi.zcodeSID
	}
	if consumed {
		fi.setState(flowStateConsumed)
		h.emitReceipt(fi, "consumed", map[string]any{
			"native_call_ref": map[string]any{
				"session_id":    sessionID,
				"turn_id":       fi.promptID,
				"connection_id": fi.connID,
				"call_id":       callID,
			},
			"session_id": sessionID,
		})
		return
	}
	fi.setState(flowStateNotExec)
	h.emitReceipt(fi, "not_executed", map[string]any{"session_id": sessionID})
}

// markConsumedLive 由活路径调用方在向 zcode 回 accept 之后调用: 回执的原生
// 绑定必须与问题通知完全一致。
func (h *flowHub) markConsumedLive(fi *flowInteraction) {
	if state := fi.getState(); state != flowStateAnswered {
		h.deps.Logger("  [flow] markConsumedLive: 交互 %s 状态 %s 非answered, 忽略", fi.id, state)
		return
	}
	fi.setState(flowStateConsumed)
	h.emitReceipt(fi, "consumed", map[string]any{
		"native_call_ref": fi.nativeRef(),
		"session_id":      fi.zcodeSID,
	})
}

// emitReceipt 发 _multica/flow/receipt 通知。发送失败只记日志 —— 回执是
// 事实上报, 平台侧有查询兜底, 不因通知失败阻断答复路径。
//
// 回执载荷按跨仓契约带齐 receipt_id / observed_at：平台按 receipt_id 做幂等、
// 按 observed_at 落时间戳，缺任一项都会被判成非法载荷。
func (h *flowHub) emitReceipt(fi *flowInteraction, outcome string, evidence map[string]any) {
	if err := h.deps.Notify("_multica/flow/receipt", map[string]any{
		"receipt_id":      newFlowReceiptID(),
		"interaction_id":  fi.id,
		"dispatch_id":     fi.dispatchID,
		"consumption_id":  fi.consumptionID,
		"answer_revision": fi.answerRevision,
		"outcome":         outcome,
		"evidence":        evidence,
		"observed_at":     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		h.deps.Logger("  [flow] 回执通知失败 (interaction=%s outcome=%s): %v", fi.id, outcome, err)
	}
}

// newFlowReceiptID 造一个 v4 UUID（回执幂等键）。桥不引 uuid 依赖：
// crypto/rand + 手工格式化就够，且格式与平台侧一致。
func newFlowReceiptID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 取不到随机数就没有可用的幂等键；时间戳兜底保证载荷合法。
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// suspend 处理 _multica/flow/suspend: 先停 turn (2s ACK), 再向后端要四类
// 停止组件, 总预算有界。turn/tools/processes 任一缺失 → unknown; 桥不负责
// daemon_writes/side_effects (留给 daemon 合并自己的证据)。
func (h *flowHub) suspend(params map[string]any) (map[string]any, *jsonrpc.Error) {
	id, _ := params["interaction_id"].(string)
	if id == "" {
		return nil, jsonrpc.Errf(-32602, "interaction_id is required")
	}
	h.mu.Lock()
	fi := h.interactions[id]
	h.mu.Unlock()
	if fi == nil {
		return nil, jsonrpc.Errf(-32604, "unknown interaction %s", id)
	}

	h.deps.StopTurn(fi.zcodeSID)

	ctx, cancel := context.WithTimeout(context.Background(), h.deps.SuspendBudget)
	defer cancel()
	result, err := h.deps.Request(ctx, "_zcode/session/stopProof", map[string]any{
		// CLI 侧的 session/stopProof 复用上游 session/stop 的参数表
		// （zcodeSessionStopParamsSchema，strict，键名是 camelCase 的
		// sessionId）。发 session_id 会被 strict schema 以
		// 「Unrecognized key: "session_id"」拒掉，四个组件就都拿不到，
		// 平台只能把停止证明降级成 unknown、恢复门继续关着。
		// 注意：CLI 的**入参**是 camelCase，**回参**（turn/tools/processes、
		// session_id）是 snake_case，两边不对称是 CLI 协议自身的约定。
		"sessionId": fi.zcodeSID,
	})
	components := map[string]any{
		"turn":          nil,
		"tools":         nil,
		"processes":     nil,
		"daemon_writes": nil, // daemon 自身写入证据由 daemon 合并
		"side_effects":  nil,
	}
	verdict := "unknown"
	if err == nil && result != nil {
		for _, key := range []string{"turn", "tools", "processes"} {
			if v, present := result[key]; present && v != nil {
				components[key] = v
			}
		}
		if components["turn"] != nil && components["tools"] != nil && components["processes"] != nil {
			verdict = "stopped"
		}
	} else if err != nil {
		h.deps.Logger("  [flow] stopProof 失败 (interaction=%s): %v", id, err)
	}

	// 挂起只对未消费的交互生效; 已消费/失效的交互仍返回停止事实, 但不再
	// 迁移状态 (消费回执已发, 不能复活)。
	fi.mu.Lock()
	switch fi.state {
	case flowStatePending:
		fi.state = flowStateSuspended
		close(fi.suspended)
	}
	fi.mu.Unlock()

	return map[string]any{
		"interaction_id": id,
		"verdict":        verdict,
		"components":     components,
	}, nil
}

// handleFlowAnswer / handleFlowSuspend 是 conn 上的 JSON-RPC 入口 (daemon → 桥)。
func (br *Bridge) handleFlowAnswer(_ context.Context, _ json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p map[string]any
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errf(-32602, "invalid params: %v", err)
	}
	return br.flow.applyAnswer(p)
}

func (br *Bridge) handleFlowSuspend(_ context.Context, _ json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p map[string]any
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errf(-32602, "invalid params: %v", err)
	}
	return br.flow.suspend(p)
}

// flowAskFull 是 handleUserInput 的 flow 分支: 发问题通知并停泊等待。返回
// 收尾方式、投影后的 zcode 答案 (answered 时非空) 与停泊交互 (answered 后由
// 调用方在回 accept 之后 markConsumedLive)。
func (br *Bridge) flowAskFull(ctx context.Context, _ string, req *zcode.ServerRequest) (flowOutcome, map[string]any, *flowInteraction) {
	questions := parseAskQuestions(req.Params["questions"], true)
	if len(questions) == 0 {
		return flowOutcomeCancelled, nil, nil
	}
	for _, q := range questions {
		if len(q.values) == 0 && q.kind != "text" {
			br.logger("  [flow] 问题 %q 无选项且非文本题, 无法投影", q.key)
			return flowOutcomeCancelled, nil, nil
		}
	}

	zcodeSID := zcode.Str(req.Params, "sessionId")
	promptID := br.promptIDFor(zcodeSID)
	interactionID := firstNonEmpty(
		zcode.Str(req.Params, "requestId"),
		"call:"+req.ID, // 无 requestId 时退化为调用内身份, 跨重启不可恢复
	)
	fi := newFlowInteraction(interactionID, zcodeSID, promptID, br.backendConnID, req.ID, questions)
	if err := br.flow.start(fi); err != nil {
		br.logger("  [flow] 问题通知失败 (%v), 回 cancel", err)
		return flowOutcomeCancelled, nil, nil
	}
	br.logger("  [flow] 交互 %s 已停泊 (native call %s), 等待 _multica/flow/answer", interactionID, req.ID)

	switch br.flow.await(ctx, fi) {
	case flowOutcomeAnswered:
		answers, ok := br.flow.projectAnswers(fi, fi.content)
		if !ok {
			// claim 阶段已校验过一次; 这里失败说明竞态改写, 拒绝并失效。
			br.logger("  [flow] 交互 %s 答案二次校验失败, 回 cancel", interactionID)
			return flowOutcomeCancelled, nil, nil
		}
		return flowOutcomeAnswered, answers, fi
	case flowOutcomeSuspended:
		br.logger("  [flow] 交互 %s 已受控挂起, 原生调用停泊待恢复", interactionID)
		return flowOutcomeSuspended, nil, fi
	default:
		return flowOutcomeCancelled, nil, nil
	}
}

// promptIDFor 查 zcode 会话当前 turn 的 promptID (native_ref 的 turn_id)。
func (br *Bridge) promptIDFor(zcodeSID string) string {
	br.mu.Lock()
	defer br.mu.Unlock()
	if turn := br.turns[zcodeSID]; turn != nil {
		return turn.promptID
	}
	return ""
}
