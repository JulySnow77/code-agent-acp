package bridge

import (
	"strings"

	"claude-acp-bridge/internal/claudecode"
	"claude-acp-bridge/internal/protocol"
)

// handleClaudeEvent is installed as one CLI process's event handler. It routes
// ordinary stream-json events to the active turn's translator and finishes the
// turn on the terminal `result` event.
//
// The CLI delivers events on one ordered pump goroutine, so session/update
// frames keep their original order on the ACP side.
func (s *Server) handleClaudeEvent(session *Session, message protocol.Message) {
	turn := session.currentTurn()
	if turn == nil {
		return
	}

	switch protocol.Str(message, "type") {
	case protocol.ClaudeTypeResult:
		turn.translator.captureResultUsage(message)
		if usage := turn.translator.usageSnapshot(); usage != nil {
			s.emitUpdate(session.ID, protocol.Message{
				"sessionUpdate": protocol.UpdateUsage,
				"used":          usage["used"],
				"size":          usage["size"],
			})
		}

		subtype := protocol.Str(message, "subtype")
		errText := strings.TrimSpace(protocol.Str(message, "result"))
		if errText == "" {
			errText = turn.translator.lastErrorText()
		}
		switch {
		case subtype == protocol.ClaudeResultMaxTurns:
			turn.finish(protocol.StopMaxTokens, errText)
		case turn.isCancelled():
			turn.finish(protocol.StopCancelled, "")
		case subtype == protocol.ClaudeResultExecutionError || protocol.Bool(message, "is_error"):
			if errText == "" {
				errText = "Claude execution failed"
			}
			turn.finish(protocol.StopRefusal, errText)
		default:
			turn.finish(protocol.StopEndTurn, "")
		}
		return
	}

	for _, update := range turn.translator.translate(message) {
		s.emitUpdate(session.ID, update)
	}
}

// handleClaudeControl is installed as the CLI's control-request handler. It
// runs on its own goroutine, so waiting for the ACP client's answer never
// blocks event delivery. Only can_use_tool is translated; other control asks
// are answered conservatively.
func (s *Server) handleClaudeControl(session *Session, requestID string, request protocol.Message) {
	if protocol.Str(request, "subtype") != protocol.ClaudeControlCanUseTool {
		// Unknown control request. Answer with an error-shaped denial rather
		// than hanging the CLI's turn; keeping the raw frame on stderr aids
		// diagnosis when a new CLI ships a new subtype.
		claudecode.Diagnostic("ignoring Claude control request subtype %q", protocol.Str(request, "subtype"))
		_ = session.CLI.RespondControl(requestID, claudeDeny("unsupported control request: "+protocol.Str(request, "subtype")))
		return
	}

	response, rpcErr := s.permissionDecision(session, request)
	if rpcErr != nil {
		claudecode.Diagnostic("permission request failed: %v; falling back to %s",
			rpcErr, s.cfg.PermissionFallback)
		response = fallbackBehavior(s.cfg.PermissionFallback, request)
	}
	if err := session.CLI.RespondControl(requestID, response); err != nil {
		claudecode.Diagnostic("cannot answer Claude control request: %v", err)
	}
}
