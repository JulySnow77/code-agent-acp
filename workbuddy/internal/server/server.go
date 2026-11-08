// Package server implements the ACP v1 stdio (ndjson) server facing any ACP
// client. It also issues `session/request_permission` calls back to its client
// and waits for the reply without losing frames that arrive meanwhile.
package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"

	"workbuddy-acp-bridge/internal/backend"
	"workbuddy-acp-bridge/internal/protocol"
)

// Server reads JSON-RPC 2.0 frames from stdin and writes frames to stdout.
type Server struct {
	backend    backend.Backend
	reader     *bufio.Reader
	writer     io.Writer
	writeMu    sync.Mutex
	clientSeq  int
	deferred   []protocol.Message
	sessionID  string
	permission string
}

// New builds a stdio server. `permissionMode` is either deny or client.
func New(bridge backend.Backend, input io.Reader, output io.Writer, permissionMode string) *Server {
	bridge.SetPermissionMode(permissionMode)
	return &Server{
		backend:    bridge,
		reader:     bufio.NewReader(input),
		writer:     output,
		permission: permissionMode,
	}
}

func (s *Server) send(message protocol.Message) error {
	body, err := protocol.Frame(message)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.writer.Write(body); err != nil {
		return err
	}
	if flusher, ok := s.writer.(interface{ Flush() error }); ok {
		return flusher.Flush()
	}
	return nil
}

func (s *Server) notify(method string, params protocol.Message) {
	if err := s.send(protocol.Message{"method": method, "params": params}); err != nil {
		backend.Log("cannot send %s: %v", method, err)
	}
}

func (s *Server) emitUpdate(update protocol.Message) {
	if update == nil {
		return
	}
	s.notify("session/update", protocol.Message{
		"sessionId": s.sessionID, "update": update,
	})
}

// readFrame reads the next frame from stdin only. A frame that was deferred
// while waiting for a permission reply must never be fed back to that same
// waiter: doing so would pop and re-queue it forever in a busy loop.
func (s *Server) readFrame() (protocol.Message, error) {
	for {
		line, err := s.reader.ReadString('\n')
		if err == io.EOF && line == "" {
			return nil, io.EOF
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		message, parseErr := protocol.ParseFrame([]byte(line))
		if parseErr != nil {
			backend.Log("ignoring malformed client frame: %v", parseErr)
			if err == io.EOF {
				return nil, io.EOF
			}
			continue
		}
		return message, nil
	}
}

// read prefers frames deferred by a previous server-to-client waiter.
func (s *Server) read() (protocol.Message, error) {
	if len(s.deferred) > 0 {
		message := s.deferred[0]
		s.deferred = s.deferred[1:]
		return message, nil
	}
	return s.readFrame()
}

// callClient issues a server-to-client JSON-RPC request and synchronously
// awaits its reply. Unrelated frames are queued for the main loop.
func (s *Server) callClient(method string, params protocol.Message) (protocol.Message, error) {
	s.clientSeq++
	identifier := fmt.Sprintf("wb-%d", s.clientSeq)
	if err := s.send(protocol.Message{"id": identifier, "method": method, "params": params}); err != nil {
		return nil, &backend.Error{Text: err.Error(), Code: -32004}
	}
	for {
		message, err := s.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, &backend.Error{
					Text: "client closed stdin while a permission reply was pending",
					Code: -32004,
				}
			}
			return nil, &backend.Error{Text: err.Error(), Code: -32004}
		}
		if _, hasMethod := message["method"]; hasMethod {
			s.deferred = append(s.deferred, message)
			continue
		}
		if !protocol.IDValuesEqual(message["id"], identifier) {
			s.deferred = append(s.deferred, message)
			continue
		}
		if errorObject, present := message["error"]; present {
			return nil, &backend.Error{Text: fmt.Sprintf("%v", errorObject), Code: -32004}
		}
		return backendResult(message["result"]), nil
	}
}

func backendResult(value any) protocol.Message {
	if result, ok := value.(map[string]any); ok {
		return protocol.Message(result)
	}
	return protocol.Message{}
}

func (s *Server) dispatch(message protocol.Message) protocol.Message {
	identifier := message["id"]
	method, _ := message["method"].(string)
	params := protocol.Message{}
	if raw, present := message["params"]; present && raw != nil {
		typed, ok := raw.(map[string]any)
		if !ok {
			return protocol.RPCError(identifier, -32602, "ACP params must be an object", nil)
		}
		params = protocol.Message(typed)
	}
	var result protocol.Message
	var err error
	switch method {
	case "initialize":
		result, err = s.handleInitialize(params)
	case "session/new":
		result, err = s.handleSessionNew(params)
	case "session/load", "session/resume":
		result, err = s.handleSessionLoad(params)
	case "session/set_mode":
		result, err = s.handleSetMode(params)
	case "session/set_config_option":
		result, err = s.handleSetConfigOption(params)
	case "session/prompt":
		result, err = s.handleSessionPrompt(params)
	case "session/cancel":
		result, err = s.handleSessionCancel(params)
	default:
		return protocol.RPCError(identifier, -32601, "Method not supported: "+method, nil)
	}
	if err != nil {
		backend.Log("%s failed: %v", method, err)
		if typed, ok := err.(*backend.Error); ok {
			return protocol.RPCError(identifier, typed.Code, typed.Error(), typed.Data)
		}
		return protocol.RPCError(identifier, -32602, fmt.Sprintf("Malformed %s request: %v", method, err), nil)
	}
	return protocol.Result(identifier, result)
}

func (s *Server) handleInitialize(params protocol.Message) (protocol.Message, error) {
	version, ok := params["protocolVersion"].(float64)
	if !ok || version != float64(protocol.Version) {
		return nil, &backend.Error{Text: "only ACP protocolVersion 1 is supported", Code: -32600}
	}
	capabilities, err := s.backend.Capabilities()
	if err != nil {
		return nil, err
	}
	return protocol.Message{
		"protocolVersion": protocol.Version,
		"agentInfo": protocol.Message{
			"name": protocol.AgentName, "version": protocol.VersionString,
		},
		"agentCapabilities": capabilities,
		"_meta":             s.backend.Meta(),
	}, nil
}

func (s *Server) handleSessionNew(params protocol.Message) (protocol.Message, error) {
	cwd, _ := params["cwd"].(string)
	servers, err := backend.ParseMCPServers(params["mcpServers"])
	if err != nil {
		return nil, err
	}
	sessionID, err := s.backend.NewSession(cwd, servers, s.emitUpdate)
	if err != nil {
		return nil, err
	}
	s.sessionID = sessionID
	return protocol.Message{"sessionId": sessionID}, nil
}

func (s *Server) handleSessionLoad(params protocol.Message) (protocol.Message, error) {
	sessionID, _ := params["sessionId"].(string)
	if sessionID == "" {
		return nil, &backend.Error{Text: "session/load requires a sessionId", Code: -32602}
	}
	cwd, _ := params["cwd"].(string)
	servers, err := backend.ParseMCPServers(params["mcpServers"])
	if err != nil {
		return nil, err
	}
	sessionID, err = s.backend.LoadSession(sessionID, cwd, servers, s.emitUpdate)
	if err != nil {
		return nil, err
	}
	s.sessionID = sessionID
	return protocol.Message{}, nil
}

func (s *Server) handleSetMode(params protocol.Message) (protocol.Message, error) {
	mode, _ := params["modeId"].(string)
	if mode == "" {
		mode = "build"
	}
	return s.backend.SetSessionMode(mode, s.emitUpdate)
}

func (s *Server) handleSetConfigOption(params protocol.Message) (protocol.Message, error) {
	configID, idOK := params["configId"].(string)
	value, valueOK := params["value"].(string)
	if !idOK || !valueOK || configID == "" {
		return nil, &backend.Error{
			Text: "session/set_config_option requires string configId and value",
			Code: -32602,
		}
	}
	return s.backend.SetConfigOption(configID, value, s.emitUpdate)
}

func (s *Server) handleSessionPrompt(params protocol.Message) (protocol.Message, error) {
	blocks, ok := params["prompt"].([]any)
	if !ok {
		return nil, &backend.Error{Text: "session/prompt requires a ContentBlock array", Code: -32602}
	}
	for _, block := range blocks {
		if _, isObject := block.(map[string]any); !isObject {
			return nil, &backend.Error{Text: "session/prompt requires a ContentBlock array", Code: -32602}
		}
	}
	stopReason, err := s.backend.Prompt(blocks, s.emitUpdate, s.callClient)
	if err != nil {
		return nil, err
	}
	return protocol.Message{"stopReason": stopReason}, nil
}

func (s *Server) handleSessionCancel(params protocol.Message) (protocol.Message, error) {
	s.backend.Cancel()
	return protocol.Message{}, nil
}

// Run serves frames until stdin closes. It returns a process exit code.
func (s *Server) Run() int {
	for {
		message, err := s.read()
		if err != nil {
			return 0
		}
		if _, hasMethod := message["method"]; !hasMethod {
			// A late reply to a server-to-client request; the waiter is gone.
			continue
		}
		if _, hasID := message["id"]; !hasID {
			// Client notification (e.g. session/cancel).
			if message["method"] == "session/cancel" {
				s.backend.Cancel()
			} else {
				backend.Log("ignoring client notification %v", message["method"])
			}
			continue
		}
		if err := s.send(s.dispatch(message)); err != nil {
			backend.Log("cannot send response: %v", err)
			return 1
		}
	}
}
