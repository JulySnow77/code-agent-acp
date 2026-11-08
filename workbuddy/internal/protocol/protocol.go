// Package protocol implements the JSON-RPC 2.0 framing and SSE decoding used
// on both sides of the WorkBuddy ACP bridge.
package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const (
	// Version is the only ACP protocol version served by this bridge.
	Version = 1
	// AgentName is the stable identity marker checked by ACP hosts.
	AgentName = "workbuddy-acp-bridge"
	// VersionString is reported by `version` and `initialize`.
	VersionString = "00.00.01"
)

// Message is one JSON-RPC 2.0 object.
type Message map[string]any

// Encode serializes a value as compact UTF-8 JSON without HTML escaping,
// matching the wire format used by the WorkBuddy local gateway.
func Encode(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// Frame serializes one ndjson frame. A frame never contains a bare newline.
func Frame(message Message) ([]byte, error) {
	out := make(Message, len(message)+1)
	for key, value := range message {
		out[key] = value
	}
	out["jsonrpc"] = "2.0"
	body, err := Encode(out)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// ParseFrame validates and decodes one ndjson frame.
func ParseFrame(line []byte) (Message, error) {
	var message Message
	if err := json.Unmarshal(line, &message); err != nil {
		return nil, err
	}
	if message == nil || message["jsonrpc"] != "2.0" {
		return nil, errors.New("ACP frame must be a JSON-RPC 2.0 object")
	}
	return message, nil
}

// RPCError builds a JSON-RPC error response. `data` may be nil.
func RPCError(id any, code int, message string, data any) Message {
	errorObject := Message{"code": code, "message": message}
	if data != nil {
		errorObject["data"] = data
	}
	return Message{"id": id, "error": errorObject}
}

// Result builds a successful JSON-RPC response.
func Result(id any, result Message) Message {
	if result == nil {
		result = Message{}
	}
	return Message{"id": id, "result": result}
}

// IDValuesEqual compares JSON-RPC ids across Go/JSON numeric representations.
func IDValuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	left, err := Encode(a)
	if err != nil {
		return false
	}
	right, err := Encode(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// SSEDecoder incrementally decodes JSON-RPC payloads carried by the `data:`
// lines of an SSE stream. Comments (`:ok`), `event:`, `id:` and `retry:`
// lines are ignored; a blank line commits the accumulated data block.
type SSEDecoder struct {
	scanner *bufio.Scanner
	data    []string
}

// NewSSEDecoder wraps a streaming HTTP response body.
func NewSSEDecoder(reader io.Reader) *SSEDecoder {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &SSEDecoder{scanner: scanner}
}

// Next returns the next JSON-RPC object, or io.EOF when the stream ends.
// Invalid JSON blocks and non-object payloads are skipped rather than
// aborting the stream.
func (d *SSEDecoder) Next() (Message, error) {
	for d.scanner.Scan() {
		line := d.scanner.Text()
		if line == "" {
			message := d.commit()
			if message != nil {
				return message, nil
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		if len(line) >= 5 && line[:5] == "data:" {
			payload := line[5:]
			if len(payload) > 0 && payload[0] == ' ' {
				payload = payload[1:]
			}
			d.data = append(d.data, payload)
		}
	}
	if err := d.scanner.Err(); err != nil {
		return nil, err
	}
	if message := d.commit(); message != nil {
		return message, nil
	}
	return nil, io.EOF
}

func (d *SSEDecoder) commit() Message {
	if len(d.data) == 0 {
		return nil
	}
	text := ""
	for index, line := range d.data {
		if index > 0 {
			text += "\n"
		}
		text += line
	}
	d.data = nil
	var message Message
	if err := json.Unmarshal([]byte(text), &message); err != nil || message == nil {
		return nil
	}
	return message
}
