package bridge

import (
	"context"
	"os"

	"claude-acp-bridge/internal/claudecode"
	"claude-acp-bridge/internal/protocol"
)

func (s *Server) handleInitialize(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	if version, ok := params["protocolVersion"].(float64); ok && int(version) != protocol.Version {
		claudecode.Diagnostic("client requested protocolVersion %d; serving %d", int(version), protocol.Version)
	}
	return protocol.Message{
		"protocolVersion": protocol.Version,
		"agentInfo": protocol.Message{
			"name":    protocol.AgentName,
			"title":   "Claude Code (via Go ACP adapter)",
			"version": protocol.VersionString,
		},
		"agentCapabilities": protocol.Message{
			"loadSession":        true,
			"promptCapabilities": protocol.Message{"image": true, "embeddedContext": true},
			"mcpCapabilities":    protocol.Message{"http": true, "sse": true},
		},
		"_meta": protocol.Message{
			"adapter":  protocol.AgentName,
			"upstream": "claude stream-json (NDJSON + control protocol)",
		},
	}, nil
}

func (s *Server) openSession(ctx context.Context, id, cwd, resumeID string, params protocol.Message) (*Session, string, *protocol.RPCError) {
	mcpPath, err := mcpConfigFile(protocol.Raw(params, "mcpServers"))
	if err != nil {
		return nil, "", rpcError(protocol.CodeInternalError, "cannot write MCP config: %v", err)
	}

	cli, err := s.claude.Open(ctx, claudecode.Config{
		Bin:            s.cfg.Bin,
		ExtraArgs:      s.cfg.ExtraArgs,
		Dir:            cwd,
		SessionID:      id,
		ResumeID:       resumeID,
		Model:          s.cfg.Model,
		PermissionMode: s.cfg.PermissionMode,
		MCPConfigPath:  mcpPath,
		Log:            s.cfg.Log,
	})
	if err != nil {
		removeTempConfig(mcpPath)
		return nil, "", internalError(claudecode.StartupError(err, ""))
	}

	session := &Session{
		ID:    id,
		CWD:   cwd,
		Mode:  modeForPolicy(s.cfg.PermissionMode),
		Model: s.cfg.Model,
		CLI:   cli,
	}
	cli.SetHandlers(
		func(message protocol.Message) { s.handleClaudeEvent(session, message) },
		func(requestID string, request protocol.Message) { s.handleClaudeControl(session, requestID, request) },
	)
	go func() {
		<-cli.Done()
		claudecode.Diagnostic("Claude process for session %s exited", id)
	}()
	s.addSession(session)
	return session, mcpPath, nil
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
	id, err := newUUID()
	if err != nil {
		return nil, rpcError(protocol.CodeInternalError, "%v", err)
	}

	// Launch with the server's long-lived context: the child must survive this
	// handler returning. A per-call timeout context would kill it via
	// exec.CommandContext as soon as the deferred cancel runs.
	session, mcpPath, rpcErr := s.openSession(s.ctx, id, cwd, "", params)
	if rpcErr != nil {
		return nil, rpcErr
	}

	return protocol.Message{
		"sessionId": id,
		"modes": protocol.Message{
			"currentModeId":  session.Mode,
			"availableModes": availableModes,
		},
		"_meta": protocol.Message{
			"claudeSessionId": id,
			"mcpConfig":       mcpPath != "",
		},
	}, nil
}

func (s *Server) handleSessionLoad(params protocol.Message) (protocol.Message, *protocol.RPCError) {
	id := protocol.Str(params, "sessionId")
	if id == "" {
		return nil, rpcError(protocol.CodeInvalidParams, "session/load requires a sessionId")
	}
	cwd := protocol.Str(params, "cwd")
	if cwd == "" {
		cwd = s.cfg.CWD
	}
	session, _, rpcErr := s.openSession(s.ctx, id, cwd, id, params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	// Claude replays the resumed conversation on stdout after --resume; those
	// events are routed through handleClaudeEvent. There is no separate
	// thread/resume RPC to replay here.
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
	content, err := blocksToContent(blocks)
	if err != nil {
		return nil, rpcError(protocol.CodeInvalidParams, "%v", err)
	}
	if session.currentTurn() != nil {
		return nil, rpcError(protocol.CodeInternalError, "a turn is already running in this session")
	}

	turn := newTurnState()
	session.startTurn(turn)
	defer session.endTurn(turn)

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.TurnTimeout)
	defer cancel()

	if err := session.CLI.SendUserMessage(content); err != nil {
		s.emitUpdate(sessionID, textUpdate(protocol.UpdateAgentMessageChunk, "Claude turn failed: "+err.Error()))
		turn.finish(protocol.StopRefusal, err.Error())
		return nil, internalError(err)
	}

	select {
	case <-turn.done:
	case <-ctx.Done():
		claudecode.Diagnostic("turn timed out or context cancelled; interrupting")
		_ = session.CLI.Interrupt(context.Background())
		turn.finish(protocol.StopCancelled, "")
	case <-s.closed:
		turn.finish(protocol.StopCancelled, "")
	case <-session.CLI.Done():
		turn.finish(protocol.StopRefusal, "Claude process exited during turn")
	}

	reason, errText := turn.outcome()
	if reason == "" {
		reason = protocol.StopCancelled
	}
	if errText != "" {
		s.emitUpdate(sessionID, textUpdate(protocol.UpdateAgentMessageChunk, "Claude error: "+errText))
		return nil, rpcError(protocol.CodeInternalError, "%s", errText)
	}
	return protocol.Message{"stopReason": reason}, nil
}

func (s *Server) cancelTurn(sessionID string) {
	session := s.session(sessionID)
	if session == nil {
		return
	}
	turn := session.markCancelled()
	if turn == nil {
		return
	}
	if err := session.CLI.Interrupt(s.ctx); err != nil {
		claudecode.Diagnostic("interrupt failed: %v", err)
	}
	// ACP cancel is best-effort and must return promptly. Finish the turn here
	// instead of waiting for a CLI `result`, which may be delayed or absent.
	// finish() is once-only, so a later result event cannot double-complete it.
	turn.finish(protocol.StopCancelled, "")
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
	if err := session.CLI.SetMode(s.ctx, sessionClaudeMode(mode)); err != nil {
		return nil, internalError(err)
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
	if err := session.CLI.SetModel(s.ctx, model); err != nil {
		return nil, internalError(err)
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
		if err := session.CLI.SetModel(s.ctx, value); err != nil {
			return nil, internalError(err)
		}
		session.setModel(value)
	case "mode", "permission_mode":
		if !validMode(value) {
			return nil, rpcError(protocol.CodeInvalidParams, "unknown value %q", value)
		}
		if err := session.CLI.SetMode(s.ctx, sessionClaudeMode(value)); err != nil {
			return nil, internalError(err)
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
	case "acceptEdits", "auto":
		return ModeAuto
	case "plan", "dontAsk":
		return ModeUntrusted
	default:
		return ModeAsk
	}
}

func sessionClaudeMode(mode string) string {
	switch mode {
	case ModeAuto:
		return "acceptEdits"
	case ModeUntrusted:
		return "plan"
	default:
		return "manual"
	}
}
