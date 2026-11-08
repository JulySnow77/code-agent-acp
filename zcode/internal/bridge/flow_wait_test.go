package bridge

import (
	"context"
	"sync"
	"testing"
	"time"
)

// flow-questions v1 的等待路径 (T003): 声明了 flowQuestions 的 daemon 不再收到
// elicitation/create, 取而代之 _multica/flow/question 通知 + 原生调用就地停泊,
// 答案经 _multica/flow/answer 回填, 任何次序下都只消费一次。

// newTestHub 构造带捕获假件的 flowHub。
func newTestHub(t *testing.T) (h *flowHub, log *flowNotifyLog, requests *flowRequestLog) {
	t.Helper()
	log = &flowNotifyLog{}
	requests = &flowRequestLog{}
	h = newFlowHub(flowHubDeps{
		Notify: func(method string, params map[string]any) error {
			log.record(method, params)
			return nil
		},
		Request: func(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
			return requests.handle(ctx, method, params)
		},
		StopTurn:      func(string) {},
		Logger:        func(string, ...any) {},
		SuspendBudget: 50 * time.Millisecond,
	})
	return h, log, requests
}

type flowNotifyEntry struct {
	Method string
	Params map[string]any
}

type flowNotifyLog struct {
	mu      sync.Mutex
	entries []flowNotifyEntry
}

func (l *flowNotifyLog) record(method string, params map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, flowNotifyEntry{method, params})
}

func (l *flowNotifyLog) of(method string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, e := range l.entries {
		if e.Method == method {
			out = append(out, e.Params)
		}
	}
	return out
}

// flowRequestLog 记录桥→后端请求, 并允许测试逐方法挂响应。
type flowRequestLog struct {
	mu        sync.Mutex
	calls     []flowNotifyEntry
	responses map[string]func(params map[string]any) (map[string]any, error)
}

func (l *flowRequestLog) handle(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	l.mu.Lock()
	l.calls = append(l.calls, flowNotifyEntry{method, params})
	fn := l.responses[method]
	l.mu.Unlock()
	if fn == nil {
		return nil, &flowErrStub{"backend unreachable"}
	}
	// 与真实 backend.Request 同语义: ctx 截断时按预算失败返回, 不陪挂。
	type outcome struct {
		result map[string]any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := fn(params)
		done <- outcome{result, err}
	}()
	select {
	case out := <-done:
		return out.result, out.err
	case <-ctx.Done():
		return nil, &flowErrStub{"request budget exceeded"}
	}
}

func (l *flowRequestLog) respond(method string, fn func(params map[string]any) (map[string]any, error)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.responses == nil {
		l.responses = map[string]func(params map[string]any) (map[string]any, error){}
	}
	l.responses[method] = fn
}

func (l *flowRequestLog) of(method string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, e := range l.calls {
		if e.Method == method {
			out = append(out, e.Params)
		}
	}
	return out
}

type flowErrStub struct{ msg string }

func (e *flowErrStub) Error() string { return e.msg }

// testFlowQuestions: 一道单选 + 一道文本, 与平台样本同形。
func testFlowQuestions() []askQuestion {
	return parseAskQuestions([]any{
		map[string]any{
			"questionId": "storage",
			"question":   "使用哪种存储？",
			"options": []any{
				map[string]any{"label": "数据库", "value": "db"},
				map[string]any{"label": "任务目录", "value": "files"},
			},
		},
		map[string]any{"questionId": "constraint", "question": "需要保留哪些约束？"},
	}, true)
}

func testFlowInteraction() *flowInteraction {
	return newFlowInteraction("req-1", "zc-s-1", "p-1", "zbconn-1", "srv-1", testFlowQuestions())
}

// TestFlowQuestionNotificationReplacesElicitationWait: 停泊期间发的是
// _multica/flow/question 通知; 答案到达后 await 返回 answered; 交互只消费一次。
func TestFlowQuestionNotificationReplacesElicitationWait(t *testing.T) {
	h, log, _ := newTestHub(t)
	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatalf("start: %v", err)
	}

	questions := log.of("_multica/flow/question")
	if len(questions) != 1 {
		t.Fatalf("_multica/flow/question 发出 %d 条, want 1", len(questions))
	}
	q := questions[0]
	if q["interaction_id"] != "req-1" {
		t.Errorf("interaction_id = %v, want req-1", q["interaction_id"])
	}
	ref, _ := q["native_call_ref"].(map[string]any)
	if ref == nil || ref["session_id"] != "zc-s-1" || ref["call_id"] != "srv-1" ||
		ref["turn_id"] != "p-1" || ref["connection_id"] != "zbconn-1" {
		t.Errorf("native_call_ref = %v, want 四字段原生绑定", ref)
	}
	qs, _ := q["questions"].([]any)
	if len(qs) != 2 {
		t.Fatalf("questions = %d, want 2", len(qs))
	}
	first, _ := qs[0].(map[string]any)
	if first["question_id"] != "storage" || first["kind"] != "single" {
		t.Errorf("第一题 = %v, want question_id=storage kind=single", first)
	}
	second, _ := qs[1].(map[string]any)
	if second["kind"] != "text" {
		t.Errorf("第二题 kind = %v, want text", second["kind"])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	awaited := make(chan flowOutcome, 1)
	go func() { awaited <- h.await(ctx, fi) }()

	resp, _ := h.applyAnswer(map[string]any{
		"interaction_id":  "req-1",
		"dispatch_id":     "d-1",
		"consumption_id":  "c-1",
		"answer_revision": "1",
		"content": map[string]any{
			"使用哪种存储？":   "db",
			"需要保留哪些约束？": "保留现有执行队列",
		},
	})
	if resp["accepted"] != true {
		t.Fatalf("applyAnswer = %v, want accepted=true", resp)
	}

	select {
	case out := <-awaited:
		if out != flowOutcomeAnswered {
			t.Fatalf("await = %v, want answered", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await 没有在答案到达后返回")
	}

	// 同一交互的第二份答案不得二次消费 (无论次序如何)。
	second2, _ := h.applyAnswer(map[string]any{
		"interaction_id": "req-1", "dispatch_id": "d-2", "consumption_id": "c-2",
		"answer_revision": "2", "content": map[string]any{"使用哪种存储？": "files"},
	})
	if second2["accepted"] == true {
		t.Error("第二份答案不应被接受 (只消费一次)")
	}

	// 未知交互的答案直接拒绝。
	if resp, _ := h.applyAnswer(map[string]any{
		"interaction_id": "nope", "content": map[string]any{},
	}); resp["accepted"] == true {
		t.Error("未知 interaction_id 不应被接受")
	}
}

// TestFlowAnswerConcurrentWithSuspendConsumesOnce: 答案先到、挂起随后 (以及反序)
// 都只消费一次 —— 活调用回填与停止后 resume 是互斥路径。
func TestFlowAnswerConcurrentWithSuspendConsumesOnce(t *testing.T) {
	h, _, _ := newTestHub(t)
	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	awaited := make(chan flowOutcome, 1)
	go func() { awaited <- h.await(ctx, fi) }()

	// 答案先到: 交互进入 answered (已被活路径领取), suspend 只能停在停泊侧。
	if resp, _ := h.applyAnswer(map[string]any{
		"interaction_id": "req-1", "dispatch_id": "d-1", "consumption_id": "c-1",
		"answer_revision": "1",
		"content":         map[string]any{"使用哪种存储？": "db", "需要保留哪些约束？": "保留现有执行队列"},
	}); resp["accepted"] != true {
		t.Fatalf("首份答案 = %v, want accepted", resp)
	}
	<-awaited

	// 挂起在已消费交互上不产生第二条消费路径。
	susp, _ := h.suspend(map[string]any{"interaction_id": "req-1"})
	if susp["verdict"] != "unknown" && susp["verdict"] != "stopped" {
		t.Fatalf("suspend verdict = %v", susp["verdict"])
	}
	if resp, _ := h.applyAnswer(map[string]any{
		"interaction_id": "req-1", "dispatch_id": "d-9", "consumption_id": "c-9",
		"answer_revision": "9", "content": map[string]any{"使用哪种存储？": "files"},
	}); resp["accepted"] == true {
		t.Error("挂起后的重复答案不应再走活路径或 resume 路径 (已消费)")
	}
}

// TestFlowAnswerAcceptsThePlatformAnswerArray pins the cross-repo contract:
// the daemon sends the platform answer array (one entry per question, keyed by
// the stable question id), and the bridge projects it onto the zcode-native
// answer keys it knows. The native key form is a bridge-internal detail, not
// the wire contract.
//
// Without this projection the bridge sees no answer at all and refuses the
// whole round, so the platform's saved answer would never reach the agent.
func TestFlowAnswerAcceptsThePlatformAnswerArray(t *testing.T) {
	h, _, _ := newTestHub(t)
	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatalf("start: %v", err)
	}

	resp, _ := h.applyAnswer(map[string]any{
		"interaction_id":  "req-1",
		"dispatch_id":     "d-1",
		"consumption_id":  "c-1",
		"answer_revision": "1",
		"answers": []any{
			map[string]any{"question_id": "storage", "selected_option_ids": []any{"db"}},
			map[string]any{"question_id": "constraint", "text": "保留现有执行队列"},
		},
	})
	accepted, _ := resp["accepted"].(bool)
	if !accepted {
		t.Fatalf("applyAnswer = %v, want accepted=true", resp)
	}

	projected, ok := h.projectAnswers(fi, map[string]any{
		"使用哪种存储？":   "db",
		"需要保留哪些约束？": "保留现有执行队列",
	})
	if !ok {
		t.Fatal("投影后的答案未通过校验")
	}
	if projected["使用哪种存储？"] != "db" {
		t.Errorf("投影答案 = %v, want 使用哪种存储？=db", projected)
	}
}

// TestFlowAnswerRejectsAnOutOfRangeOption pins the guard: an option that is
// not part of the question never reaches zcode — the whole round is refused.
func TestFlowAnswerRejectsAnOutOfRangeOption(t *testing.T) {
	h, _, _ := newTestHub(t)
	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatalf("start: %v", err)
	}
	resp, _ := h.applyAnswer(map[string]any{
		"interaction_id": "req-1",
		"answers": []any{
			map[string]any{"question_id": "storage", "selected_option_ids": []any{"not-an-option"}},
			map[string]any{"question_id": "constraint", "text": "保留现有执行队列"},
		},
	})
	accepted, _ := resp["accepted"].(bool)
	if accepted {
		t.Fatalf("applyAnswer = %v, want refused", resp)
	}
	if fi.getState() != flowStateInvalidate {
		t.Errorf("状态 = %q, want invalidated", fi.getState())
	}
}
