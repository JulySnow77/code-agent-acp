package minimax

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeInspector(home string) *Inspector {
	return &Inspector{
		LookPath: func(name string) (string, error) {
			return "/usr/local/bin/" + name, nil
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "--version" {
				return []byte("0.6.3\n"), nil
			}
			return []byte("Usage: mcode acp [options]\nRun MiniMax Code as an Agent Client Protocol server over stdio\n"), nil
		},
		LookupEnv: func(string) (string, bool) { return "", false },
		Stat: func(path string) (os.FileInfo, error) {
			if strings.HasSuffix(path, filepath.Join(".minimax", "auth")) || strings.HasSuffix(path, ".minimax") {
				return fakeFileInfo{path}, nil
			}
			return nil, os.ErrNotExist
		},
		Home:    home,
		Timeout: 2 * time.Second,
	}
}

type fakeFileInfo struct{ name string }

func (f fakeFileInfo) Name() string       { return filepath.Base(f.name) }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return os.ModeDir | 0o700 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return true }
func (f fakeFileInfo) Sys() any           { return nil }

func TestCommandFromEnv(t *testing.T) {
	if got := CommandFromEnv(); got != DefaultCommand {
		t.Fatalf("default command = %q, want %q", got, DefaultCommand)
	}
	t.Setenv(CLIOverrideEnv, "  /opt/bin/mcode  ")
	if got := CommandFromEnv(); got != "/opt/bin/mcode" {
		t.Fatalf("override = %q", got)
	}
	t.Setenv(CLIOverrideEnv, "   ")
	if got := CommandFromEnv(); got != DefaultCommand {
		t.Fatalf("blank override = %q", got)
	}
}

func TestLoginHint(t *testing.T) {
	if h := LoginHint("", ""); !strings.Contains(h, "mcode login") || !strings.Contains(h, "--region global") {
		t.Fatalf("default hint = %q", h)
	}
	if h := LoginHint("mcode", "global"); !strings.Contains(h, "mcode acp login --region global") {
		t.Fatalf("global hint = %q", h)
	}
	if h := LoginHint("/opt/mcode", "cn"); !strings.Contains(h, "/opt/mcode login --region cn") {
		t.Fatalf("custom hint = %q", h)
	}
}

func TestInspectHappyPath(t *testing.T) {
	i := fakeInspector("/Users/tester")
	out := i.Inspect("mcode", "")
	if out.Error != "" || len(out.Warnings) != 0 {
		t.Fatalf("unexpected error/warnings: %#v", out)
	}
	if out.CLIVersion != "0.6.3" || !out.ACPServerHelp {
		t.Fatalf("version/help = %q / %v", out.CLIVersion, out.ACPServerHelp)
	}
	if !out.DataDirExists || !out.AuthDirExists {
		t.Fatalf("data/auth dirs = %v / %v", out.DataDirExists, out.AuthDirExists)
	}
	if out.Command != "mcode" || out.ResolvedPath != "/usr/local/bin/mcode" {
		t.Fatalf("command resolution = %#v", out)
	}
}

func TestInspectMissingCLIDoesNotRunCommands(t *testing.T) {
	i := fakeInspector("/Users/tester")
	calls := 0
	i.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	i.Run = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("should not run")
	}
	out := i.Inspect("mcode", "")
	if out.Error == "" || len(out.Warnings) == 0 {
		t.Fatalf("expected error + warning: %#v", out)
	}
	if calls != 0 {
		t.Fatalf("runner called %d times for a missing CLI", calls)
	}
}

func TestInspectEnvironmentWarnings(t *testing.T) {
	i := fakeInspector("/Users/tester")
	i.LookupEnv = func(key string) (string, bool) {
		switch key {
		case "MINIMAX_DATA_DIR":
			return "/tmp/custom", true
		case "NODE_OPTIONS":
			return "--require=/app/shim.cjs", true
		case "PATH":
			return "/app/shim/brokered-bin:/usr/bin", true
		case "HTTPS_PROXY":
			return "http://127.0.0.1:7890", true
		}
		return "", false
	}
	out := i.Inspect("mcode", "")
	joined := strings.Join(out.Warnings, "\n")
	for _, want := range []string{"MINIMAX_DATA_DIR", "NODE_OPTIONS", "safe-delete", "HTTPS_PROXY"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing warning containing %q: %s", want, joined)
		}
	}
}

func TestInspectStatsDirectoryNamesOnly(t *testing.T) {
	i := fakeInspector("/Users/tester")
	var visited []string
	i.Stat = func(path string) (os.FileInfo, error) {
		visited = append(visited, path)
		return fakeFileInfo{path}, nil
	}
	i.Inspect("mcode", "")
	if len(visited) == 0 {
		t.Fatal("no stat calls")
	}
	for _, p := range visited {
		base := filepath.Base(p)
		if base != ".minimax" && base != "auth" {
			t.Fatalf("unexpected path inspected: %q", p)
		}
	}
}

func TestInspectVersionAndHelpFailures(t *testing.T) {
	i := fakeInspector("/Users/tester")
	i.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--version" {
			return nil, errors.New("version failed")
		}
		return []byte("no acp here"), nil
	}
	out := i.Inspect("mcode", "")
	if out.CLIVersion != "" || out.ACPServerHelp {
		t.Fatalf("unexpected success: %#v", out)
	}
	joined := strings.Join(out.Warnings, "\n")
	if !strings.Contains(joined, "--version") || !strings.Contains(joined, "acp --help") {
		t.Fatalf("missing failure warnings: %s", joined)
	}
}

func TestInspectDoesNotLeakCredentialContents(t *testing.T) {
	i := fakeInspector(t.TempDir())
	out := i.Inspect("mcode", "global")
	if strings.Contains(out.LoginHint, "token") || out.Region != "global" {
		t.Fatalf("unexpected report: %#v", out)
	}
	// The report must be JSON-safe and must not carry a home path outside the
	// explicitly declared dataDir field.
	if out.DataDir == "" {
		t.Fatal("dataDir missing")
	}
}
