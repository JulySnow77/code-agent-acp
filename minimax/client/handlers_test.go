package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minimax-acp/acp"
)

func TestAutoPermission(t *testing.T) {
	p := &acp.RequestPermissionParams{
		SessionID: "s",
		Options: []acp.PermissionOption{
			{OptionID: "allow-once", Name: "Allow once", Kind: acp.PermissionAllowOnce},
			{OptionID: "allow-always", Name: "Always", Kind: acp.PermissionAllowAlways},
			{OptionID: "reject-once", Name: "Reject", Kind: acp.PermissionRejectOnce},
		},
	}
	deny, rpcErr := AutoPermission(PermissionDeny)(context.Background(), p)
	if rpcErr != nil || deny.Outcome.Outcome != acp.OutcomeSelected || deny.Outcome.OptionID != "reject-once" {
		t.Fatalf("deny picked %#v (%v)", deny, rpcErr)
	}
	allow, rpcErr := AutoPermission(PermissionAllow)(context.Background(), p)
	if rpcErr != nil || allow.Outcome.Outcome != acp.OutcomeSelected || allow.Outcome.OptionID != "allow-once" {
		t.Fatalf("allow picked %#v (%v)", allow, rpcErr)
	}
	empty, rpcErr := AutoPermission(PermissionAllow)(context.Background(), &acp.RequestPermissionParams{})
	if rpcErr != nil || empty.Outcome.Outcome != acp.OutcomeCancelled {
		t.Fatalf("empty options must yield cancelled: %#v (%v)", empty, rpcErr)
	}
}

func TestLocalFS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &LocalFS{Root: dir}
	res, rpcErr := fs.ReadTextFile(context.Background(), acp.ReadTextFileParams{SessionID: "s", Path: path})
	if rpcErr != nil || res.Content != "a\nb\nc\n" {
		t.Fatalf("read: %#v %v", res, rpcErr)
	}
	line, limit := 2, 2
	res, rpcErr = fs.ReadTextFile(context.Background(), acp.ReadTextFileParams{SessionID: "s", Path: path, Line: &line, Limit: &limit})
	if rpcErr != nil || res.Content != "b\nc" {
		t.Fatalf("line/limit: %q %v", res, rpcErr)
	}
	if _, rpcErr = fs.ReadTextFile(context.Background(), acp.ReadTextFileParams{Path: "relative.txt"}); rpcErr == nil {
		t.Fatal("relative path must be rejected")
	}
	if _, rpcErr = fs.ReadTextFile(context.Background(), acp.ReadTextFileParams{Path: filepath.Join(dir, "..", "outside.txt")}); rpcErr == nil {
		t.Fatal("path outside root must be rejected")
	}
	if _, rpcErr = fs.WriteTextFile(context.Background(), acp.WriteTextFileParams{Path: path, Content: "x"}); rpcErr == nil {
		t.Fatal("write must be disabled by default")
	}
	fs.AllowWrite = true
	newPath := filepath.Join(dir, "written.txt")
	if _, rpcErr = fs.WriteTextFile(context.Background(), acp.WriteTextFileParams{Path: newPath, Content: "written"}); rpcErr != nil {
		t.Fatalf("write: %v", rpcErr)
	}
	data, err := os.ReadFile(newPath)
	if err != nil || string(data) != "written" {
		t.Fatalf("write result: %q %v", data, err)
	}
}

func TestLocalTerminal(t *testing.T) {
	term := NewLocalTerminal()
	defer term.ReleaseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, rpcErr := term.Create(ctx, acp.CreateTerminalParams{SessionID: "s"}); rpcErr == nil {
		t.Fatal("empty command must be rejected")
	}
	created, rpcErr := term.Create(ctx, acp.CreateTerminalParams{
		SessionID: "s", Command: "/bin/echo", Args: []string{"hello-acp"},
	})
	if rpcErr != nil || created.TerminalID == "" {
		t.Fatalf("create: %#v %v", created, rpcErr)
	}
	waited, rpcErr := term.WaitForExit(ctx, acp.WaitForTerminalExitParams{SessionID: "s", TerminalID: created.TerminalID})
	if rpcErr != nil || waited.ExitCode == nil || *waited.ExitCode != 0 {
		t.Fatalf("wait_for_exit: %#v %v", waited, rpcErr)
	}
	out, rpcErr := term.Output(ctx, acp.TerminalOutputParams{SessionID: "s", TerminalID: created.TerminalID})
	if rpcErr != nil || !strings.Contains(out.Output, "hello-acp") || out.Truncated {
		t.Fatalf("output: %#v %v", out, rpcErr)
	}
	if out.ExitStatus == nil || out.ExitStatus.ExitCode == nil {
		t.Fatal("terminal/output must include exitStatus after the command exited")
	}
	if _, rpcErr = term.Release(ctx, acp.ReleaseTerminalParams{SessionID: "s", TerminalID: created.TerminalID}); rpcErr != nil {
		t.Fatalf("release: %v", rpcErr)
	}
	if _, rpcErr = term.Output(ctx, acp.TerminalOutputParams{SessionID: "s", TerminalID: created.TerminalID}); rpcErr == nil || rpcErr.Code != acp.CodeResourceNotFound {
		t.Fatalf("released terminal must be gone: %v", rpcErr)
	}
}

func TestLocalTerminalKill(t *testing.T) {
	term := NewLocalTerminal()
	defer term.ReleaseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, rpcErr := term.Create(ctx, acp.CreateTerminalParams{SessionID: "s", Command: "/bin/sleep", Args: []string{"30"}})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if _, rpcErr = term.Kill(ctx, acp.KillTerminalParams{SessionID: "s", TerminalID: created.TerminalID}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waited, rpcErr := term.WaitForExit(ctx, acp.WaitForTerminalExitParams{SessionID: "s", TerminalID: created.TerminalID})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if waited.ExitCode != nil && *waited.ExitCode == 0 {
		t.Fatal("killed command must not report a successful exit")
	}
}
