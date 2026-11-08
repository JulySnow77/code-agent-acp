package bridge

import (
	"context"
	"testing"
	"time"
)

// _multica/flow/suspend 的停止组件契约 (T003): 2s stop ACK 之外还要收齐
// 工具终态与进程 reap; 任何组件缺失只能 verdict=unknown, 绝不报 stopped。

// TestFlowSuspendCollectsComponents: 后端 stopProof 齐全时返回 stopped,
// 桥侧不负责的 daemon_writes/side_effects 留空 (daemon 自行合并)。
func TestFlowSuspendCollectsComponents(t *testing.T) {
	h, _, reqs := newTestHub(t)
	stopped := make(chan string, 1)
	h.deps.StopTurn = func(sid string) { stopped <- sid }
	reqs.respond("_zcode/session/stopProof", func(params map[string]any) (map[string]any, error) {
		return map[string]any{
			"turn":      map[string]any{"state": "aborted"},
			"tools":     []any{map[string]any{"name": "write_file", "state": "terminated"}},
			"processes": []any{map[string]any{"pid": 4242, "state": "reaped"}},
			"session":   "closed",
		}, nil
	})

	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	awaited := make(chan flowOutcome, 1)
	go func() { awaited <- h.await(ctx, fi) }()

	resp, _ := h.suspend(map[string]any{"interaction_id": "req-1"})
	if resp["verdict"] != "stopped" {
		t.Fatalf("verdict = %v, want stopped", resp["verdict"])
	}
	comps, _ := resp["components"].(map[string]any)
	if comps["turn"] == nil || comps["tools"] == nil || comps["processes"] == nil {
		t.Errorf("components = %v, want turn/tools/processes 非空", comps)
	}
	if comps["daemon_writes"] != nil || comps["side_effects"] != nil {
		t.Errorf("daemon_writes/side_effects 应留给 daemon: %v", comps)
	}
	select {
	case sid := <-stopped:
		if sid != "zc-s-1" {
			t.Errorf("stopTurn sid = %q, want zc-s-1", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("suspend 没有先停 turn")
	}
	proofs := reqs.of("_zcode/session/stopProof")
	// CLI 侧 session/stopProof 的参数表是 strict 的、键名为 camelCase 的
	// sessionId（复用上游 session/stop 的参数表）；发 session_id 会被拒，
	// 四个组件全拿不到，平台只能把停止证明降级成 unknown。
	if len(proofs) != 1 || proofs[0]["sessionId"] != "zc-s-1" {
		t.Errorf("stopProof 请求 = %v", proofs)
	}
	select {
	case out := <-awaited:
		if out != flowOutcomeSuspended {
			t.Fatalf("await = %v, want suspended", out)
		}
	case <-time.After(time.Second):
		t.Fatal("停泊的调用没有在挂起后返回")
	}
}

// TestFlowSuspendUnknownWhenToolNeverEnds: 模拟工具不结束 (stopProof 拿不到
// 工具终态 → tools=null): 必须 unknown, 且在有界预算内返回, 不无限保活。
func TestFlowSuspendUnknownWhenToolNeverEnds(t *testing.T) {
	h, _, reqs := newTestHub(t)
	h.deps.StopTurn = func(string) {}
	reqs.respond("_zcode/session/stopProof", func(params map[string]any) (map[string]any, error) {
		return map[string]any{
			"turn":      map[string]any{"state": "aborted"},
			"tools":     nil, // 工具永不结束: 后端给不出终态
			"processes": []any{},
			"session":   "closed",
		}, nil
	})

	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, _ := h.suspend(map[string]any{"interaction_id": "req-1"})
	if resp["verdict"] != "unknown" {
		t.Fatalf("verdict = %v, want unknown (工具终态缺失)", resp["verdict"])
	}
	comps, _ := resp["components"].(map[string]any)
	if comps["tools"] != nil {
		t.Errorf("tools = %v, want nil", comps["tools"])
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("suspend 耗时 %s, 超出有界预算", elapsed)
	}
}

// TestFlowSuspendUnknownWhenProofHangs: 后端不回应 stopProof 时按预算截断,
// 返回 unknown 而不是挂死 (资源时限: 挂起报告 30s 有界, 测试用 50ms)。
func TestFlowSuspendUnknownWhenProofHangs(t *testing.T) {
	h, _, reqs := newTestHub(t)
	h.deps.StopTurn = func(string) {}
	reqs.respond("_zcode/session/stopProof", func(params map[string]any) (map[string]any, error) {
		time.Sleep(5 * time.Second)
		return map[string]any{"turn": "aborted"}, nil
	})

	fi := testFlowInteraction()
	if err := h.start(fi); err != nil {
		t.Fatal(err)
	}
	done := make(chan map[string]any, 1)
	go func() { resp, _ := h.suspend(map[string]any{"interaction_id": "req-1"}); done <- resp }()
	select {
	case resp := <-done:
		if resp["verdict"] != "unknown" {
			t.Fatalf("verdict = %v, want unknown (stopProof 超预算)", resp["verdict"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("suspend 没有在有界预算内返回")
	}
}
