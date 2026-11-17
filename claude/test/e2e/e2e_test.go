// Package e2e drives the compiled binary with a scripted `claude` child
// process. Unlike the in-process tests, this covers real process launch, stdio
// plumbing, the stream-json wire format and clean shutdown.
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

	"claude-acp-bridge/internal/protocol"
	"claude-acp-bridge/internal/testutil"
)

// The fake CLI reads one NDJSON user message, then emits a scripted turn: an
// init frame, an assistant text block, a tool_use + tool_result pair, and a
// final result. It records every stdin frame so the test can assert the exact
// wire shape (no jsonrpc member, correct control protocol).
const fakeClaude = `#!/usr/bin/env python3
import json, os, sys

log_path = os.environ.get("FAKE_CLAUDE_LOG")

def out(obj):
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()

def reply_prompt(content):
    out({"type": "system", "subtype": "init", "session_id": "sess-fake",
         "cwd": "/tmp", "model": "fake-model", "tools": ["Bash"], "uuid": "u0"})
    out({"type": "assistant", "session_id": "sess-fake", "uuid": "u1",
         "message": {"role": "assistant", "content": [{"type": "text", "text": "pong"}]}})
    out({"type": "assistant", "session_id": "sess-fake", "uuid": "u2",
         "message": {"role": "assistant", "content": [
             {"type": "tool_use", "id": "toolu-fake", "name": "Bash",
              "input": {"command": "echo pong"}}]}})
    out({"type": "user", "session_id": "sess-fake", "uuid": "u3",
         "message": {"role": "user", "content": [
             {"type": "tool_result", "tool_use_id": "toolu-fake", "content": "pong\n"}]}})
    out({"type": "result", "subtype": "success", "is_error": False,
         "session_id": "sess-fake", "stop_reason": "end_turn", "result": "pong",
         "usage": {"input_tokens": 5, "output_tokens": 2},
         "modelUsage": {"fake-model": {"contextWindow": 200000}}, "uuid": "u4"})

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
    if message.get("type") == "user":
        reply_prompt(message.get("message", {}).get("content"))
    elif message.get("type") == "control_request":
        out({"type": "control_response", "response": {
            "subtype": "success", "request_id": message.get("request_id", ""),
            "response": {}}})
`

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
	binary := filepath.Join(t.TempDir(), "claude-acp-bridge")
	command := exec.Command("go", "build", "-o", binary, "./cmd/claude-acp-bridge")
	command.Dir = moduleRoot(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, output)
	}
	return binary
}

func writeFakeClaude(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for the subprocess fake CLI")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(script, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, filepath.Join(dir, "frames.jsonl")
}

func TestServeTalksACPToRealChildProcess(t *testing.T) {
	binary := buildBinary(t)
	script, logPath := writeFakeClaude(t)

	command := exec.Command(binary, "serve")
	command.Env = append(os.Environ(),
		"CLAUDE_BIN="+script,
		"FAKE_CLAUDE_LOG="+logPath,
		"CLAUDE_ACP_TURN_TIMEOUT=20s",
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
	if len(sessionID) != 36 {
		t.Errorf("sessionId = %q, want a UUID", sessionID)
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
	updates, err := client.WaitUpdates(4, 5*time.Second)
	if err != nil {
		t.Fatalf("%v; stderr=%s", err, stderr.String())
	}
	foundText, foundTool := false, false
	for _, update := range updates {
		body := protocol.Sub(update, "update")
		switch protocol.Str(body, "sessionUpdate") {
		case protocol.UpdateAgentMessageChunk:
			if protocol.Str(protocol.Sub(body, "content"), "text") == "pong" {
				foundText = true
			}
		case protocol.UpdateToolCallUpdate:
			foundTool = true
		}
	}
	if !foundText || !foundTool {
		t.Errorf("missing streamed text or tool call: %v", updates)
	}

	// The child frames must be plain stream-json with no jsonrpc member, and
	// must include the required verbose stream-json flags on launch (verified
	// via the doctor test and argv assertions).
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
			t.Errorf("stream-json frame contains jsonrpc: %s", line)
		}
	}
	if !strings.Contains(string(frames), `"type":"user"`) {
		t.Errorf("child never received a user message; frames=%s", frames)
	}
}

func TestDoctorReportsReadOnlyChecks(t *testing.T) {
	binary := buildBinary(t)
	script, _ := writeFakeClaude(t)
	command := exec.Command(binary, "doctor")
	command.Env = append(os.Environ(), "CLAUDE_BIN="+script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, output)
	}
	if report["ok"] != true {
		t.Errorf("doctor ok = %v: %s", report["ok"], output)
	}
	argv, _ := report["streamJsonArgv"].([]any)
	joined := ""
	for _, item := range argv {
		joined += protocol.StringValue(item) + " "
	}
	for _, flag := range []string{"--input-format", "stream-json", "--output-format", "stream-json", "--permission-prompt-tool", "stdio"} {
		if !strings.Contains(joined, flag) {
			t.Errorf("doctor argv missing %q: %s", flag, joined)
		}
	}
}
