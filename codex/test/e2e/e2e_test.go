// Package e2e drives the compiled binary with a scripted `codex app-server`
// child process. Unlike the in-process tests, this covers real process launch,
// pipe plumbing, the JSON-RPC-lite wire format and clean shutdown.
package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-acp-bridge/internal/protocol"
	"codex-acp-bridge/internal/testutil"
)

const fakeAppServer = `#!/usr/bin/env python3
import json, os, sys

log_path = os.environ.get("FAKE_APP_SERVER_LOG")

def out(obj):
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()

def note(method, params):
    out({"method": method, "params": params})

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    if log_path:
        with open(log_path, "a", encoding="utf-8") as handle:
            handle.write(line + "\n")
    try:
        message = json.loads(line)
    except ValueError:
        continue
    identifier = message.get("id")
    method = message.get("method")
    if method == "initialize":
        out({"id": identifier, "result": {
            "userAgent": "fake-app-server/1", "codexHome": "/tmp/fake",
            "platformFamily": "unix", "platformOs": "test"}})
    elif method == "thread/start":
        out({"id": identifier, "result": {"thread": {"id": "thr-real"}}})
    elif method == "turn/start":
        turn_id = "turn-real"
        thread_id = message.get("params", {}).get("threadId", "thr-real")
        out({"id": identifier, "result": {"turn": {"id": turn_id, "status": "inProgress"}}})
        note("item/agentMessage/delta", {"threadId": thread_id, "turnId": turn_id,
                                         "itemId": "m1", "delta": "pong"})
        note("item/completed", {"threadId": thread_id, "turnId": turn_id,
                               "item": {"type": "commandExecution", "id": "c1",
                                        "command": "echo pong", "exitCode": 0,
                                        "aggregatedOutput": "pong\n", "status": "completed"}})
        note("turn/completed", {"threadId": thread_id, "turnId": turn_id,
                               "turn": {"id": turn_id, "status": "completed"}})
    elif method == "turn/interrupt":
        out({"id": identifier, "result": {}})
`

// exec writes to Stderr from its own goroutine; reading it while the process
// is alive needs a lock (go test -race).
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func buildBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "codex-acp-bridge")
	command := exec.Command("go", "build", "-o", binary, "./cmd/codex-acp-bridge")
	command.Dir = moduleRoot(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, output)
	}
	return binary
}

func writeFakeCodex(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for the subprocess fake app-server")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(script, []byte(fakeAppServer), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, filepath.Join(dir, "frames.jsonl")
}

func TestServeTalksACPToRealChildProcess(t *testing.T) {
	binary := buildBinary(t)
	script, logPath := writeFakeCodex(t)

	command := exec.Command(binary, "serve")
	command.Env = append(os.Environ(),
		"CODEX_BIN="+script,
		"FAKE_APP_SERVER_LOG="+logPath,
		"CODEX_ACP_TURN_TIMEOUT=20s",
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	client := testutil.NewHarness(stdin, stdout, "allow_once")
	client.Start()

	defer func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("bridge exit: %v; stderr=%s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			t.Error("bridge did not exit when stdin closed")
		}
		if strings.Contains(stderr.String(), "panic:") {
			t.Errorf("bridge panicked: %s", stderr.String())
		}
	}()

	init, rpcErr := client.Call(protocol.MethodInitialize, protocol.Message{
		"protocolVersion": 1,
		"clientInfo":      protocol.Message{"name": "e2e", "version": "1"},
	})
	if rpcErr != nil {
		t.Fatalf("initialize: %v; stderr=%s", rpcErr, stderr.String())
	}
	if int(protocol.Num(init, "protocolVersion")) != 1 {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	session, rpcErr := client.Call(protocol.MethodSessionNew, protocol.Message{"cwd": moduleRoot(t)})
	if rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	sessionID := protocol.Str(session, "sessionId")
	if sessionID != "thr-real" {
		t.Errorf("sessionId = %q, want thr-real", sessionID)
	}
	result, rpcErr := client.Call(protocol.MethodSessionPrompt, protocol.Message{
		"sessionId": sessionID,
		"prompt":    []any{protocol.Message{"type": "text", "text": "ping"}},
	})
	if rpcErr != nil {
		t.Fatalf("session/prompt: %v; stderr=%s", rpcErr, stderr.String())
	}
	if protocol.Str(result, "stopReason") != protocol.StopEndTurn {
		t.Errorf("stopReason = %v", result["stopReason"])
	}
	updates, err := client.WaitUpdates(3, 5*time.Second)
	if err != nil {
		t.Fatalf("%v; stderr=%s", err, stderr.String())
	}
	foundText := false
	foundTool := false
	for _, update := range updates {
		body := protocol.Sub(update, "update")
		switch protocol.Str(body, "sessionUpdate") {
		case protocol.UpdateAgentMessageChunk:
			if protocol.Str(protocol.Sub(body, "content"), "text") == "pong" {
				foundText = true
			}
		case protocol.UpdateToolCall:
			foundTool = true
		}
	}
	if !foundText || !foundTool {
		t.Errorf("missing streamed text or tool call: %v", updates)
	}

	// The app-server dialect must NOT carry the "jsonrpc" member.
	frames, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(frames)), "\n") {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("invalid child frame %q: %v", line, err)
		}
		if _, present := frame["jsonrpc"]; present {
			t.Errorf("app-server frame contains jsonrpc: %s", line)
		}
	}
	for _, method := range []string{"initialize", "initialized", "thread/start", "turn/start"} {
		if !strings.Contains(string(frames), `"method":"`+method+`"`) {
			t.Errorf("child never received %s; frames=%s", method, frames)
		}
	}
}

func TestDoctorPerformsReadOnlyHandshake(t *testing.T) {
	binary := buildBinary(t)
	script, _ := writeFakeCodex(t)
	command := exec.Command(binary, "doctor")
	command.Env = append(os.Environ(), "CODEX_BIN="+script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, output)
	}
	if report["ok"] != true {
		t.Errorf("doctor ok = %v", report["ok"])
	}
	appServer, _ := report["appServer"].(map[string]any)
	if appServer["userAgent"] != "fake-app-server/1" {
		t.Errorf("doctor did not capture the handshake: %v", appServer)
	}
}
