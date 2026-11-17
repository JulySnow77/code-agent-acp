package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestACPFrameAddsJSONRPCVersion(t *testing.T) {
	body, err := ACPFrame(Message{"id": 1, "method": "initialize"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Errorf("frame must end with a newline: %q", body)
	}
	message, err := ParseFrame(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if message["jsonrpc"] != JSONRPCVersion {
		t.Errorf("jsonrpc = %v", message["jsonrpc"])
	}
	if _, err := ParseFrame([]byte(`{"id":1,"method":"initialize"}`), true); err == nil {
		t.Error("ACP parser must reject frames without jsonrpc:2.0")
	}
}

func TestStreamFrameOmitsJSONRPCVersion(t *testing.T) {
	body, err := StreamFrame(Message{"type": "user", "jsonrpc": "2.0"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := ParseFrame(body, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := message["jsonrpc"]; present {
		t.Errorf("stream-json frame should not add jsonrpc: %s", body)
	}
	if message["type"] != "user" {
		t.Errorf("type = %v", message["type"])
	}
}

func TestParseFrameRejectsGarbage(t *testing.T) {
	for _, line := range []string{"", "not json", "null", "[]"} {
		if _, err := ParseFrame([]byte(line), false); err == nil {
			t.Errorf("expected %q to be rejected", line)
		}
	}
}

func TestIDKeyDistinguishesStringAndNumber(t *testing.T) {
	if IDKey(1) == IDKey("1") {
		t.Error("JSON-RPC ids 1 and \"1\" must not collide")
	}
	if IDKey(float64(7)) != IDKey(int64(7)) {
		t.Error("numeric ids decoded from JSON must match their Go counterparts")
	}
}

func TestResultAndErrorFrame(t *testing.T) {
	result := Result("abc", Message{"ok": true})
	if result["id"] != "abc" {
		t.Errorf("id = %v", result["id"])
	}
	errFrame := RPCErrorResponse("abc", CodeMethodNotFound, "nope", Message{"method": "x"})
	object := AsMessage(errFrame["error"])
	if Num(object, "code") != CodeMethodNotFound {
		t.Errorf("error code = %v", object["code"])
	}
}

func TestAsMessageAndAsSliceAcceptBothShapes(t *testing.T) {
	if got := AsMessage(map[string]any{"a": 1}); got["a"] != 1 {
		t.Errorf("decoded object: %v", got)
	}
	if got := AsMessage(Message{"a": 2}); got["a"] != 2 {
		t.Errorf("named Message: %v", got)
	}
	if got := AsSlice([]Message{{"a": 1}}); len(got) != 1 || AsMessage(got[0])["a"] != 1 {
		t.Errorf("[]Message: %v", got)
	}
}

func TestWireLogRecordsFrames(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenWireLog(dir, "unit")
	if err != nil {
		t.Fatal(err)
	}
	log.Record("in", []byte(`{"method":"initialize"}`))
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("wire log files: %v %v", entries, err)
	}
	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"direction":"in"`) {
		t.Errorf("wire log content = %s", body)
	}
	var nilLog *WireLog
	nilLog.Record("in", nil)
	if err := nilLog.Close(); err != nil {
		t.Errorf("nil log Close = %v", err)
	}
}
