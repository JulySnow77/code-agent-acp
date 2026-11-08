// Command fake-acp-server is a minimal standard ACP v1 stdio bridge fixture.
//
// It deliberately speaks only ACP v1 and advertises no `_zcode` private
// extension, so a downstream ACP client cannot accidentally depend on ZCode
// behavior. Its `version` output is also not a ZCode bridge. It exists for
// protocol cross-checking only and is not part of the main test chain.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"workbuddy-acp-bridge/internal/protocol"
)

const fixtureSession = "workbuddy-test-session"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("workbuddy-acp-bridge fixture")
		return
	}
	result := fixtureResult()
	session := fixtureSession
	pending := any(nil)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var message protocol.Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		method, _ := message["method"].(string)
		params := protocol.Message{}
		if raw, ok := message["params"].(map[string]any); ok {
			params = protocol.Message(raw)
		}
		identifier := message["id"]
		if method == "session/cancel" {
			send(protocol.Message{"id": pending, "result": protocol.Message{"stopReason": "cancelled"}})
			continue
		}
		if method == "" {
			continue
		}
		response := protocol.Message{}
		switch method {
		case "initialize":
			response = protocol.Message{
				"protocolVersion": 1,
				"agentInfo":       protocol.Message{"name": "fake-workbuddy"},
				"agentCapabilities": protocol.Message{
					"sessionCapabilities": protocol.Message{"resume": protocol.Message{}},
				},
			}
		case "session/new":
			response = protocol.Message{"sessionId": session}
		case "session/resume":
			if value, ok := params["sessionId"].(string); ok {
				session = value
			}
		case "session/set_config_option":
			response = protocol.Message{"configOptions": []any{
				protocol.Message{"id": "model", "currentValue": params["value"]},
			}}
		case "session/prompt":
			pending = identifier
			send(protocol.Message{
				"method": "session/update",
				"params": protocol.Message{
					"sessionId": session,
					"update": protocol.Message{
						"sessionUpdate": "agent_message_chunk",
						"content": protocol.Message{
							"type": "text", "text": string(result),
						},
					},
				},
			})
			response = protocol.Message{"stopReason": "end_turn"}
		}
		send(protocol.Message{"id": identifier, "result": response})
	}
}

func fixtureResult() []byte {
	raw := os.Getenv("AUTO_RUN_FAKE_WB_RESULT")
	if raw == "" {
		raw = `{"value":"works"}`
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		value = protocol.Message{"value": "works"}
	}
	encoded, _ := protocol.Encode(value)
	return encoded
}

func send(message protocol.Message) {
	body, err := protocol.Frame(message)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	_, _ = os.Stdout.Write(body)
}
