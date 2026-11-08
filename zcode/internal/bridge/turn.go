package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// turnState 是 turn 循环里跨事件累积的输出标记 (上游 _run_event_turn 的
// emitted_text / emitted_output): 决定完成时的兜底回复与失败检测。
type turnState struct {
	emittedText   bool // 是否流式发过 agent_message_chunk
	emittedOutput bool // 是否发过任何实质输出 (text/tool, 决定失败检测)
}

// stallWindow 是事件流停滞的判定窗口 (上游同值): turn 已启动且超过 15s 无事件
// 时, 用状态投影确认 turn 是否其实已完成 (turn.completed 可能丢失)。
const stallWindow = 15 * time.Second

// runTurn 是 session/prompt 的主体: subscribe → send → 事件循环 → 返回结果。
//
// 这是整个桥的核心: 把 zcode 的异步事件模型转成 ACP 的阻塞请求。
// Go 版只走事件订阅 (0.16+), 不实现轮询降级 (PORTING_PLAN §1.2 D1)。
func (br *Bridge) runTurn(ctx context.Context, s *session, promptID, promptText string) (any, *jsonrpc.Error) {
	// 新 turn 入口清 state.updated 投影, 防跨 turn 陈旧值误导停滞检查 (上游 P1-1)。
	br.backend.ResetProjection(s.zcodeSID)

	// ⚠️ 时序关键 (T02 修): turn 必须**先**登记再 subscribe + send。
	// 旧实现反过来 —— 注册发生在 send 之后, 于是同一会话的第二个 prompt 能在
	// 这个窗口里进来, RegisterListener 对同一 sid 直接覆盖 (只打一条警告),
	// 第一个 turn 的权限请求随即被当成「无在飞 turn」自动 cancel: 用户视角是
	// 第一个 turn 的授权被无故拒绝。
	//
	// P3 起并发 prompt 在 handleSessionPrompt 的闸口排队, 到这里时闸口已保证
	// 本会话没有别的 turn —— 下面的 -32600 是闸口之外的最后一道防线 (防御
	// 深度), 正常路径不再触发。
	interactCtx, abort := context.WithCancel(ctx)
	defer abort()
	turn := &activeTurn{cancel: make(chan struct{}), promptID: promptID, abort: abort}
	br.mu.Lock()
	if prev := br.turns[s.zcodeSID]; prev != nil {
		br.mu.Unlock()
		br.logger("⚠ session/prompt: 会话 %s 已有在飞 turn %s, 拒绝并发 prompt",
			s.zcodeSID, prev.promptID)
		return nil, jsonrpc.Errf(-32600,
			"该会话已有在飞 turn (promptId=%s); zcode 一次只跑一个 turn, 请等它结束或先发 session/cancel",
			prev.promptID)
	}
	br.turns[s.zcodeSID] = turn
	br.mu.Unlock()
	defer func() {
		br.mu.Lock()
		delete(br.turns, s.zcodeSID)
		br.mu.Unlock()
	}()

	// ⚠️ 时序关键: 先 subscribe + 注册 listener, 再 session/send —— 否则 send
	// 与注册之间的 session/event 会被 reader 丢弃, 短 turn 的 turn.completed
	// 丢失直接超时 (上游同注释)。
	l := zcode.NewListener(s.zcodeSID)
	subParams := map[string]any{"sessionId": s.zcodeSID}
	subResp, err := br.request("session/subscribe", subParams)
	if err != nil {
		return nil, jsonrpc.Errf(-32603, "zcode session/subscribe 失败 (0.16+ 必须走事件订阅): %v", rpcMessage(err))
	}
	if proj := zcode.Sub(zcode.Sub(subResp.ResultMap(), "snapshot"), "projection"); proj != nil {
		l.ContextWindow = int(zcode.Num(proj, "contextWindow"))
	}
	br.backend.RegisterListener(l)
	defer br.backend.UnregisterListener(s.zcodeSID)

	sendResp, err := br.request("session/send", map[string]any{
		"sessionId": s.zcodeSID,
		"content":   promptText, // C5: content 必须是 string
	})
	if err != nil {
		return nil, jsonrpc.Errf(-32603, "zcode send failed: %v", rpcMessage(err))
	}
	if accepted, _ := sendResp.ResultMap()["accepted"].(bool); !accepted {
		return nil, jsonrpc.Errf(-32603, "zcode send 未被接受 (无 accepted 响应)")
	}

	// 本 turn 的提问去重表: zcode 会重发没等到应答的 interaction/requestUserInput
	// (新 id、同一个 requestId), 命中就重放上一份答案, 不把同一次提问问两遍。
	// 跟着 turn 走 —— requestId 只在一次交互内有效, 跨 turn 复用同一个键去重会
	// 误杀真实的新提问。见 askDedup。
	askDedup := newAskDedup()

	tr := NewTranslator()
	tr.ContextWindow = l.ContextWindow
	st := turnState{}

	// turn 预算手动管理 (而非 ctx.WithTimeout): 人在回路问答后要重置预算,
	// 一次性的 ctx 做不到 (上游在答复后重置 t0)。turnCtx 只承载连接级取消。
	turnCtx := ctx
	turnBudget := time.Now().Add(br.timeouts.Turn)
	tickBudget := time.NewTicker(time.Second)
	defer tickBudget.Stop()

	tickStall := time.NewTicker(500 * time.Millisecond)
	defer tickStall.Stop()
	tickDead := time.NewTicker(time.Second)
	defer tickDead.Stop()

	var lastEvent time.Time
	var lastStallCheck time.Time

	for {
		// Give cancellation priority over a completion event produced by a
		// declined interaction, so it cannot turn a cancelled prompt into success.
		if turnCtx.Err() != nil {
			br.stopBackendTurn(s.zcodeSID)
			return nil, jsonrpc.Errf(-32603, "session/prompt 被客户端取消")
		}
		select {
		case <-turn.cancel:
			br.stopBackendTurn(s.zcodeSID)
			return br.waitForCancelled(turnCtx, l, tr, s)
		default:
		}
		select {
		case ev := <-l.Events:
			lastEvent = time.Now()
			for _, update := range tr.Translate(ev) {
				switch update["sessionUpdate"] {
				case "agent_message_chunk":
					st.emittedText = true
					st.emittedOutput = true
				case "tool_call":
					st.emittedOutput = true
				}
				if err := br.conn.Notify("session/update", map[string]any{
					"sessionId": s.acpSID,
					"update":    update,
				}); err != nil {
					br.logger("⚠ session/update 发送失败: %v", err)
				}
			}
			if tr.TurnDone {
				return br.finishTurn(turnCtx, s, tr, st)
			}

		case req := <-l.Requests:
			// 人在回路 (Phase 4): AskUserQuestion 与工具权限确认都转发给
			// ACP 客户端; 其余反问仍然立即回 -32601 不挂死。
			//
			// 用 interactCtx 而不是裸 ctx: 取消会掐断它, 于是这里同步等待答案的
			// case body 能立刻返回 (T02)。取消之后循环下一轮命中已关闭的
			// case <-turn.cancel 收尾, 语义不变。
			switch req.Method {
			case "interaction/requestUserInput":
				elapsed := br.handleUserInput(interactCtx, s.acpSID, req, askDedup)
				br.logger("  [ask] 问答耗时 %s", elapsed.Round(time.Millisecond))
				// 人在回路可能耗时很久, 不占模型 turn 预算与静默窗口
				// (上游答复后重置 t0 与 last_event_time)。
				turnBudget = time.Now().Add(br.timeouts.Turn)
				lastEvent = time.Now()
			case "interaction/requestPermission":
				br.handlePermission(interactCtx, s.acpSID, req)
				turnBudget = time.Now().Add(br.timeouts.Turn)
				lastEvent = time.Now()
			case providerRuntimeHeadersMethod:
				// 订阅型 provider 的鉴权询问: 无头桥接必须自己应答, 否则 turn
				// 以 ModelRequestAuthMissing 告终 (见 provider_auth.go)。
				br.handleProviderRuntimeHeaders(req)
				turnBudget = time.Now().Add(br.timeouts.Turn)
				lastEvent = time.Now()
			default:
				br.declineServerRequest(req)
			}

		case <-turn.cancel:
			// 本地标记已被 session/cancel 置位; 补发 session/stop 并收尾。
			br.stopBackendTurn(s.zcodeSID)
			// 给 zcode 一点时间推 turn.completed(cancelled), 但不再等新事件。
			return br.waitForCancelled(turnCtx, l, tr, s)

		case <-turnCtx.Done():
			// 连接级取消 (客户端断开): 立即终止。
			br.stopBackendTurn(s.zcodeSID)
			return nil, jsonrpc.Errf(-32603, "session/prompt 被客户端取消")

		case <-tickBudget.C:
			if time.Now().After(turnBudget) {
				br.stopBackendTurn(s.zcodeSID)
				return nil, jsonrpc.Errf(-32000,
					"turn 超时 (预算 %s, 配置 ZCODE_ACP_TURN_TIMEOUT)", br.timeouts.Turn)
			}

		case <-tickDead.C:
			if br.backend.Dead() {
				return nil, jsonrpc.Errf(-32603, "%s", br.backend.ExitDetail())
			}

		case <-tickStall.C:
			if !tr.TurnStarted || time.Since(lastEvent) < stallWindow || br.backend.Dead() {
				continue
			}
			if !lastStallCheck.IsZero() && time.Since(lastStallCheck) < stallWindow {
				continue
			}
			lastStallCheck = time.Now()
			idle, proj := br.confirmIdle(s)
			if !idle {
				continue
			}
			br.logger("⚠ 事件流停滞但 turn 已 idle (turn.completed 丢失?), 收尾")
			return br.finishStalled(s, tr, st, proj)
		}
	}
}

// finishTurn 处理终止帧 (turn.completed / turn.failed) 的收尾与响应。
func (br *Bridge) finishTurn(ctx context.Context, s *session, tr *Translator, st turnState) (any, *jsonrpc.Error) {
	if tr.TurnFailed {
		return br.failTurn(s, tr, st)
	}
	switch {
	case tr.ResultType == "cancelled":
		return map[string]any{"stopReason": "cancelled"}, nil
	case strings.HasPrefix(tr.ResultType, "error_"):
		return nil, jsonrpc.Errf(-32000, "zcode turn failed: %s", tr.ResultType)
	}
	// 正常完成。纯工具 turn 没有流式文本 → 兜底取最后一条 assistant 回复。
	if !st.emittedText {
		if reply := br.fetchLastReply(ctx, s); reply != "" {
			_ = br.conn.Notify("session/update", map[string]any{
				"sessionId": s.acpSID,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": reply},
				},
			})
		}
	}
	return map[string]any{"stopReason": "end_turn"}, nil
}

// failTurn 是 D4 (错误可见): 推错误文本给客户端 + prompt 返回 JSON-RPC error。
func (br *Bridge) failTurn(s *session, tr *Translator, st turnState) (any, *jsonrpc.Error) {
	text := firstNonEmpty(tr.TurnError, "turn failed")
	_ = br.conn.Notify("session/update", map[string]any{
		"sessionId": s.acpSID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "⚠ " + text},
		},
	})
	data, _ := json.Marshal(map[string]any{
		"resultType":  "error",
		"attribution": tr.Attribution,
	})
	if legacyTurnFailed() {
		// --legacy-turn-failed: 恢复上游「failed 当 end_turn」行为 (§8 D4 备选)。
		br.logger("turn failed (--legacy-turn-failed, 不上报): %s", text)
		return map[string]any{"stopReason": "end_turn"}, nil
	}
	br.logger("session/prompt: turn 失败 (%s)", text)
	return nil, &jsonrpc.Error{Code: -32000, Message: "zcode turn failed: " + text, Data: data}
}

func legacyTurnFailed() bool {
	return strings.TrimSpace(getenv("ZCODE_ACP_LEGACY_TURN_FAILED")) != ""
}

// waitForCancelled 在本地取消后短等 turn.completed(cancelled) 或 turn.failed;
// 等不到也不无限挂 —— 本地已判 cancelled。
func (br *Bridge) waitForCancelled(ctx context.Context, l *zcode.Listener, tr *Translator, s *session) (any, *jsonrpc.Error) {
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-l.Events:
			tr.Translate(ev)
			if tr.TurnDone {
				// 推掉终止帧的翻译产物 (usage 等), 忽略失败语义。
				if tr.TurnFailed {
					return map[string]any{"stopReason": "cancelled"}, nil
				}
				if tr.ResultType == "cancelled" {
					return map[string]any{"stopReason": "cancelled"}, nil
				}
				return map[string]any{"stopReason": "cancelled"}, nil
			}
		case <-deadline:
			return map[string]any{"stopReason": "cancelled"}, nil
		case <-ctx.Done():
			return map[string]any{"stopReason": "cancelled"}, nil
		}
	}
}

// finishStalled 是停滞收尾: 不重放 differ (Go 版无 differ, 会重复发事件),
// 只做兜底回复 + 失败检测 (上游同: 既无终止帧又无实质输出疑似失败)。
func (br *Bridge) finishStalled(s *session, tr *Translator, st turnState, proj map[string]any) (any, *jsonrpc.Error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stallReply := ""
	if !st.emittedText {
		stallReply = br.fetchLastReply(ctx, s)
		if stallReply != "" {
			_ = br.conn.Notify("session/update", map[string]any{
				"sessionId": s.acpSID,
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": stallReply},
				},
			})
		}
	}
	if !st.emittedOutput && stallReply == "" {
		return nil, jsonrpc.Errf(-32603,
			"zcode turn 完成但无输出 (停滞收尾疑似失败; 可能 turn 事件丢失或 provider 错误)")
	}
	return map[string]any{"stopReason": "end_turn"}, nil
}

// confirmIdle 用状态投影 / session/read 权威确认 turn 是否已结束。
func (br *Bridge) confirmIdle(s *session) (bool, map[string]any) {
	proj := br.backend.Projection(s.zcodeSID)
	if proj == nil || zcode.Str(proj, "status") != "idle" {
		resp, err := br.request("session/read", map[string]any{"sessionId": s.zcodeSID})
		if err != nil {
			return false, nil
		}
		proj = zcode.Sub(resp.ResultMap(), "projection")
	}
	return proj != nil && zcode.Str(proj, "status") == "idle", proj
}

// fetchLastReply turn 完成后读 session/messages, 取最后一条 assistant 文本
// (纯工具 turn 的兜底回复)。zcode 在 idle 后有短暂数据一致性延迟, 带 4 次重试
// (上游 _fetch_last_reply)。
func (br *Bridge) fetchLastReply(ctx context.Context, s *session) string {
	for attempt := 0; attempt < 4; attempt++ {
		select {
		case <-ctx.Done():
			return ""
		default:
		}
		resp, err := br.request("session/messages", map[string]any{"sessionId": s.zcodeSID})
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		messages := zcode.Slice(resp.ResultMap(), "messages")
		for i := len(messages) - 1; i >= 0; i-- {
			m, ok := messages[i].(map[string]any)
			if !ok {
				continue
			}
			if zcode.Str(zcode.Sub(m, "info"), "role") != "assistant" {
				continue
			}
			var texts []string
			for _, rawPart := range zcode.Slice(m, "parts") {
				part, ok := rawPart.(map[string]any)
				if !ok || zcode.Str(part, "type") != "text" {
					continue
				}
				if t := zcode.Str(part, "text"); t != "" {
					texts = append(texts, t)
				}
			}
			if text := strings.Join(texts, "\n"); strings.TrimSpace(text) != "" {
				return text
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return ""
}

// declineServerRequest 在 Phase 2 (无人在回路) 拒绝 zcode 的交互反问:
// requestUserInput 回 cancel, requestPermission 回 -32601 (与 backend 无人
// 接收分支一致)。Phase 4 的 ask.go / permission.go 会接管这里。
func (br *Bridge) declineServerRequest(req *zcode.ServerRequest) {
	switch req.Method {
	case "interaction/requestUserInput":
		br.logger("  ← %s 无人在回路 (Phase 4 前), 回 cancel", req.Method)
		_ = br.backend.Respond(req.ID, map[string]any{
			"action": "cancel",
			"reason": "bridge has no interactive client in this build",
		}, nil)
	case providerRuntimeHeadersMethod:
		// 不依赖人在回路: 凭证是本机的, 不该因为 turn 阶段不同而时有时无。
		br.handleProviderRuntimeHeaders(req)
	default:
		br.logger("  ← %s 无人在回路 (Phase 4 前), 回 -32601", req.Method)
		_ = br.backend.Respond(req.ID, nil, &zcode.RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("no interactive turn to handle %s", req.Method),
		})
	}
}

// getenv 抽 os.Getenv, 便于测试替换。
var getenv = os.Getenv
