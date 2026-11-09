package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"kimi-acp/acp"
	"kimi-acp/agent"
)

// newProxiedEcho wires a raw ACP client to an Echo agent through the bridge:
//
//	client <-> bridge <-> echo agent
//
// No external process is involved, but all four JSON-RPC hops are real.
func newProxiedEcho(t *testing.T) (*acp.Conn, *Bridge, func(), *sync.Mutex, *[]string, *int) {
	t.Helper()
	downClientTr, downBridgeTr := acp.NewBufferedPair()
	upBridgeTr, upAgentTr := acp.NewBufferedPair()
	ctx, cancel := context.WithCancel(context.Background())

	ag := agent.New(upAgentTr, agent.NewEcho().Handlers(), agent.Options{})
	go func() { _ = ag.Conn().Run(ctx) }()
	b := New(downBridgeTr, upBridgeTr, Options{})
	go func() { _ = b.Run(ctx) }()
	cl := acp.NewConn(downClientTr)
	go func() { _ = cl.Run(ctx) }()

	var mu sync.Mutex
	methods := []string{}
	permissionCalls := 0
	cl.OnDefaultNotification(func(_ context.Context, method string, _ json.RawMessage) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
	})
	cl.OnRequest(acp.MethodSessionRequestPermission, func(_ context.Context, _ json.RawMessage) (any, *acp.RPCError) {
		mu.Lock()
		permissionCalls++
		mu.Unlock()
		return acp.SelectedOutcome("allow-once"), nil
	})

	cleanup := func() { cl.Close(); b.Close(); ag.Close(); cancel() }
	return cl, b, cleanup, &mu, &methods, &permissionCalls
}

func TestBridgeForwardsRequestsResponsesAndNotifications(t *testing.T) {
	cl, _, cleanup, mu, methods, permissionCalls := newProxiedEcho(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	var initResult acp.InitializeResult
	if err := cl.Call(ctx, acp.MethodInitialize, acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersionV1,
		ClientCapabilities: acp.ClientCapabilities{
			FS: &acp.FileSystemCapabilities{ReadTextFile: true},
		},
	}, &initResult); err != nil {
		t.Fatal(err)
	}
	if initResult.ProtocolVersion != acp.ProtocolVersionV1 || initResult.AgentInfo == nil || initResult.AgentInfo.Name != "kimi-acp-go-echo" {
		t.Fatalf("initialize was not forwarded verbatim: %#v", initResult)
	}

	var ns acp.NewSessionResult
	if err := cl.Call(ctx, acp.MethodSessionNew, acp.NewSessionParams{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}, &ns); err != nil {
		t.Fatal(err)
	}
	var promptResult acp.PromptResult
	if err := cl.Call(ctx, acp.MethodSessionPrompt, acp.PromptParams{
		SessionID: ns.SessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock("tool and permission please")},
	}, &promptResult); err != nil {
		t.Fatal(err)
	}
	if promptResult.StopReason != acp.StopEndTurn {
		t.Fatalf("stopReason = %q", promptResult.StopReason)
	}

	// Reverse calls and notifications must make it back through the bridge.
	mu.Lock()
	gotPermissions := *permissionCalls
	updates := 0
	for _, m := range *methods {
		if m == acp.MethodSessionUpdate {
			updates++
		}
	}
	mu.Unlock()
	if gotPermissions != 1 {
		t.Fatalf("reverse permission request not forwarded: %d", gotPermissions)
	}
	if updates < 2 {
		t.Fatalf("session/update notifications not forwarded: %d", updates)
	}
}

func TestBridgeForwardsErrorsUnchanged(t *testing.T) {
	cl, _, cleanup, _, _, _ := newProxiedEcho(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cl.Call(ctx, acp.MethodInitialize, acp.InitializeParams{ProtocolVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	err := cl.Call(ctx, "vendor/unknown", nil, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeMethodNotFound {
		t.Fatalf("error response was not forwarded: %v", err)
	}
}

func TestPromptResponseWaitsForForwardedUpdates(t *testing.T) {
	client, bridge, cleanup, _, _, _ := newProxiedEcho(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var session acp.NewSessionResult
	if err := client.Call(ctx, acp.MethodSessionNew, acp.NewSessionParams{CWD: t.TempDir()}, &session); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	bridge.up.OnDefaultNotification(func(_ context.Context, method string, params json.RawMessage) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		_ = bridge.down.NotifyRaw(method, params)
	})
	completed := make(chan error, 1)
	go func() {
		completed <- client.Call(ctx, acp.MethodSessionPrompt, acp.PromptParams{
			SessionID: session.SessionID, Prompt: []acp.ContentBlock{acp.TextBlock("result")},
		}, nil)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("no streamed update received")
	}
	select {
	case err := <-completed:
		t.Fatalf("prompt returned before streamed updates were forwarded: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}
