package e2e

import (
	"testing"
	"time"
)

// AC11 的桥端到端消费回执: flow 客户端收到问题通知 → _multica/flow/answer →
// zcode 收到 accept → 客户端收到 consumed 回执, 回执的原生绑定与问题通知
// 逐字段一致。

// waitAgentNotification 等一条 agent→client 通知 (无 id 帧)。
func waitAgentNotification(t *testing.T, c *acpClient, method string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-c.frames:
			if !ok {
				t.Fatal("serve stdout 已关闭")
			}
			if f["method"] == method && f["id"] == nil {
				params, _ := f["params"].(map[string]any)
				return params
			}
		case <-deadline:
			t.Fatalf("等待通知 %s 超时", method)
		}
	}
}

func TestFlowReceiptLiveConsumptionE2E(t *testing.T) {
	f, c, done := startAskCaps(t, askScript(flowStorageQuestion, flowConstraintQuestion), flowQuestionsCaps())

	question := waitAgentNotification(t, c, "_multica/flow/question", 10*time.Second)
	if question["interaction_id"] == "" {
		t.Fatalf("interaction_id 缺失: %v", question)
	}
	nativeRef, _ := question["native_call_ref"].(map[string]any)
	if nativeRef == nil || nativeRef["call_id"] == "" || nativeRef["session_id"] == "" {
		t.Fatalf("native_call_ref 缺字段: %v", nativeRef)
	}
	qs, _ := question["questions"].([]any)
	if len(qs) != 2 {
		t.Fatalf("questions = %d, want 2", len(qs))
	}

	// 整个问答期间不应再出现 elicitation/create —— flow 协商取代了它。
	assertNoAgentRequest(t, c, "elicitation/create", 2*time.Second)

	resp := c.request(4, "_multica/flow/answer", map[string]any{
		"interaction_id":  question["interaction_id"],
		"dispatch_id":     "d-e2e-1",
		"consumption_id":  "c-e2e-1",
		"answer_revision": "1",
		"content": map[string]any{
			"使用哪种存储？":   "db",
			"需要保留哪些约束？": "保留现有执行队列",
		},
	})
	if resp["error"] != nil {
		t.Fatalf("_multica/flow/answer 报错: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	if result["accepted"] != true {
		t.Fatalf("answer 结果 = %v, want accepted=true", result)
	}

	waitPromptOK(t, done)
	replies := askReplies(t, f, "interaction/requestUserInput", 1)
	answers := askAnswers(t, replies[0])
	if answers["使用哪种存储？"] != "db" || answers["需要保留哪些约束？"] != "保留现有执行队列" {
		t.Errorf("zcode answers = %v", answers)
	}

	// flow 连接的 session/create 必须带 _meta.multica.flowQuestions 声明 ——
	// zcode 侧据此关闭 v4 自动回答并启用持久交互。
	for _, entry := range f.loggedRequests(t) {
		if entry["method"] != "session/create" {
			continue
		}
		params, _ := entry["params"].(map[string]any)
		meta, _ := params["_meta"].(map[string]any)
		multica, _ := meta["multica"].(map[string]any)
		if multica["flowQuestions"] != "v1" {
			t.Errorf("session/create _meta.multica = %v, want flowQuestions=v1", params["_meta"])
		}
		break
	}

	receipt := waitAgentNotification(t, c, "_multica/flow/receipt", 5*time.Second)
	if receipt["outcome"] != "consumed" {
		t.Fatalf("receipt outcome = %v, want consumed", receipt["outcome"])
	}
	if receipt["dispatch_id"] != "d-e2e-1" || receipt["consumption_id"] != "c-e2e-1" ||
		receipt["answer_revision"] != "1" {
		t.Errorf("回执身份字段 = %v", receipt)
	}
	ev, _ := receipt["evidence"].(map[string]any)
	gotRef, _ := ev["native_call_ref"].(map[string]any)
	for _, k := range []string{"session_id", "turn_id", "connection_id", "call_id"} {
		if gotRef[k] != nativeRef[k] {
			t.Errorf("回执 native_call_ref[%s] = %v, want %v (与问题通知一致)", k, gotRef[k], nativeRef[k])
		}
	}
}
