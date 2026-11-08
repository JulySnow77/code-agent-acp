package zcode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBin 返回上游 Python fake app-server 的路径 (PORTING_PLAN §7: Phase 1~3
// 复用上游 fake 驱动真实子进程链路)。fake 无 python3 或不可执行时跳过,
// 而不是让 CI 挂在一个环境问题上。
func fakeBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join("..", "..", "test", "fakepy", "zcode_app_server")
	info, err := os.Stat(bin)
	if err != nil {
		t.Skipf("缺少 fake app-server: %v", err)
	}
	if info.Mode()&0111 == 0 {
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Skipf("fake 无执行权限且无法修复: %v", err)
		}
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("系统无 python3, 跳过 fake 驱动的测试")
	}
	return bin
}

func startFakeBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := Start(Options{
		Bin:          fakeBin(t),
		Env:          os.Environ(),
		ProbeTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("启动 fake backend: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func mustRequest(t *testing.T, b *Backend, id, method string, params any) *Response {
	t.Helper()
	resp, err := b.RequestWithTimeout(id, method, params, 15*time.Second)
	if err != nil {
		t.Fatalf("%s 请求失败: %v", method, err)
	}
	return resp
}

func createFakeSession(t *testing.T, b *Backend) string {
	t.Helper()
	resp := mustRequest(t, b, "t-create", "session/create", map[string]any{
		"workspace": map[string]any{"workspacePath": "/tmp", "workspaceKey": "tmp"},
	})
	sid := Str(Sub(resp.ResultMap(), "session"), "sessionId")
	if sid == "" {
		t.Fatalf("session/create 未返回 sessionId: %s", string(resp.Result))
	}
	return sid
}

// nextEvent 等待指定 type 的事件, 中途的其他事件跳过 (fake 的 turn 里
// started/completed 与我们断言的目标交错出现)。
func nextEvent(t *testing.T, l *Listener, want string) Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-l.Events:
			if ev.Kind() == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("等待事件 %s 超时 (seq=%d)", want, l.Seq())
		}
	}
}

// TestFakeTextTurn 走完整链路: create → subscribe → send 文本 turn,
// 断言事件按序到达且 seq 单调。这是 backend 子进程 + JSON Lines + pending 表
// + 事件路由的最小闭环 (Phase 1 验收)。
func TestFakeTextTurn(t *testing.T) {
	b := startFakeBackend(t)
	sid := createFakeSession(t, b)

	mustRequest(t, b, "t-sub", "session/subscribe", map[string]any{
		"sessionId": sid, "deliveryKind": "desktop-continuous", "includeSnapshot": true, "afterSeq": 0,
	})

	l := NewListener(sid)
	b.RegisterListener(l)
	defer b.UnregisterListener(sid)

	resp := mustRequest(t, b, "t-send", "session/send", map[string]any{
		"sessionId": sid, "content": "hello",
	})
	if accepted, _ := resp.ResultMap()["accepted"].(bool); !accepted {
		t.Fatalf("session/send 未被接受: %s", string(resp.Result))
	}

	started := nextEvent(t, l, "turn.started")
	if started.PayloadStr("prompt") != "hello" {
		t.Errorf("turn.started prompt = %q, want %q", started.PayloadStr("prompt"), "hello")
	}
	delta := nextEvent(t, l, "model.streaming")
	if delta.PayloadStr("kind") != "text_delta" || delta.PayloadStr("text") == "" {
		t.Errorf("model.streaming 载荷异常: %v", delta.Payload)
	}
	completed := nextEvent(t, l, "turn.completed")
	if completed.PayloadStr("resultType") != "success" {
		t.Errorf("turn.completed resultType = %q, want success", completed.PayloadStr("resultType"))
	}
	if !(started.Seq < delta.Seq && delta.Seq < completed.Seq) {
		t.Errorf("seq 不单调: started=%d delta=%d completed=%d", started.Seq, delta.Seq, completed.Seq)
	}
	if l.Seq() < completed.Seq {
		t.Errorf("Listener.Seq()=%d 落后于最后事件 %d", l.Seq(), completed.Seq)
	}
}

// TestFakeAskUserInputRoundTrip 验证 server→client 反向调用的投递与应答回流:
// fake 发 interaction/requestUserInput 并阻塞等应答, 收到才推 turn.completed。
// 应答没有真正写回子进程 stdin 时, fake 会在 ANSWER_TIMEOUT 后改推 turn.failed,
// 测试将在 nextEvent(turn.completed) 处超时 —— 即失败可被观测 (上游 fake 设计)。
func TestFakeAskUserInputRoundTrip(t *testing.T) {
	b := startFakeBackend(t)
	sid := createFakeSession(t, b)

	mustRequest(t, b, "t-sub", "session/subscribe", map[string]any{
		"sessionId": sid, "deliveryKind": "desktop-continuous", "includeSnapshot": true, "afterSeq": 0,
	})
	l := NewListener(sid)
	b.RegisterListener(l)
	defer b.UnregisterListener(sid)

	mustRequest(t, b, "t-send", "session/send", map[string]any{
		"sessionId": sid, "content": "ASK_USER_QUESTION",
	})
	nextEvent(t, l, "turn.started")

	var req *ServerRequest
	select {
	case req = <-l.Requests:
	case <-time.After(10 * time.Second):
		t.Fatal("未收到 interaction/requestUserInput")
	}
	if req.Method != "interaction/requestUserInput" {
		t.Fatalf("反向调用方法 = %s", req.Method)
	}
	if Str(req.Params, "toolName") != "AskUserQuestion" {
		t.Errorf("toolName = %q, want AskUserQuestion", Str(req.Params, "toolName"))
	}
	if len(Slice(req.Params, "questions")) == 0 {
		t.Errorf("questions 为空: %v", req.Params)
	}

	if err := b.Respond(req.ID, map[string]any{
		"action":  "accept",
		"content": map[string]any{"answers": []any{"React"}},
	}, nil); err != nil {
		t.Fatalf("应答回写失败: %v", err)
	}

	completed := nextEvent(t, l, "turn.completed")
	if completed.PayloadStr("resultType") != "success" {
		t.Fatalf("问答后 turn 未成功结束: %v", completed.Payload)
	}

	observed := b.ObservedServerMethods()
	if observed["interaction/requestUserInput"] < 1 {
		t.Errorf("ObservedServerMethods 未记录 interaction/requestUserInput: %v", observed)
	}
}

// TestFakeProbeLegacy 验证协议探测路径: fake (≤0.15 行为面) 对旧信封
// workspace/readState 回显 probe id 的错误响应 → 判 legacy 且有实锤。
func TestFakeProbeLegacy(t *testing.T) {
	b := startFakeBackend(t)
	if !b.WaitProbe(15 * time.Second) {
		t.Fatal("协议探测超时")
	}
	if got := b.Mode(); got != ProtocolLegacy {
		t.Errorf("Mode() = %s, want legacy", got)
	}
	if !b.Confirmed() {
		t.Error("探测有响应却标记为未确认")
	}
}

// TestBackendExitFailsPending 验证 §5.4 的失败面: 子进程退出后, 所有 pending
// 请求立即失败并带上退出码, 而不是永远挂起 (上游把子进程 stderr 丢 DEVNULL,
// C1 的启动错误因此被吞)。
func TestBackendExitFailsPending(t *testing.T) {
	if _, err := exec.LookPath("false"); err != nil {
		t.Skip("系统无 /bin/false")
	}
	b, err := Start(Options{Bin: "false"})
	if err != nil {
		t.Fatalf("启动: %v", err)
	}
	defer b.Close()

	deadline := time.Now().Add(10 * time.Second)
	for !b.Dead() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !b.Dead() {
		t.Fatal("子进程未退出")
	}

	// 已死后发新请求: 立即失败。
	if _, err := b.RequestWithTimeout("t1", "session/create", nil, 5*time.Second); err == nil {
		t.Error("后端已死, 新请求应立即失败")
	}

	detail := b.ExitDetail()
	if !strings.Contains(detail, "code 1") {
		t.Errorf("ExitDetail 缺退出码: %q", detail)
	}
}

// TestStderrRing 验证 stderr 环形缓冲的容量淘汰。
func TestStderrRing(t *testing.T) {
	r := newStderrRing(3)
	for _, ln := range []string{"a", "b", "c", "d", "e"} {
		_, _ = r.Write([]byte(ln + "\n"))
	}
	got := r.tail(10)
	want := []string{"c", "d", "e"}
	if len(got) != len(want) {
		t.Fatalf("tail = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tail = %v, want %v", got, want)
		}
	}
	if n := len(newStderrRing(2).tail(5)); n != 0 {
		t.Errorf("空 ring tail = %d, want 0", n)
	}
}

// TestMergeStatePatch 验证 state.updated 的合并与 FIFO 淘汰 (maxSessionStates)。
func TestMergeStatePatch(t *testing.T) {
	b := &Backend{
		state:    map[string]map[string]any{},
		stateSeq: nil,
	}
	b.mergeStatePatch(map[string]any{"params": map[string]any{
		"sessionId": "s1", "patch": map[string]any{"status": "idle"},
	}})
	b.mergeStatePatch(map[string]any{"params": map[string]any{
		"sessionId": "s1", "patch": map[string]any{"todos": []any{1, 2}},
	}})
	p := b.Projection("s1")
	if p == nil || Str(p, "status") != "idle" {
		t.Fatalf("合并结果异常: %v", p)
	}
	if len(Slice(p, "todos")) != 2 {
		t.Errorf("第二轮 patch 未合并: %v", p)
	}

	b.ResetProjection("s1")
	if b.Projection("s1") != nil {
		t.Error("ResetProjection 未清掉投影")
	}

	// 超上限淘汰最旧 (maxSessionStates=64)。
	for i := 0; i <= maxSessionStates; i++ {
		b.mergeStatePatch(map[string]any{"params": map[string]any{
			"sessionId": fmt.Sprintf("bulk-%d", i), "patch": map[string]any{"i": i},
		}})
	}
	if len(b.state) > maxSessionStates {
		t.Errorf("state 条目 %d 超上限 %d", len(b.state), maxSessionStates)
	}
}

// TestEnhanceRegistry 验证 prompt/enhance 异步 job 的等待注册与清理。
func TestEnhanceRegistry(t *testing.T) {
	b := &Backend{enhance: map[string]chan map[string]any{}}
	ch, err := b.RegisterEnhance("job-1")
	if err != nil {
		t.Fatalf("注册: %v", err)
	}
	if _, err := b.RegisterEnhance("job-1"); err == nil {
		t.Error("重复注册应报错")
	}

	b.routeEnhanceResult(map[string]any{"params": map[string]any{
		"requestId": "job-1", "status": "completed", "text": "enhanced",
	}})
	select {
	case got := <-ch:
		if Str(got, "status") != "completed" {
			t.Errorf("enhance result 异常: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("result 未投递到等待通道")
	}

	// 无等待者的 result 不 panic、不留残留。
	b.routeEnhanceResult(map[string]any{"params": map[string]any{
		"requestId": "nobody", "status": "completed",
	}})
	b.UnregisterEnhance("job-1")
	if len(b.enhance) != 0 {
		t.Errorf("enhance 表未清空: %v", b.enhance)
	}
}
