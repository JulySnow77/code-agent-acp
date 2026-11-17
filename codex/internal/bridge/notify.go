package bridge

import (
	"strings"

	"codex-acp-bridge/internal/codexapp"
	"codex-acp-bridge/internal/protocol"
)

// handleCodexNotification is installed as the app-server notification handler.
// The app-server client delivers these on one ordered pump goroutine, so
// session/update frames keep their original order on the ACP side.
func (s *Server) handleCodexNotification(method string, params protocol.Message) {
	threadID := protocol.Str(params, "threadId")
	session := s.session(threadID)
	if session == nil {
		return
	}
	turn := session.currentTurn()
	if turn == nil {
		return
	}
	// A thread may have several turns over its lifetime. Once the turn id is
	// known, ignore late notifications from earlier turns.
	if notificationTurnID := protocol.Str(params, "turnId"); notificationTurnID != "" {
		if id := turn.turnID(); id != "" && id != notificationTurnID {
			return
		}
	}

	switch method {
	case "turn/started":
		if id := protocol.Str(protocol.Sub(params, "turn"), "id"); id != "" {
			if interruptNow := turn.setID(id); interruptNow {
				_ = s.codex.TurnInterrupt(s.ctx, threadID, id)
			}
		}
		return

	case "turn/completed":
		turnObject := protocol.Sub(params, "turn")
		if id := protocol.Str(turnObject, "id"); id != "" {
			turn.setID(id)
		}
		status := protocol.Str(turnObject, "status")
		errText := turnErrorMessage(turnObject)
		if errText == "" {
			errText = turn.translator.lastErrorText()
		}
		if errText == "" && status == "failed" {
			errText = "Codex turn failed"
		}
		switch {
		case errText != "":
			turn.finish(protocol.StopRefusal, errText)
		case status == "interrupted" || turn.isCancelled():
			turn.finish(protocol.StopCancelled, "")
		default:
			turn.finish(protocol.StopEndTurn, "")
		}
		return
	}

	updates := turn.translator.translate(method, params)
	for _, update := range updates {
		s.emitUpdate(threadID, update)
	}
	if len(updates) == 0 && method == "error" && !protocol.Bool(params, "willRetry") {
		// The error notification is already visible in the transcript; do not
		// end the turn here — app-server still owes us turn/completed.
		codexapp.Diagnostic("app-server reported a non-retryable error for thread %s", threadID)
	}
}

func turnErrorMessage(turn protocol.Message) string {
	errObject := protocol.Sub(turn, "error")
	if errObject == nil {
		return ""
	}
	text := protocol.Str(errObject, "message")
	if text == "" {
		text = protocol.StringValue(errObject["codexErrorInfo"])
	}
	return strings.TrimSpace(text)
}
