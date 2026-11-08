package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSession(t *testing.T, root, name string, values map[string]any) {
	t.Helper()
	row := map[string]any{
		"pid": 100, "kind": "interactive", "sessionId": "session-" + name,
		"cwd": "/tmp/project", "url": "http://127.0.0.1:64250",
		"lastHeartbeat": 100,
	}
	for key, value := range values {
		row[key] = value
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyLiveLoopbackInteractiveSessionsAreReturned(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "live", map[string]any{"lastHeartbeat": 200})
	writeSession(t, root, "idle", map[string]any{"kind": "prewarm", "lastHeartbeat": 300})
	writeSession(t, root, "remote", map[string]any{"url": "https://example.com/acp"})
	writeSession(t, root, "path", map[string]any{"url": "http://127.0.0.1:64250/api/v1/acp"})
	if err := os.WriteFile(filepath.Join(root, "broken.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := ReadSessions(root)
	if len(rows) != 1 || rows[0].SessionID != "session-live" {
		t.Fatalf("unexpected sessions: %+v", rows)
	}
	if rows[0].Endpoint != "http://127.0.0.1:64250" || rows[0].Pid != 100 {
		t.Fatalf("endpoint or pid lost: %+v", rows[0])
	}
}

func TestEndpointValidationRejectsNonLoopbackAndPaths(t *testing.T) {
	for _, url := range []string{
		"https://127.0.0.1:1234", "http://192.168.1.1:1234",
		"http://127.0.0.1:1234/path", "http://127.0.0.1:0", "",
		"http://127.0.0.1:1234?x=1", "http://user@127.0.0.1:1234",
	} {
		if IsLoopbackEndpoint(url) {
			t.Errorf("expected %q to be rejected", url)
		}
	}
	for _, url := range []string{"http://127.0.0.1:64250", "http://localhost:64250/", "http://[::1]:64250"} {
		if !IsLoopbackEndpoint(url) {
			t.Errorf("expected %q to be accepted", url)
		}
	}
}

func TestSelectBySessionIDCwdAndRecency(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "old", nil)
	writeSession(t, root, "new", map[string]any{"lastHeartbeat": 900, "cwd": "/tmp/other"})
	if got, err := Select(Options{Directory: root}); err != nil || got.SessionID != "session-new" {
		t.Fatalf("recency selection failed: %+v %v", got, err)
	}
	if got, err := Select(Options{SessionID: "session-old", Directory: root}); err != nil || got.SessionID != "session-old" {
		t.Fatalf("session id selection failed: %+v %v", got, err)
	}
	if got, err := Select(Options{Cwd: "/tmp/other", Directory: root}); err != nil || got.SessionID != "session-new" {
		t.Fatalf("cwd selection failed: %+v %v", got, err)
	}
	if _, err := Select(Options{SessionID: "missing", Directory: root}); err == nil ||
		!strings.Contains(err.Error(), "no live loopback endpoint") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := Select(Options{Cwd: "/tmp/missing", Directory: root}); err == nil ||
		!strings.Contains(err.Error(), "no live interactive session") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExplicitEndpointIsValidatedAndNeverReadFromDisk(t *testing.T) {
	root := t.TempDir()
	got, err := Select(Options{Endpoint: "http://127.0.0.1:9999", SessionID: "manual", Directory: root})
	if err != nil || got.Endpoint != "http://127.0.0.1:9999" || got.SessionID != "manual" || !got.Explicit {
		t.Fatalf("explicit endpoint failed: %+v %v", got, err)
	}
	if _, err := Select(Options{Endpoint: "http://example.com", Directory: root}); err == nil {
		t.Fatal("remote endpoint must be rejected")
	}
	if rows := ReadSessions(filepath.Join(root, "missing")); len(rows) != 0 {
		t.Fatalf("missing directory must yield no sessions, got %+v", rows)
	}
	if _, err := Select(Options{Directory: filepath.Join(root, "missing")}); err == nil ||
		!strings.Contains(err.Error(), "no live interactive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHeartbeatAcceptsMixedNumericForms(t *testing.T) {
	for _, row := range []Session{
		{LastHeartbeat: float64(42)}, {LastHeartbeat: 43},
		{LastHeartbeat: "44"}, {LastHeartbeat: json.Number("45")},
		{LastHeartbeat: nil},
	} {
		if row.Heartbeat() < 0 {
			t.Fatalf("negative heartbeat for %v", row.LastHeartbeat)
		}
	}
	if (Session{LastHeartbeat: "44"}).Heartbeat() != 44 {
		t.Fatal("string heartbeat must be parsed")
	}
}
