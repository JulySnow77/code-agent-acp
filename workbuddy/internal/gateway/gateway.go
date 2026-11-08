// Package gateway is the HTTP/SSE JSON-RPC client for the WorkBuddy local
// ACP gateway (verified against WorkBuddy 2.165.0 on macOS, 2026-11-08).
//
//	POST /api/v1/acp/connect  {} -> {"connectionId":..., "sessionToken":...}
//	POST /api/v1/acp
//	    Content-Type: application/json
//	    Accept: application/json, text/event-stream   (both are mandatory)
//	    Acp-Connection-Id: <connectionId>
//	    acp-session-token: <sessionToken>
//	    x-codebuddy-request: 1
//	The response body of the POST itself is an SSE stream carrying
//	`session/update` notifications and the final id-matched result.
//
// The official hosted Open API uses a separate GET SSE channel plus POST;
// this client targets the local loopback gateway only.
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy-acp-bridge/internal/protocol"
)

// Error classifies gateway failures so backends can map them to ACP errors.
type Error struct {
	Message  string
	Category string
	Data     any
}

func (e *Error) Error() string { return e.Message }

// Client is a stateful connection to one WorkBuddy loopback gateway.
type Client struct {
	endpoint     string
	timeout      time.Duration
	http         *http.Client
	connectionID string
	sessionToken string
	mu           sync.Mutex
	sequence     int
}

// New obtains one-time loopback credentials. No long-lived key is stored.
func New(endpoint string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Only the dial and response-header phases are time bounded. A prompt can
	// stream for much longer than the timeout; an overall http.Client.Timeout
	// would truncate a valid long turn, unlike the Python transport's
	// per-read socket timeout.
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: timeout,
	}
	client := &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		timeout:  timeout,
		http:     &http.Client{Transport: transport},
	}
	connectionID, sessionToken, err := client.connect()
	if err != nil {
		return nil, err
	}
	client.connectionID, client.sessionToken = connectionID, sessionToken
	return client, nil
}

func (c *Client) open(path string, body any, auth bool) (*http.Response, error) {
	payload, err := protocol.Encode(body)
	if err != nil {
		return nil, &Error{Message: err.Error(), Category: "network"}
	}
	request, err := http.NewRequest(http.MethodPost, c.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Message: err.Error(), Category: "network"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if auth {
		request.Header.Set("Acp-Connection-Id", c.connectionID)
		request.Header.Set("acp-session-token", c.sessionToken)
		request.Header.Set("x-codebuddy-request", "1")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &Error{
			Message:  fmt.Sprintf("ACP gateway is unreachable at %s: %v", c.endpoint, err),
			Category: "network",
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 400))
		response.Body.Close()
		return nil, &Error{
			Message:  fmt.Sprintf("ACP gateway returned HTTP %d: %s", response.StatusCode, string(detail)),
			Category: "http",
		}
	}
	return response, nil
}

func (c *Client) connect() (string, string, error) {
	response, err := c.open("/api/v1/acp/connect", protocol.Message{}, false)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", "", &Error{Message: "connect returned invalid JSON: " + err.Error(), Category: "capability"}
	}
	connectionID, _ := payload["connectionId"].(string)
	sessionToken, _ := payload["sessionToken"].(string)
	if connectionID == "" || sessionToken == "" {
		return "", "", &Error{Message: "connect did not return connectionId and sessionToken", Category: "capability"}
	}
	return connectionID, sessionToken, nil
}

// Initialize negotiates ACP v1 with the local gateway.
func (c *Client) Initialize() (protocol.Message, error) {
	return c.Call("initialize", protocol.Message{
		"protocolVersion": protocol.Version,
		"clientInfo": protocol.Message{
			"name":    protocol.AgentName,
			"version": protocol.VersionString,
		},
		"clientCapabilities": protocol.Message{
			"fs": protocol.Message{"readTextFile": false, "writeTextFile": false},
		},
	}, nil, nil)
}

// Callbacks receive streamed server notifications and server-to-client requests.
type EventFunc func(protocol.Message)
type RequestFunc func(protocol.Message)

// Call sends one JSON-RPC request and returns its result, streaming events.
func (c *Client) Call(method string, params any, onEvent EventFunc, onRequest RequestFunc) (protocol.Message, error) {
	if params == nil {
		params = protocol.Message{}
	}
	c.mu.Lock()
	c.sequence++
	identifier := c.sequence
	c.mu.Unlock()

	message := protocol.Message{
		"jsonrpc": "2.0",
		"id":      identifier,
		"method":  method,
		"params":  params,
	}
	response, err := c.open("/api/v1/acp", message, true)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	decoder := protocol.NewSSEDecoder(response.Body)
	for {
		payload, err := decoder.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &Error{Message: "cannot read ACP SSE stream: " + err.Error(), Category: "stream"}
		}
		if _, hasMethod := payload["method"]; hasMethod {
			if onEvent != nil {
				onEvent(payload)
			}
			if onRequest != nil {
				if _, hasID := payload["id"]; hasID {
					onRequest(payload)
				}
			}
			continue
		}
		if !protocol.IDValuesEqual(payload["id"], identifier) {
			continue
		}
		if errorObject, ok := payload["error"]; ok {
			detail, _ := errorObject.(map[string]any)
			text := "ACP gateway error"
			var data any
			if detail != nil {
				if value, ok := detail["message"].(string); ok {
					text = value
				}
				data = detail["data"]
			}
			return nil, &Error{Message: text, Category: "acp_error", Data: data}
		}
		if result, ok := payload["result"].(map[string]any); ok {
			return protocol.Message(result), nil
		}
		return protocol.Message{}, nil
	}
	return nil, &Error{
		Message:  fmt.Sprintf("gateway closed the SSE stream before the %s response", method),
		Category: "stream",
	}
}

// Respond answers a server-to-client request (e.g. a permission prompt).
func (c *Client) Respond(identifier any, result any) error {
	message := protocol.Message{"jsonrpc": "2.0", "id": identifier, "result": result}
	response, err := c.open("/api/v1/acp", message, true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = protocol.NewSSEDecoder(response.Body).Next()
	return nil
}
