package client

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"kimi-acp/acp"
	"kimi-acp/agent"
)

type testEnv struct {
	cl *Client

	mu               sync.Mutex
	updates          []acp.SessionUpdate
	permissionCalls  []*acp.RequestPermissionParams
	permissionAnswer acp.RequestPermissionResult
}

func (e *testEnv) addUpdate(u acp.SessionUpdate) {
	e.mu.Lock()
	e.updates = append(e.updates, u)
	e.mu.Unlock()
}

func (e *testEnv) snapshot() []acp.SessionUpdate {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]acp.SessionUpdate(nil), e.updates...)
}

func (e *testEnv) permissionCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.permissionCalls)
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{permissionAnswer: acp.SelectedOutcome("allow-once")}
	aTr, bTr := acp.NewBufferedPair()
	ctx, cancel := context.WithCancel(context.Background())
	ag := agent.New(aTr, agent.NewEcho().Handlers(), agent.Options{})
	go func() { _ = ag.Conn().Run(ctx) }()

	e.cl = Dial(bTr, Options{
		Info: acp.Implementation{Name: "test-client", Version: "0"},
		Capabilities: acp.ClientCapabilities{
			FS:       &acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
		Handlers: HandlerFuncs{
			SessionUpdate: func(n *acp.SessionNotification) { e.addUpdate(n.Update) },
			RequestPermission: func(_ context.Context, p *acp.RequestPermissionParams) (*acp.RequestPermissionResult, *acp.RPCError) {
				e.mu.Lock()
				e.permissionCalls = append(e.permissionCalls, p)
				answer := e.permissionAnswer
				e.mu.Unlock()
				return &answer, nil
			},
			ReadTextFile: func(_ context.Context, p acp.ReadTextFileParams) (*acp.ReadTextFileResult, *acp.RPCError) {
				return &acp.ReadTextFileResult{Content: "file:" + p.Path}, nil
			},
			WriteTextFile: func(_ context.Context, _ acp.WriteTextFileParams) (*acp.EmptyResult, *acp.RPCError) {
				return &acp.EmptyResult{}, nil
			},
			TerminalCreate: func(_ context.Context, _ acp.CreateTerminalParams) (*acp.CreateTerminalResult, *acp.RPCError) {
				return &acp.CreateTerminalResult{TerminalID: "term_test"}, nil
			},
			TerminalOutput: func(_ context.Context, _ acp.TerminalOutputParams) (*acp.TerminalOutputResult, *acp.RPCError) {
				return &acp.TerminalOutputResult{Output: "done", Truncated: false}, nil
			},
			TerminalWaitExit: func(_ context.Context, _ acp.WaitForTerminalExitParams) (*acp.WaitForTerminalExitResult, *acp.RPCError) {
				return &acp.WaitForTerminalExitResult{}, nil
			},
			TerminalKill: func(_ context.Context, _ acp.KillTerminalParams) (*acp.EmptyResult, *acp.RPCError) {
				return &acp.EmptyResult{}, nil
			},
			TerminalRelease: func(_ context.Context, _ acp.ReleaseTerminalParams) (*acp.EmptyResult, *acp.RPCError) {
				return &acp.EmptyResult{}, nil
			},
		},
	})
	t.Cleanup(func() { e.cl.Close(); ag.Close(); cancel() })
	return e
}

func testCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func TestInitializeNegotiatesV1(t *testing.T) {
	e := newTestEnv(t)
	ctx, cancel := testCtx(t)
	defer cancel()
	res, err := e.cl.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProtocolVersion != acp.ProtocolVersionV1 || res.AgentInfo == nil || res.AgentInfo.Name != "kimi-acp-go-echo" {
		t.Fatalf("unexpected initialize result: %#v", res)
	}
	if !res.AgentCapabilities.CanLoadSession() || !res.AgentCapabilities.CanFork() || !res.AgentCapabilities.CanLogout() {
		t.Fatalf("missing capabilities: %#v", res.AgentCapabilities)
	}
	if e.cl.InitializeResult() == nil {
		t.Fatal("InitializeResult was not stored")
	}
}

func TestFullSessionLifecycle(t *testing.T) {
	e := newTestEnv(t)
	ctx, cancel := testCtx(t)
	defer cancel()
	if _, err := e.cl.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	ns, err := e.cl.NewSession(ctx, acp.NewSessionParams{CWD: t.TempDir()})
	if err != nil || ns.SessionID == "" {
		t.Fatalf("session/new: %v %#v", err, ns)
	}
	result, err := e.cl.Prompt(ctx, ns.SessionID, []acp.ContentBlock{acp.TextBlock("hello 世界")})
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != acp.StopEndTurn {
		t.Fatalf("stopReason = %q", result.StopReason)
	}
	var text strings.Builder
	chunks := 0
	for _, u := range e.snapshot() {
		if u.Type() == acp.UpdateAgentMessageChunk {
			text.WriteString(u.Text())
			chunks++
		}
	}
	if chunks < 2 || text.String() != "echo: hello 世界" {
		t.Fatalf("streamed %d chunks, text=%q", chunks, text.String())
	}

	before := len(e.snapshot())
	if _, err := e.cl.LoadSession(ctx, acp.LoadSessionParams{SessionID: ns.SessionID, CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if len(e.snapshot()) <= before {
		t.Fatal("session/load did not replay history")
	}
	if _, err := e.cl.ResumeSession(ctx, acp.ResumeSessionParams{SessionID: ns.SessionID, CWD: t.TempDir()}); err != nil {
		t.Fatalf("session/resume: %v", err)
	}
	list, err := e.cl.ListSessions(ctx, acp.ListSessionsParams{})
	if err != nil || len(list.Sessions) != 1 {
		t.Fatalf("session/list: %v %#v", err, list)
	}
	if err := e.cl.DeleteSession(ctx, ns.SessionID); err != nil {
		t.Fatal(err)
	}
	list, err = e.cl.ListSessions(ctx, acp.ListSessionsParams{})
	if err != nil || len(list.Sessions) != 0 {
		t.Fatalf("session/delete did not remove: %v %#v", err, list)
	}
}

func TestPromptReverseCalls(t *testing.T) {
	e := newTestEnv(t)
	ctx, cancel := testCtx(t)
	defer cancel()
	_, _ = e.cl.Initialize(ctx)
	ns, err := e.cl.NewSession(ctx, acp.NewSessionParams{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cl.Prompt(ctx, ns.SessionID, []acp.ContentBlock{acp.TextBlock("please run a tool and ask permission")}); err != nil {
		t.Fatal(err)
	}
	if e.permissionCount() != 1 {
		t.Fatalf("expected one permission request, got %d", e.permissionCount())
	}
	seenTool, seenUpdate := false, false
	for _, u := range e.snapshot() {
		if u.Type() == acp.UpdateToolCall {
			seenTool = true
		}
		if u.Type() == acp.UpdateToolCallUpdate && u.ToolStatus() == acp.ToolStatusCompleted {
			seenUpdate = true
		}
	}
	if !seenTool || !seenUpdate {
		t.Fatalf("missing tool call updates: %#v", e.snapshot())
	}
}

func TestAuthRequiredIsTypedError(t *testing.T) {
	aTr, bTr := acp.NewBufferedPair()
	ag := agent.New(aTr, agent.HandlerFuncs{
		NewSession: func(_ context.Context, _ acp.NewSessionParams) (*acp.NewSessionResult, *acp.RPCError) {
			return nil, acp.AuthRequired("Authentication required")
		},
	}, agent.Options{})
	go func() { _ = ag.Conn().Run(context.Background()) }()
	cl := Dial(bTr, Options{})
	t.Cleanup(func() { cl.Close(); ag.Close() })
	ctx, cancel := testCtx(t)
	defer cancel()
	if _, err := cl.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := cl.NewSession(ctx, acp.NewSessionParams{CWD: t.TempDir()})
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeAuthRequired {
		t.Fatalf("expected -32000 auth_required, got %v", err)
	}
}

func TestUnsupportedProtocolVersion(t *testing.T) {
	aTr, bTr := acp.NewBufferedPair()
	ag := agent.New(aTr, agent.HandlerFuncs{
		Initialize: func(_ context.Context, _ acp.InitializeParams) (*acp.InitializeResult, *acp.RPCError) {
			return &acp.InitializeResult{ProtocolVersion: 99}, nil
		},
	}, agent.Options{})
	go func() { _ = ag.Conn().Run(context.Background()) }()
	cl := Dial(bTr, Options{})
	t.Cleanup(func() { cl.Close(); ag.Close() })
	ctx, cancel := testCtx(t)
	defer cancel()
	if _, err := cl.Initialize(ctx); !errors.Is(err, ErrUnsupportedProtocolVersion) {
		t.Fatalf("expected ErrUnsupportedProtocolVersion, got %v", err)
	}
}

func TestCancelEndsTurnWithCancelled(t *testing.T) {
	aTr, bTr := acp.NewBufferedPair()
	started := make(chan struct{})
	ag := agent.New(aTr, agent.HandlerFuncs{
		NewSession: func(_ context.Context, _ acp.NewSessionParams) (*acp.NewSessionResult, *acp.RPCError) {
			return &acp.NewSessionResult{SessionID: "s1"}, nil
		},
		Prompt: func(ctx context.Context, _ acp.PromptParams, _ *agent.Emitter) (*acp.PromptResult, *acp.RPCError) {
			close(started)
			<-ctx.Done()
			return &acp.PromptResult{StopReason: acp.StopCancelled}, nil
		},
	}, agent.Options{})
	go func() { _ = ag.Conn().Run(context.Background()) }()
	cl := Dial(bTr, Options{})
	t.Cleanup(func() { cl.Close(); ag.Close() })
	ctx, cancel := testCtx(t)
	defer cancel()
	if _, err := cl.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.NewSession(ctx, acp.NewSessionParams{CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	type promptResult struct {
		r   *acp.PromptResult
		err error
	}
	ch := make(chan promptResult, 1)
	go func() {
		r, err := cl.Prompt(ctx, "s1", []acp.ContentBlock{acp.TextBlock("long")})
		ch <- promptResult{r, err}
	}()
	<-started
	if err := cl.Cancel(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.err != nil || got.r == nil || got.r.StopReason != acp.StopCancelled {
			t.Fatalf("expected cancelled stop reason: %#v err=%v", got.r, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not return after cancel")
	}
}
