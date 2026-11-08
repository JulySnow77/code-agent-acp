package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy-acp-bridge/internal/cli"
	"workbuddy-acp-bridge/internal/testutil"
)

var (
	buildOnce  sync.Once
	buildDir   string
	binaryPath string
	buildError error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func projectRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func build(t *testing.T, outputName, packagePath string) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildError = os.MkdirTemp("", "workbuddy-acp-cli-")
		if buildError != nil {
			return
		}
		binaryPath = filepath.Join(buildDir, "workbuddy-acp-bridge")
		command := exec.Command("go", "build", "-o", binaryPath, "./cmd/workbuddy-acp-bridge")
		command.Dir = projectRoot()
		if output, err := command.CombinedOutput(); err != nil {
			buildError = &buildFailure{string(output), err}
		}
	})
	if buildError != nil {
		t.Fatalf("cannot build CLI binary: %v", buildError)
	}
	if outputName == "workbuddy-acp-bridge" {
		return binaryPath
	}
	fixture := filepath.Join(buildDir, outputName)
	command := exec.Command("go", "build", "-o", fixture, packagePath)
	command.Dir = projectRoot()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cannot build %s: %v %s", outputName, err, output)
	}
	return fixture
}

type buildFailure struct {
	output string
	err    error
}

func (f *buildFailure) Error() string { return f.err.Error() + ": " + f.output }

func runBinary(t *testing.T, stdin string, env []string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Stdin = strings.NewReader(stdin)
	command.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("command timed out: %v", args)
	}
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("cannot start %v: %v", args, err)
		}
	}
	return code, stdout.String(), stderr.String()
}

var stdioFrames = testutil.ClientFrames(
	map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1}},
	map[string]any{"id": 2, "method": "session/new", "params": map[string]any{"cwd": "/tmp", "mcpServers": []any{}}},
	map[string]any{"id": 3, "method": "session/prompt",
		"params": map[string]any{"prompt": []any{map[string]any{"type": "text", "text": "go"}}}},
)

func TestVersionMarkerIsPrintedByTheExecutable(t *testing.T) {
	binary := build(t, "workbuddy-acp-bridge", "")
	code, stdout, stderr := runBinary(t, "", nil, binary, "version")
	if code != 0 || !strings.Contains(stdout, "workbuddy-acp-bridge") {
		t.Fatalf("version failed: %d %q %q", code, stdout, stderr)
	}
}

func TestSessionsCommandUsesTheRequestedDirectory(t *testing.T) {
	binary := build(t, "workbuddy-acp-bridge", "")
	missing := filepath.Join(t.TempDir(), "no-such-sessions")
	code, stdout, stderr := runBinary(t, "", nil, binary, "sessions", "--json", "--sessions-dir", missing)
	if code != 0 || strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("sessions failed: %d %q %q", code, stdout, stderr)
	}
}

func TestDoctorIsReadOnlyAndReportsGatewayCapabilities(t *testing.T) {
	binary := build(t, "workbuddy-acp-bridge", "")
	gateway := testutil.NewFakeGateway()
	defer gateway.Close()
	code, stdout, stderr := runBinary(t, "", nil, binary, "doctor",
		"--endpoint", gateway.Endpoint(), "--json")
	if code != 0 {
		t.Fatalf("doctor failed: %d %q %q", code, stdout, stderr)
	}
	var report cli.DoctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "valid" || report.ProtocolVersion != float64(1) || report.PromptSent {
		t.Fatalf("unexpected report: %+v", report)
	}
	capabilities, _ := report.AgentCapabilities.(map[string]any)
	if capabilities["loadSession"] != true {
		t.Fatalf("capabilities missing: %v", report.AgentCapabilities)
	}
	methods := strings.Join(gateway.Methods(), ",")
	if strings.Contains(methods, "session/load") || strings.Contains(methods, "session/prompt") {
		t.Fatalf("doctor must not touch a conversation: %s", methods)
	}
}

func TestDoctorReportsFailureWithNonZeroExit(t *testing.T) {
	binary := build(t, "workbuddy-acp-bridge", "")
	code, stdout, _ := runBinary(t, "", nil, binary, "doctor",
		"--endpoint", "http://127.0.0.1:1", "--json", "--timeout", "2")
	if code != 1 || !strings.Contains(stdout, `"status": "error"`) {
		t.Fatalf("unexpected doctor result: %d %q", code, stdout)
	}
}

func TestServeAsRealStdioProcessCompletesRoundTrip(t *testing.T) {
	binary := build(t, "workbuddy-acp-bridge", "")
	code, stdout, stderr := runBinary(t, stdioFrames,
		[]string{`WORKBUDDY_FAKE_RESULT={"value":"cli"}`},
		binary, "serve", "--backend", "fake")
	if code != 0 {
		t.Fatalf("serve failed: %d %q", code, stderr)
	}
	if !strings.Contains(stdout, `"stopReason":"end_turn"`) ||
		!strings.Contains(stdout, `{\"value\":\"cli\"}`) {
		t.Fatalf("unexpected stdio output: %s", stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("non-ndjson output line: %q", line)
		}
		if frame["jsonrpc"] != "2.0" {
			t.Fatalf("frame is not JSON-RPC 2.0: %q", line)
		}
	}
}

func TestBackendEnvOverrideSupportsFixedHostArgv(t *testing.T) {
	binary := build(t, "workbuddy-acp-bridge", "")
	code, stdout, stderr := runBinary(t, stdioFrames,
		[]string{"WORKBUDDY_ACP_BACKEND=fake", `WORKBUDDY_FAKE_RESULT={"value":"env"}`},
		binary, "serve", "--mode", "plan")
	if code != 0 || !strings.Contains(stdout, `"stopReason":"end_turn"`) {
		t.Fatalf("env override failed: %d %q %q", code, stdout, stderr)
	}
}

func TestUnknownBackendEnvIsRejected(t *testing.T) {
	binary := build(t, "workbuddy-acp-bridge", "")
	code, _, stderr := runBinary(t, "", []string{"WORKBUDDY_ACP_BACKEND=bogus"},
		binary, "serve", "--mode", "plan")
	if code != 1 || !strings.Contains(stderr, "unknown WORKBUDDY_ACP_BACKEND") {
		t.Fatalf("unexpected result: %d %q", code, stderr)
	}
}

func TestFixtureBinarySpeaksStandardACPv1(t *testing.T) {
	fixture := build(t, "fake-acp-server", "./cmd/fake-acp-server")
	code, stdout, stderr := runBinary(t, "", nil, fixture, "version")
	if code != 0 || !strings.Contains(stdout, "workbuddy-acp-bridge fixture") {
		t.Fatalf("fixture version failed: %d %q %q", code, stdout, stderr)
	}
	frames := testutil.ClientFrames(
		map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1}},
		map[string]any{"id": 2, "method": "session/new", "params": map[string]any{}},
		map[string]any{"id": 3, "method": "session/prompt", "params": map[string]any{"prompt": []any{}}},
	)
	code, stdout, stderr = runBinary(t, frames, []string{`AUTO_RUN_FAKE_WB_RESULT={"value":"fixture"}`}, fixture)
	if code != 0 || !strings.Contains(stdout, `"stopReason":"end_turn"`) ||
		!strings.Contains(stdout, `"sessionCapabilities"`) {
		t.Fatalf("fixture round trip failed: %d %q %q", code, stdout, stderr)
	}
}
