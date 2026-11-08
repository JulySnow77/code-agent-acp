package bridge

import (
	"context"
	"testing"
	"time"
)

// _multica/flow/receipt 的消费回执契约 (T003): 活路径回执的原生绑定必须与
// 问题通知逐字段相等; 恢复路径按 _zcode/interaction/resume 的实报结果出
// consumed / not_executed, 绝不替模型自报成功。

// TestFlowLiveAnswerEmitsConsumedReceipt: 活调用答复后, 回执 evidence 的
// native_call_ref 与问题通知的四字段原生绑定完全一致。
func TestFlowLiveAnswerEmitsConsumedReceipt(t *testing.T) {
	h, log, _ := newTestHub(t)
	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	awaited := make(chan flowOutcome, 1)
	go func() { awaited <- h.await(ctx, fi) }()

	if resp, _ := h.applyAnswer(map[string]any{
		"interaction_id": "req-1", "dispatch_id": "d-1", "consumption_id": "c-1",
		"answer_revision": "1",
		"content":         map[string]any{"使用哪种存储？": "db", "需要保留哪些约束？": "保留现有执行队列"},
	}); resp["accepted"] != true {
		t.Fatalf("applyAnswer = %v", resp)
	}
	if out := <-awaited; out != flowOutcomeAnswered {
		t.Fatalf("await = %v", out)
	}
	h.markConsumedLive(fi)

	receipts := log.of("_multica/flow/receipt")
	if len(receipts) != 1 {
		t.Fatalf("receipt 发出 %d 条, want 1", len(receipts))
	}
	r := receipts[0]
	if r["outcome"] != "consumed" {
		t.Errorf("outcome = %v, want consumed", r["outcome"])
	}
	if r["dispatch_id"] != "d-1" || r["consumption_id"] != "c-1" || r["answer_revision"] != "1" {
		t.Errorf("回执身份字段 = %v/%v/%v", r["dispatch_id"], r["consumption_id"], r["answer_revision"])
	}
	ev, _ := r["evidence"].(map[string]any)
	gotRef, _ := ev["native_call_ref"].(map[string]any)
	questionRef, _ := log.of("_multica/flow/question")[0]["native_call_ref"].(map[string]any)
	for _, k := range []string{"session_id", "turn_id", "connection_id", "call_id"} {
		if gotRef[k] != questionRef[k] {
			t.Errorf("evidence.native_call_ref[%s] = %v, want %v (与问题通知一致)",
				k, gotRef[k], questionRef[k])
		}
	}
	if ev["session_id"] != questionRef["session_id"] {
		t.Errorf("evidence.session_id = %v, want %v", ev["session_id"], questionRef["session_id"])
	}
}

// TestFlowResumeConsumedEmitsReceipt: 挂起后的答案走 _zcode/interaction/resume;
// 后端实报 consumed:true 时回执的原生绑定取**消费侧** call_id。
func TestFlowResumeConsumedEmitsReceipt(t *testing.T) {
	h, log, reqs := newTestHub(t)
	h.deps.StopTurn = func(string) {}
	reqs.respond("_zcode/session/stopProof", func(map[string]any) (map[string]any, error) {
		return map[string]any{"turn": "aborted", "tools": []any{}, "processes": []any{}, "session": "closed"}, nil
	})
	reqs.respond("_zcode/interaction/resume", func(params map[string]any) (map[string]any, error) {
		if params["interaction_id"] != "req-1" {
			t.Errorf("resume interaction_id = %v, want req-1", params["interaction_id"])
		}
		if params["session_id"] != "zc-s-1" {
			t.Errorf("resume session_id = %v, want zc-s-1", params["session_id"])
		}
		answers, _ := params["answers"].(map[string]any)
		if answers["使用哪种存储？"] != "db" {
			t.Errorf("resume answers = %v, want 投影后的 zcode 答案", answers)
		}
		return map[string]any{"consumed": true, "call_id": "srv-42", "session_id": "zc-s-1"}, nil
	})

	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.suspend(map[string]any{"interaction_id": "req-1"}); resp["verdict"] != "stopped" {
		t.Fatalf("suspend = %v", resp)
	}

	if resp, _ := h.applyAnswer(map[string]any{
		"interaction_id": "req-1", "dispatch_id": "d-1", "consumption_id": "c-1",
		"answer_revision": "1",
		"content":         map[string]any{"使用哪种存储？": "db", "需要保留哪些约束？": "保留现有执行队列"},
	}); resp["accepted"] != true {
		t.Fatalf("resume 后的答案 = %v, want accepted", resp)
	}

	receipts := log.of("_multica/flow/receipt")
	if len(receipts) != 1 {
		t.Fatalf("receipt 发出 %d 条, want 1", len(receipts))
	}
	r := receipts[0]
	if r["outcome"] != "consumed" {
		t.Fatalf("outcome = %v, want consumed", r["outcome"])
	}
	ev, _ := r["evidence"].(map[string]any)
	ref, _ := ev["native_call_ref"].(map[string]any)
	if ref["call_id"] != "srv-42" {
		t.Errorf("回执 call_id = %v, want srv-42 (消费侧调用)", ref["call_id"])
	}
	if ev["session_id"] != "zc-s-1" {
		t.Errorf("回执 session_id = %v, want zc-s-1", ev["session_id"])
	}
}

// TestFlowResumeNotExecuted: 后端实报 consumed:false 时只发 not_executed,
// 不得伪装成已消费。
func TestFlowResumeNotExecuted(t *testing.T) {
	h, log, reqs := newTestHub(t)
	h.deps.StopTurn = func(string) {}
	reqs.respond("_zcode/session/stopProof", func(map[string]any) (map[string]any, error) {
		return map[string]any{"turn": "aborted", "tools": []any{}, "processes": []any{}, "session": "closed"}, nil
	})
	reqs.respond("_zcode/interaction/resume", func(map[string]any) (map[string]any, error) {
		return map[string]any{"consumed": false, "call_id": "", "session_id": "zc-s-1"}, nil
	})

	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	_, _ = h.suspend(map[string]any{"interaction_id": "req-1"})
	_, _ = h.applyAnswer(map[string]any{
		"interaction_id": "req-1", "dispatch_id": "d-1", "consumption_id": "c-1",
		"answer_revision": "1",
		"content":         map[string]any{"使用哪种存储？": "db", "需要保留哪些约束？": "保留现有执行队列"},
	})

	receipts := log.of("_multica/flow/receipt")
	if len(receipts) != 1 {
		t.Fatalf("receipt 发出 %d 条, want 1", len(receipts))
	}
	if receipts[0]["outcome"] != "not_executed" {
		t.Fatalf("outcome = %v, want not_executed", receipts[0]["outcome"])
	}
}

// TestFlowResumeRefusesInvalidContent: 恢复路径同样过 readAnswer 校验 ——
// 答案非法时不调用后端 resume, 也不发 consumed 回执。
func TestFlowResumeRefusesInvalidContent(t *testing.T) {
	h, log, reqs := newTestHub(t)
	h.deps.StopTurn = func(string) {}
	reqs.respond("_zcode/session/stopProof", func(map[string]any) (map[string]any, error) {
		return map[string]any{"turn": "aborted", "tools": []any{}, "processes": []any{}, "session": "closed"}, nil
	})

	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	_, _ = h.suspend(map[string]any{"interaction_id": "req-1"})

	resp, _ := h.applyAnswer(map[string]any{
		"interaction_id": "req-1", "dispatch_id": "d-1", "consumption_id": "c-1",
		"answer_revision": "1",
		// 单选答案不在选项集内且未声明 allowCustomText → 非法。
		"content": map[string]any{"使用哪种存储？": "对象存储"},
	})
	if resp["accepted"] == true {
		t.Fatal("非法内容不应被接受")
	}
	if n := len(reqs.of("_zcode/interaction/resume")); n != 0 {
		t.Errorf("非法内容不应触发 resume, got %d 次", n)
	}
	if n := len(log.of("_multica/flow/receipt")); n != 0 {
		t.Errorf("非法内容不应产生回执, got %d 条", n)
	}
	_ = time.Second
}
