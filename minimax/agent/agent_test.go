package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"minimax-acp/acp"
)

func newAgentPair(t *testing.T, h HandlerFuncs) (*acp.Conn, func()) {
	t.Helper()
	aTr, bTr := acp.NewBufferedPair()
	ctx, cancel := context.WithCancel(context.Background())
	a := New(aTr, h, Options{})
	go func() { _ = a.Conn().Run(ctx) }()
	cl := acp.NewConn(bTr)
	go func() { _ = cl.Run(ctx) }()
	return cl, func() { cl.Close(); a.Close(); cancel() }
}

func TestServeBaselineMethods(t *testing.T) {
	cl, cleanup := newAgentPair(t, NewEcho().Handlers())
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	var methods []string
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

	var initResult acp.InitializeResult
	if err := cl.Call(ctx, acp.MethodInitialize, acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersionV1,
		ClientCapabilities: acp.ClientCapabilities{
			FS:       &acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	}, &initResult); err != nil {
		t.Fatal(err)
	}
	if initResult.ProtocolVersion != acp.ProtocolVersionV1 || !initResult.AgentCapabilities.CanLoadSession() {
		t.Fatalf("bad initialize result: %#v", initResult)
	}

	var ns acp.NewSessionResult
	if err := cl.Call(ctx, acp.MethodSessionNew, acp.NewSessionParams{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}, &ns); err != nil {
		t.Fatal(err)
	}
	var promptResult acp.PromptResult
	if err := cl.Call(ctx, acp.MethodSessionPrompt, acp.PromptParams{
		SessionID: ns.SessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock("with tool and permission")},
	}, &promptResult); err != nil {
		t.Fatal(err)
	}
	if promptResult.StopReason != acp.StopEndTurn {
		t.Fatalf("stopReason = %q", promptResult.StopReason)
	}
	// Notifications are handled by an ordered background worker; a prompt
	// response may reach us before the tail session/update frames are rendered.
	// Flush instead of racing the worker (this was flaky in the upstream test).
	if err := cl.FlushNotifications(ctx); err != nil {
		t.Fatalf("flush notifications: %v", err)
	}

	mu.Lock()
	seenUpdate := false
	for _, m := range methods {
		if m == acp.MethodSessionUpdate {
			seenUpdate = true
		}
	}
	gotPermissionCalls := permissionCalls
	mu.Unlock()
	if !seenUpdate {
		t.Fatal("agent did not stream session/update")
	}
	if gotPermissionCalls != 1 {
		t.Fatalf("expected one reverse permission request, got %d", gotPermissionCalls)
	}

	var empty map[string]any
	if err := cl.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionParams{
		SessionID: ns.SessionID, CWD: t.TempDir(), MCPServers: []acp.MCPServer{},
	}, &empty); err != nil {
		t.Fatal(err)
	}
	if err := cl.Call(ctx, "vendor/unknown", nil, &empty); err == nil {
		t.Fatal("unknown method must fail")
	} else {
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != acp.CodeMethodNotFound {
			t.Fatalf("expected -32601, got %v", err)
		}
	}
}

func TestEmitterChecksClientCapabilities(t *testing.T) {
	gate := make(chan error, 1)
	h := HandlerFuncs{
		NewSession: func(_ context.Context, _ acp.NewSessionParams) (*acp.NewSessionResult, *acp.RPCError) {
			return &acp.NewSessionResult{SessionID: "s"}, nil
		},
		Prompt: func(ctx context.Context, _ acp.PromptParams, e *Emitter) (*acp.PromptResult, *acp.RPCError) {
			_, err := e.ReadTextFile(ctx, acp.ReadTextFileParams{SessionID: "s", Path: "/tmp/x"})
			gate <- err
			return &acp.PromptResult{StopReason: acp.StopEndTurn}, nil
		},
	}
	cl, cleanup := newAgentPair(t, h)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Client advertises NO fs capability.
	if err := cl.Call(ctx, acp.MethodInitialize, acp.InitializeParams{ProtocolVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := cl.Call(ctx, acp.MethodSessionNew, acp.NewSessionParams{CWD: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := cl.Call(ctx, acp.MethodSessionPrompt, acp.PromptParams{
		SessionID: "s", Prompt: []acp.ContentBlock{acp.TextBlock("x")},
	}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-gate:
		if !errors.Is(err, ErrCapabilityNotAdvertised) {
			t.Fatalf("expected ErrCapabilityNotAdvertised, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("emitter did not run")
	}
}

func TestEchoHelpers(t *testing.T) {
	chunks := chunkRunes("abcdefghij", 3)
	if strings.Join(chunks, "") != "abcdefghij" || len(chunks) != 4 {
		t.Fatalf("chunkRunes failed: %#v", chunks)
	}
	if got := chunkRunes("", 3); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty chunk: %#v", got)
	}
	text := collectPromptText([]acp.ContentBlock{
		acp.TextBlock("hi"),
		acp.ResourceLinkBlock("main.go", "file:///tmp/main.go"),
		acp.ImageBlock("AA==", "image/png"),
		acp.EmbeddedResourceBlock("code", "file:///tmp/x", "text/plain"),
	})
	for _, want := range []string{"hi", "main.go", "image/png", "file:///tmp/x"} {
		if !strings.Contains(text, want) {
			t.Fatalf("collectPromptText %q missing %q", text, want)
		}
	}
}
