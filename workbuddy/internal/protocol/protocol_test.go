package protocol

import (
	"io"
	"strings"
	"testing"
)

func TestFrameIsSingleLineAndCarriesJSONRPC(t *testing.T) {
	encoded, err := Frame(Message{"id": 1, "method": "initialize", "params": Message{"a": "中"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), "\n") != 1 {
		t.Fatalf("frame must contain exactly one newline: %q", encoded)
	}
	message, err := ParseFrame(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if message["jsonrpc"] != "2.0" {
		t.Fatalf("unexpected jsonrpc: %v", message["jsonrpc"])
	}
	params, _ := message["params"].(map[string]any)
	if params["a"] != "中" {
		t.Fatalf("UTF-8 content was escaped or lost: %q", encoded)
	}
	if strings.Contains(string(encoded), `\u`) {
		t.Fatalf("frame should not escape non-ASCII: %q", encoded)
	}
}

func TestParseFrameRejectsMalformed(t *testing.T) {
	for _, line := range []string{`{"jsonrpc":"1.0","id":1}`, `[]`, `not json`, `null`} {
		if _, err := ParseFrame([]byte(line)); err == nil {
			t.Errorf("expected %q to be rejected", line)
		}
	}
}

func TestRPCErrorKeepsOptionalData(t *testing.T) {
	if got := RPCError(7, -32601, "nope", nil)["error"].(Message)["code"]; got != -32601 {
		t.Fatalf("unexpected code: %v", got)
	}
	response := RPCError(7, -32000, "busy", Message{"reason": "writer_occupied"})
	errorObject := response["error"].(Message)
	data := errorObject["data"].(Message)
	if data["reason"] != "writer_occupied" {
		t.Fatalf("data lost: %v", errorObject)
	}
	if _, present := RPCError(1, -1, "x", nil)["error"].(Message)["data"]; present {
		t.Fatal("data must be omitted when nil")
	}
}

func TestSSEDecodesCommentsMultilineDataAndBadJSON(t *testing.T) {
	stream := ":ok\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\n" +
		"data: \"id\":1,\"result\":{}}\n\ndata: {broken\n\n"
	decoder := NewSSEDecoder(strings.NewReader(stream))
	message, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if message["id"] != float64(1) {
		t.Fatalf("unexpected message: %v", message)
	}
	if _, err := decoder.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestSSECommitsTrailingBlockWithoutBlankLine(t *testing.T) {
	decoder := NewSSEDecoder(strings.NewReader(`data: {"a":1}` + "\n"))
	message, err := decoder.Next()
	if err != nil || message["a"] != float64(1) {
		t.Fatalf("unexpected result: %v %v", message, err)
	}
	if _, err := decoder.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestSSEIgnoresEventIdAndRetryLines(t *testing.T) {
	stream := "event: message\nid: 7\nretry: 1000\n:ok\ndata: {\"id\":7}\n\n"
	message, err := NewSSEDecoder(strings.NewReader(stream)).Next()
	if err != nil || message["id"] != float64(7) {
		t.Fatalf("unexpected result: %v %v", message, err)
	}
}

func TestIDValuesEqualAcrossNumericRepresentations(t *testing.T) {
	if !IDValuesEqual(float64(3), 3) || !IDValuesEqual("wb-1", "wb-1") {
		t.Fatal("equivalent ids must match")
	}
	if IDValuesEqual(float64(3), "3") || IDValuesEqual(nil, 3) {
		t.Fatal("different ids must not match")
	}
}
