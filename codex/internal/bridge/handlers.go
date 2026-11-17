package bridge

import (
	"context"
	"os"
	"time"

	"codex-acp-bridge/internal/codexapp"
	"codex-acp-bridge/internal/protocol"
)

func (s *Server) handleInitialize(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	// ACP v1: the agent answers with the version it will actually speak.
	if version, ok := params["protocolVersion"].(float64); ok && int(version) != protocol.Version {
		codexapp.Diagnostic("client requested protocolVersion %d; serving %d", int(version), protocol.Version)
	}
	init := s.codex.InitializeResult()
	codexMeta := protocol.Message{}
	for _, key := range []string{"userAgent", "codexHome", "platformFamily", "platformOs"} {
		if value := init[key]; value != nil {
			codexMeta[key] = value
		}
	}
	return protocol.Message{
		"protocolVersion": protocol.Version,
		"agentInfo": protocol.Message{
			"name":    protocol.AgentName,
			"title":   "Codex (via Go ACP adapter)",
			"version": protocol.VersionString,
		},
		"agentCapabilities": protocol.Message{
			"loadSession":        true,
			"promptCapabilities": protocol.Message{"image": true, "embeddedContext": true},
			"mcpCapabilities":    protocol.Message{"http": true, "sse": true},
		},
		"_meta": protocol.Message{
			"adapter":  protocol.AgentName,
			"upstream": "codex app-server (JSON-RPC lite)",
			"codex":    codexMeta,
		},
	}, nil
}

func (s *Server) handleSessionNew(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	cwd := protocol.Str(params, "cwd")
	if cwd == "" {
		cwd = s.cfg.CWD
	}
	if cwd == "" {
		if dir, err := os.Getwd(); err == nil {
			cwd = dir
		}
	}
	threadParams := protocol.Message{
		"cwd":            cwd,
		"approvalPolicy": s.cfg.ApprovalPolicy,
		"sandbox":        s.cfg.Sandbox,
	}
	if s.cfg.Model != "" {
		threadParams["model"] = s.cfg.Model
	}
	if config := mcpConfig(protocol.Raw(params, "mcpServers")); config != nil {
		threadParams["config"] = config
	}

	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	thread, err := s.codex.ThreadStart(ctx, threadParams)
	if err != nil {
		return nil, internalError(err)
	}
	threadID := protocol.Str(thread, "id")
	if threadID == "" {
		return nil, rpcError(protocol.CodeInternalError, "thread/start returned no thread.id")
	}
	session := &Session{
		ID:       threadID,
		ThreadID: threadID,
		CWD:      cwd,
		Mode:     modeForPolicy(s.cfg.ApprovalPolicy),
		Model:    s.cfg.Model,
	}
	s.addSession(session)

	return protocol.Message{
		"sessionId": threadID,
		"modes": protocol.Message{
			"currentModeId":  session.Mode,
			"availableModes": availableModes,
		},
		"_meta": protocol.Message{"codexThreadId": threadID},
	}, nil
}

func (s *Server) handleSessionLoad(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	threadID := protocol.Str(params, "sessionId")
	if threadID == "" {
		return nil, rpcError(protocol.CodeInvalidParams, "session/load requires a sessionId")
	}
	resumeParams := protocol.Message{}
	if cwd := protocol.Str(params, "cwd"); cwd != "" {
		resumeParams["cwd"] = cwd
	}
	if config := mcpConfig(protocol.Raw(params, "mcpServers")); config != nil {
		resumeParams["config"] = config
	}
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	thread, err := s.codex.ThreadResume(ctx, threadID, resumeParams)
	if err != nil {
		return nil, internalError(err)
	}
	session := &Session{
		ID:       threadID,
		ThreadID: threadID,
		CWD:      protocol.Str(params, "cwd"),
		Mode:     modeForPolicy(s.cfg.ApprovalPolicy),
		Model:    s.cfg.Model,
	}
	s.addSession(session)

	// ACP loadSession expects the transcript to be replayed as session/update
	// notifications before the response is returned.
	s.replayThread(session, thread)

	return protocol.Message{
		"modes": protocol.Message{
			"currentModeId":  session.Mode,
			"availableModes": availableModes,
		},
	}, nil
}

func (s *Server) handleSessionPrompt(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	sessionID := protocol.Str(params, "sessionId")
	session := s.session(sessionID)
	if session == nil {
		return nil, rpcError(protocol.CodeInvalidParams, "unknown sessionId %q", sessionID)
	}
	blocks, ok := params["prompt"].([]any)
	if !ok {
		return nil, rpcError(protocol.CodeInvalidParams, "session/prompt requires a ContentBlock array")
	}
	input, err := blocksToUserInput(blocks)
	if err != nil {
		return nil, rpcError(protocol.CodeInvalidParams, "%v", err)
	}
	if session.currentTurn() != nil {
		return nil, rpcError(protocol.CodeInternalError, "a turn is already running in this session")
	}

	turn := newTurnState()
	session.startTurn(turn)
	defer session.endTurn(turn)

	mode, model := session.snapshot()
	turnParams := protocol.Message{
		"threadId":       sessionID,
		"input":          input,
		"approvalPolicy": policyForMode(mode),
	}
	if model != "" {
		turnParams["model"] = model
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.TurnTimeout)
	defer cancel()
	turnObject, err := s.codex.TurnStart(ctx, turnParams)
	if err != nil {
		s.emitUpdate(sessionID, textUpdate(protocol.UpdateAgentMessageChunk, "Codex turn failed: "+err.Error()))
		turn.finish(protocol.StopRefusal, err.Error())
		return nil, internalError(err)
	}
	if interruptNow := turn.setID(protocol.Str(turnObject, "id")); interruptNow {
		_ = s.codex.TurnInterrupt(s.ctx, sessionID, turn.turnID())
	}

	select {
	case <-turn.done:
		// Normal completion (or an app-server error notification).
	case <-ctx.Done():
		codexapp.Diagnostic("turn timed out or context cancelled; interrupting")
		if id := turn.turnID(); id != "" {
			_ = s.codex.TurnInterrupt(context.Background(), sessionID, id)
		}
		turn.finish(protocol.StopCancelled, "")
	case <-s.closed:
		turn.finish(protocol.StopCancelled, "")
	}

	reason, errText := turn.outcome()
	if reason == "" {
		reason = protocol.StopCancelled
	}
	if errText != "" {
		// Error visibility (same policy as zcode-acp-bridge): surface the text
		// in the transcript AND fail the JSON-RPC call instead of pretending
		// the turn ended normally.
		s.emitUpdate(sessionID, textUpdate(protocol.UpdateAgentMessageChunk, "Codex error: "+errText))
		return nil, rpcError(protocol.CodeInternalError, "%s", errText)
	}
	return protocol.Message{"stopReason": reason}, nil
}

func (s *Server) handleSetMode(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	session := s.session(protocol.Str(params, "sessionId"))
	if session == nil {
		return nil, rpcError(protocol.CodeInvalidParams, "unknown sessionId")
	}
	mode := protocol.Str(params, "modeId")
	if !validMode(mode) {
		return nil, rpcError(protocol.CodeInvalidParams, "unknown modeId %q", mode)
	}
	session.setMode(mode)
	return protocol.Message{}, nil
}

func (s *Server) handleSetModel(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	session := s.session(protocol.Str(params, "sessionId"))
	if session == nil {
		return nil, rpcError(protocol.CodeInvalidParams, "unknown sessionId")
	}
	model := protocol.Str(params, "modelId")
	if model == "" {
		return nil, rpcError(protocol.CodeInvalidParams, "session/set_model requires a modelId")
	}
	session.setModel(model)
	return protocol.Message{}, nil
}

func (s *Server) handleSetConfigOption(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	session := s.session(protocol.Str(params, "sessionId"))
	if session == nil {
		return nil, rpcError(protocol.CodeInvalidParams, "unknown sessionId")
	}
	configID, value := protocol.Str(params, "configId"), protocol.Str(params, "value")
	switch configID {
	case "model":
		session.setModel(value)
	case "approval_policy", "mode":
		if !validMode(value) {
			return nil, rpcError(protocol.CodeInvalidParams, "unknown value %q", value)
		}
		session.setMode(value)
	default:
		return nil, rpcError(protocol.CodeInvalidParams, "unsupported configId %q", configID)
	}
	return protocol.Message{}, nil
}

func validMode(mode string) bool {
	for _, candidate := range availableModes {
		if protocol.Str(candidate, "id") == mode {
			return true
		}
	}
	return false
}

func modeForPolicy(policy string) string {
	switch policy {
	case "never":
		return ModeAuto
	case "untrusted":
		return ModeUntrusted
	default:
		return ModeAsk
	}
}

func policyForMode(mode string) string {
	switch mode {
	case ModeAuto:
		return "never"
	case ModeUntrusted:
		return "untrusted"
	default:
		return "on-request"
	}
}

// mcpConfig translates ACP mcpServers into a Codex `config` override object.
// Codex expects TOML-equivalent keys: `mcp_servers.<name>.command/args/env`
// for stdio servers and `mcp_servers.<name>.url` for HTTP/SSE servers.
func mcpConfig(servers []any) protocol.Message {
	if len(servers) == 0 {
		return nil
	}
	entries := protocol.Message{}
	for _, raw := range servers {
		item := protocol.AsMessage(raw)
		if item == nil {
			continue
		}
		name := protocol.Str(item, "name")
		if name == "" {
			continue
		}
		entry := protocol.Message{}
		switch protocol.Str(item, "type") {
		case "http", "sse":
			entry["url"] = protocol.Str(item, "url")
			headers := protocol.Message{}
			for _, rawHeader := range protocol.Raw(item, "headers") {
				header := protocol.AsMessage(rawHeader)
				if header == nil {
					continue
				}
				if key := protocol.Str(header, "name"); key != "" {
					headers[key] = protocol.Str(header, "value")
				}
			}
			if len(headers) > 0 {
				entry["http_headers"] = headers
			}
		default:
			entry["command"] = protocol.Str(item, "command")
			entry["args"] = protocol.Raw(item, "args")
			env := protocol.Message{}
			for _, rawEnv := range protocol.Raw(item, "env") {
				variable := protocol.AsMessage(rawEnv)
				if variable == nil {
					continue
				}
				if key := protocol.Str(variable, "name"); key != "" {
					env[key] = protocol.Str(variable, "value")
				}
			}
			entry["env"] = env
		}
		entries[name] = entry
	}
	if len(entries) == 0 {
		return nil
	}
	return protocol.Message{"mcp_servers": entries}
}

func textUpdate(kind, text string) protocol.Message {
	return protocol.Message{
		"sessionUpdate": kind,
		"content":       protocol.Message{"type": "text", "text": text},
	}
}
