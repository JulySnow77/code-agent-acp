package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildBinary compiles the real CLI once per test package and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kimi-acp")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = wd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

type driver struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr *bytes.Buffer
	notes  []string
}

func startDriver(t *testing.T, bin string, args ...string) *driver {
	t.Helper()
	cmd := exec.Command(bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 64), stderr: &bytes.Buffer{}}
	cmd.Stderr = d.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		s := bufio.NewScanner(stdout)
		s.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for s.Scan() {
			d.lines <- s.Text()
		}
		close(d.lines)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return d
}

func (d *driver) send(v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.stdin.Write(append(b, '\n')); err != nil {
		d.t.Fatal(err)
	}
}

func (d *driver) reply(obj map[string]any) {
	method, _ := obj["method"].(string)
	result := map[string]any{}
	switch method {
	case "session/request_permission":
		result = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}}
	case "fs/read_text_file":
		result = map[string]any{"content": "content"}
	case "fs/write_text_file", "terminal/create", "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		result = map[string]any{}
	}
	d.send(map[string]any{"jsonrpc": "2.0", "id": obj["id"], "result": result})
}

// call waits for the response with the given id, answering reverse requests and
// recording notifications along the way.
func (d *driver) call(id int, method string, params map[string]any) map[string]any {
	d.t.Helper()
	d.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	deadline := time.After(8 * time.Second)
	for {
		select {
		case line, ok := <-d.lines:
			if !ok {
				d.t.Fatalf("stdout closed while waiting for %s (stderr=%s)", method, d.stderr.String())
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				d.t.Fatalf("non-JSON stdout line %q (stderr=%s)", line, d.stderr.String())
			}
			if m, ok := obj["method"].(string); ok {
				d.notes = append(d.notes, m)
				if _, hasID := obj["id"]; hasID {
					d.reply(obj)
				}
				continue
			}
			if got, ok := obj["id"].(float64); ok && int(got) == id {
				return obj
			}
		case <-deadline:
			d.t.Fatalf("timeout waiting for %s (stderr=%s)", method, d.stderr.String())
		}
	}
}

func initializeFrame() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
			"terminal": true,
		},
		"clientInfo": map[string]any{"name": "cli-test", "version": "0"},
	}
}

func resultOf(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	if e, ok := obj["error"]; ok {
		t.Fatalf("unexpected error response: %v", e)
	}
	r, ok := obj["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %#v", obj)
	}
	return r
}

func TestServeSubprocessRoundTrip(t *testing.T) {
	bin := buildBinary(t)
	d := startDriver(t, bin, "serve")
	result := resultOf(t, d.call(1, "initialize", initializeFrame()))
	if result["protocolVersion"] != float64(1) {
		t.Fatalf("bad initialize result: %#v", result)
	}
	cwd, _ := os.Getwd()
	ns := resultOf(t, d.call(2, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}))
	sessionID, _ := ns["sessionId"].(string)
	if sessionID == "" {
		t.Fatalf("no sessionId: %#v", ns)
	}
	pr := resultOf(t, d.call(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "subprocess round trip"}},
	}))
	if pr["stopReason"] != "end_turn" {
		t.Fatalf("bad prompt result: %#v", pr)
	}
	updates := 0
	for _, m := range d.notes {
		if m == "session/update" {
			updates++
		}
	}
	if updates < 2 {
		t.Fatalf("expected streamed updates, got %d (%v)", updates, d.notes)
	}
	errObj := d.call(4, "vendor/unknown", map[string]any{})
	e, ok := errObj["error"].(map[string]any)
	if !ok || e["code"] != float64(-32601) {
		t.Fatalf("expected -32601, got %#v", errObj)
	}
}

func TestBridgeSubprocessWithGoAgent(t *testing.T) {
	bin := buildBinary(t)
	// Use our own ACP agent as the upstream instead of requiring `kimi`.
	d := startDriver(t, bin, "bridge", "--command", bin, "--args", "serve")
	result := resultOf(t, d.call(1, "initialize", initializeFrame()))
	if result["protocolVersion"] != float64(1) {
		t.Fatalf("bridge did not forward initialize: %#v", result)
	}
	cwd, _ := os.Getwd()
	ns := resultOf(t, d.call(2, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}))
	sessionID, _ := ns["sessionId"].(string)
	pr := resultOf(t, d.call(3, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": "through the bridge"}},
	}))
	if pr["stopReason"] != "end_turn" {
		t.Fatalf("bridge prompt result: %#v", pr)
	}
}

func TestDoctorSubprocessReadOnly(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "doctor", "--command", bin, "--args", "serve")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, stderr.String())
	}
	var out struct {
		Status            string `json:"status"`
		PromptSent        bool   `json:"promptSent"`
		ProtocolVersion   int    `json:"protocolVersion"`
		SessionListCalled bool   `json:"sessionListCalled"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("doctor did not emit JSON: %v\n%s", err, stdout.String())
	}
	if out.Status != "valid" || out.PromptSent || out.ProtocolVersion != 1 || !out.SessionListCalled {
		t.Fatalf("unexpected doctor report: %#v", out)
	}
}

func TestVersionSubprocess(t *testing.T) {
	bin := buildBinary(t)
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != fmt.Sprintf("kimi-acp %s", version) {
		t.Fatalf("unexpected version output: %q", out)
	}
}
