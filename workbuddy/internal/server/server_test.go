package server_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"workbuddy-acp-bridge/internal/backend"
	"workbuddy-acp-bridge/internal/protocol"
	"workbuddy-acp-bridge/internal/server"
	"workbuddy-acp-bridge/internal/testutil"
)

var (
	initFrame = protocol.Message{
		"id": 1, "method": "initialize", "params": protocol.Message{"protocolVersion": 1},
	}
	newFrame = protocol.Message{
		"id": 2, "method": "session/new", "params": protocol.Message{"cwd": "/tmp", "mcpServers": []any{}},
	}
	modeFrame = protocol.Message{
		"id": 3, "method": "session/set_mode",
		"params": protocol.Message{"sessionId": "gateway-session", "modeId": "plan"},
	}
	configFrame = protocol.Message{
		"id": 4, "method": "session/set_config_option",
		"params": protocol.Message{"sessionId": "gateway-session", "configId": "model", "value": "provider/model"},
	}
	promptFrame = protocol.Message{
		"id": 5, "method": "session/prompt",
		"params": protocol.Message{
			"sessionId": "gateway-session",
			"prompt":    []any{protocol.Message{"type": "text", "text": "Return the result"}},
		},
	}
)

func runServer(frames string, bridge backend.Backend, permission string) (int, []protocol.Message) {
	var output bytes.Buffer
	code := server.New(bridge, strings.NewReader(frames), &output, permission).Run()
	messages := []protocol.Message{}
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var message protocol.Message
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			panic(err)
		}
		messages = append(messages, message)
	}
	return code, messages
}

func replyByID(messages []protocol.Message, id any) protocol.Message {
	for _, message := range messages {
		if _, hasMethod := message["method"]; hasMethod {
			continue
		}
		if raw, present := message["id"]; present && protocol.IDValuesEqual(raw, id) {
			return message
		}
	}
	return nil
}

func messageMap(value any) protocol.Message {
	if typed, ok := value.(map[string]any); ok {
		return protocol.Message(typed)
	}
	return protocol.Message{}
}

func updates(messages []protocol.Message) []protocol.Message {
	result := []protocol.Message{}
	for _, message := range messages {
		if message["method"] == "session/update" {
			result = append(result, messageMap(messageMap(message["params"])["update"]))
		}
	}
	return result
}

func newLocal(t *testing.T, gateway *testutil.FakeGateway) *backend.Local {
	t.Helper()
	bridge, err := backend.NewLocal(backend.LocalOptions{
		Endpoint: gateway.Endpoint(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return bridge
}

func TestFullRoundTripPreservesACPContractAndStreamsUpdates(t *testing.T) {
	gateway := testutil.NewFakeGateway()
	defer gateway.Close()
	frames := testutil.ClientFrames(initFrame, newFrame, modeFrame, configFrame, promptFrame)
	code, messages := runServer(frames, newLocal(t, gateway), "deny")
	if code != 0 {
		t.Fatalf("unexpected exit code %d", code)
	}
	first := messageMap(replyByID(messages, 1)["result"])
	if first["protocolVersion"] != float64(1) {
		t.Fatalf("unexpected initialize result: %v", first)
	}
	if messageMap(first["agentCapabilities"])["loadSession"] != true {
		t.Fatal("loadSession must be advertised")
	}
	meta := messageMap(messageMap(first["_meta"])["workbuddy"])
	if meta["modelSelectionVerified"] != false {
		t.Fatal("model selection must not be claimed as verified")
	}
	if messageMap(replyByID(messages, 2)["result"])["sessionId"] != "gateway-session" {
		t.Fatalf("unexpected session/new reply: %v", replyByID(messages, 2))
	}
	if len(messageMap(replyByID(messages, 3)["result"])) != 0 {
		t.Fatalf("set_mode must return an empty result: %v", replyByID(messages, 3))
	}
	options, _ := messageMap(replyByID(messages, 4)["result"])["configOptions"].([]any)
	if len(options) != 1 || messageMap(options[0])["currentValue"] != "provider/model" {
		t.Fatalf("config option not echoed: %v", replyByID(messages, 4))
	}
	if messageMap(replyByID(messages, 5)["result"])["stopReason"] != "end_turn" {
		t.Fatalf("unexpected prompt result: %v", replyByID(messages, 5))
	}
	foundResult := false
	for _, update := range updates(messages) {
		if update["sessionUpdate"] != "agent_message_chunk" {
			continue
		}
		if text, _ := messageMap(update["content"])["text"].(string); text == `{"value":"works"}` {
			foundResult = true
		}
	}
	if !foundResult {
		t.Fatalf("structured result was not streamed: %v", updates(messages))
	}
	methods := strings.Join(gateway.Methods(), ",")
	if !strings.Contains(methods, "session/new") || !strings.Contains(methods, "session/prompt") {
		t.Fatalf("downstream methods missing: %s", methods)
	}
	for _, accept := range gateway.Accepts() {
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			t.Fatalf("dual Accept header missing: %q", accept)
		}
	}
}

func TestWriterOccupiedErrorIsPropagatedNotSwallowed(t *testing.T) {
	gateway := testutil.NewFakeGateway()
	defer gateway.Close()
	load := protocol.Message{
		"id": 2, "method": "session/load",
		"params": protocol.Message{"sessionId": "occupied", "cwd": "/tmp", "mcpServers": []any{}},
	}
	_, messages := runServer(testutil.ClientFrames(initFrame, load), newLocal(t, gateway), "deny")
	errorObject := messageMap(replyByID(messages, 2)["error"])
	if !strings.Contains(fmtText(errorObject["message"]), "external writer") {
		t.Fatalf("error message lost: %v", errorObject)
	}
	if messageMap(errorObject["data"])["reason"] != "writer_occupied" {
		t.Fatalf("error data lost: %v", errorObject)
	}
}

func fmtText(value any) string {
	text, _ := value.(string)
	return text
}

func TestDeniedPermissionIsAnsweredBackToTheGateway(t *testing.T) {
	gateway := testutil.NewFakeGateway()
	defer gateway.Close()
	gateway.SetEmitPermission(true)
	frames := testutil.ClientFrames(initFrame, newFrame, promptFrame)
	_, messages := runServer(frames, newLocal(t, gateway), "deny")
	if messageMap(replyByID(messages, 5)["result"])["stopReason"] != "end_turn" {
		t.Fatalf("prompt did not finish: %v", replyByID(messages, 5))
	}
	replies := gateway.Replies()
	if len(replies) != 1 {
		t.Fatalf("expected one permission answer, got %v", replies)
	}
	result := messageMap(replies[0]["result"])
	if nested, ok := result["outcome"].(map[string]any); !ok || nested["outcome"] != "cancelled" {
		t.Fatalf("permission was not denied: %v", result)
	}
}

func TestClientPermissionModeForwardsTheDecisionToTheACPClient(t *testing.T) {
	gateway := testutil.NewFakeGateway()
	defer gateway.Close()
	gateway.SetEmitPermission(true)
	upstreamReply := protocol.Message{
		"id": "wb-1", "result": protocol.Message{
			"outcome": protocol.Message{"outcome": "selected", "optionId": "allow"},
		},
	}
	frames := testutil.ClientFrames(initFrame, newFrame, promptFrame, upstreamReply)
	_, messages := runServer(frames, newLocal(t, gateway), "client")
	outbound := 0
	for _, message := range messages {
		if message["method"] == "session/request_permission" {
			outbound++
		}
	}
	if outbound != 1 {
		t.Fatalf("expected one upstream permission request, got %d", outbound)
	}
	replies := gateway.Replies()
	if len(replies) != 1 {
		t.Fatalf("expected one permission answer, got %v", replies)
	}
	nested := messageMap(messageMap(replies[0]["result"])["outcome"])
	if nested["outcome"] != "selected" {
		t.Fatalf("client decision was not forwarded: %v", replies[0])
	}
}

func TestDeferredFramesDoNotBreakPermissionReplies(t *testing.T) {
	gateway := testutil.NewFakeGateway()
	defer gateway.Close()
	gateway.SetEmitPermission(true)
	unrelated := protocol.Message{"method": "session/unknown", "params": protocol.Message{}}
	upstreamReply := protocol.Message{
		"id": "wb-1", "result": protocol.Message{"outcome": protocol.Message{"outcome": "cancelled"}},
	}
	frames := testutil.ClientFrames(initFrame, newFrame, promptFrame, unrelated, upstreamReply)
	_, messages := runServer(frames, newLocal(t, gateway), "client")
	if messageMap(replyByID(messages, 5)["result"])["stopReason"] != "end_turn" {
		t.Fatalf("prompt did not finish: %v", replyByID(messages, 5))
	}
	if len(gateway.Replies()) != 1 {
		t.Fatalf("permission answer lost: %v", gateway.Replies())
	}
}

func TestFakeBackendNeedsNoNetworkAndSupportsLoadAndCancel(t *testing.T) {
	bridge, err := backend.NewFake(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	load := protocol.Message{
		"id": 2, "method": "session/load",
		"params": protocol.Message{"sessionId": "restored", "mcpServers": []any{}},
	}
	cancel := protocol.Message{"method": "session/cancel", "params": protocol.Message{"sessionId": "restored"}}
	frames := testutil.ClientFrames(initFrame, load, configFrame, promptFrame, cancel)
	code, messages := runServer(frames, bridge, "deny")
	if code != 0 || messageMap(replyByID(messages, 5)["result"])["stopReason"] != "end_turn" {
		t.Fatalf("unexpected run: %d %v", code, messages)
	}
	if bridge.SessionID() != "restored" {
		t.Fatalf("session not restored: %s", bridge.SessionID())
	}
	calls := strings.Join(bridge.Calls(), ",")
	if !strings.Contains(calls, "session/load") || !strings.Contains(calls, "session/cancel") {
		t.Fatalf("missing calls: %s", calls)
	}
}

func TestMalformedFramesAndUnknownMethodsDoNotCrashTheServer(t *testing.T) {
	bridge, err := backend.NewFake(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	unknown := protocol.Message{"id": 9, "method": "session/unknown", "params": protocol.Message{}}
	frames := "{broken\n" + testutil.ClientFrames(unknown, initFrame)
	code, messages := runServer(frames, bridge, "deny")
	if code != 0 {
		t.Fatalf("unexpected exit code %d", code)
	}
	if messageMap(replyByID(messages, 9)["error"])["code"] != float64(-32601) {
		t.Fatalf("unknown method not reported: %v", replyByID(messages, 9))
	}
	if messageMap(replyByID(messages, 1)["result"])["protocolVersion"] != float64(1) {
		t.Fatalf("server did not recover: %v", replyByID(messages, 1))
	}
}

func TestInvalidPromptPayloadAndVersionAreRejected(t *testing.T) {
	bridge, err := backend.NewFake(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := protocol.Message{
		"id": 2, "method": "session/prompt", "params": protocol.Message{"prompt": "not-an-array"},
	}
	badVersion := protocol.Message{
		"id": 3, "method": "initialize", "params": protocol.Message{"protocolVersion": 2},
	}
	_, messages := runServer(testutil.ClientFrames(initFrame, bad, badVersion), bridge, "deny")
	if messageMap(replyByID(messages, 2)["error"])["code"] != float64(-32602) {
		t.Fatalf("invalid prompt not rejected: %v", replyByID(messages, 2))
	}
	if messageMap(replyByID(messages, 3)["error"])["code"] != float64(-32600) {
		t.Fatalf("invalid protocol version not rejected: %v", replyByID(messages, 3))
	}
}
