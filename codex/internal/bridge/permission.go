package bridge

import (
	"strings"

	"codex-acp-bridge/internal/codexapp"
	"codex-acp-bridge/internal/protocol"
)

// app-server methods that require a human decision. They arrive as
// server-initiated JSON-RPC requests (with an id) and must be answered with a
// {result: ...} frame on the same connection.
const (
	MethodCommandApproval     = "item/commandExecution/requestApproval"
	MethodFileChangeApproval  = "item/fileChange/requestApproval"
	MethodPermissionsApproval = "item/permissions/requestApproval"
	MethodLegacyExecApproval  = "execCommandApproval"
	MethodLegacyPatchApproval = "applyPatchApproval"
)

// handleCodexRequest is installed as the app-server reverse-request handler.
// It runs on its own goroutine, so waiting for the ACP client's answer never
// blocks app-server notification delivery.
func (s *Server) handleCodexRequest(id any, method string, params protocol.Message) {
	response, rpcErr := s.approvalDecision(method, params)
	if rpcErr != nil {
		codexapp.Diagnostic("approval request %s failed: %v; falling back to %s",
			method, rpcErr, s.cfg.PermissionFallback)
		response = protocol.Message{
			"decision": fallbackDecision(method, s.cfg.PermissionFallback),
		}
	}
	if err := s.codex.Respond(id, response, nil); err != nil {
		codexapp.Diagnostic("cannot answer app-server request %s: %v", method, err)
	}
}

// approvalDecision asks the ACP client via session/request_permission and maps
// the selected option onto the app-server decision vocabulary.
func (s *Server) approvalDecision(method string, params protocol.Message) (protocol.Message, *protocol.RPCError) {
	if method == MethodPermissionsApproval {
		// Permission-profile requests need a structured grant, not a simple
		// accept/decline. Decline explicitly rather than inventing a grant.
		return nil, rpcError(protocol.CodeMethodNotFound,
			"app-server permission-profile requests are not translated by this adapter")
	}

	sessionID := s.sessionForApproval(params)
	if sessionID == "" {
		return nil, rpcError(protocol.CodeInternalError, "no ACP session for approval request")
	}
	toolCall := permissionToolCall(method, params)
	options := []protocol.Message{
		{"optionId": protocol.PermissionAllowOnce, "name": "允许一次", "kind": protocol.PermissionAllowOnce},
		{"optionId": protocol.PermissionAllowAlways, "name": "本次会话始终允许", "kind": protocol.PermissionAllowAlways},
		{"optionId": protocol.PermissionRejectOnce, "name": "拒绝", "kind": protocol.PermissionRejectOnce},
		{"optionId": protocol.PermissionRejectAlways, "name": "拒绝并中止", "kind": protocol.PermissionRejectAlways},
	}
	result, callErr := s.callClient(s.ctx, protocol.MethodSessionRequestPermission, protocol.Message{
		"sessionId": sessionID,
		"toolCall":  toolCall,
		"options":   options,
	})
	if callErr != nil {
		return nil, callErr
	}
	outcome := protocol.Sub(result, "outcome")
	selected := protocol.Str(outcome, "optionId")
	if protocol.Str(outcome, "outcome") == protocol.OutcomeCancelled {
		selected = protocol.PermissionRejectAlways
	}
	if selected == "" {
		return nil, rpcError(protocol.CodeInternalError, "permission reply has no optionId")
	}
	return protocol.Message{"decision": decisionFor(method, selected)}, nil
}

func (s *Server) sessionForApproval(params protocol.Message) string {
	if id := protocol.Str(params, "threadId"); id != "" {
		if s.session(id) != nil {
			return id
		}
	}
	// Some legacy requests omit threadId; fall back to the only live session.
	s.sessMu.RLock()
	defer s.sessMu.RUnlock()
	if len(s.sessions) == 1 {
		for id := range s.sessions {
			return id
		}
	}
	return ""
}

func permissionToolCall(method string, params protocol.Message) protocol.Message {
	itemID := protocol.Str(params, "itemId")
	call := protocol.Message{
		"toolCallId": itemID,
		"status":     protocol.ToolStatusPending,
	}
	switch method {
	case MethodFileChangeApproval:
		call["title"] = "Codex 请求修改文件"
		call["kind"] = protocol.ToolKindEdit
		call["rawInput"] = protocol.Message{
			"grantRoot": protocol.StringValue(params["grantRoot"]),
			"reason":    protocol.Str(params, "reason"),
		}
	default:
		command := protocol.Str(params, "command")
		if command == "" {
			command = protocol.StringValue(params["commandActions"])
		}
		call["title"] = truncate(firstNonEmpty(command, "Codex 请求执行命令"), 160)
		call["kind"] = protocol.ToolKindExecute
		call["rawInput"] = protocol.Message{
			"command": command,
			"cwd":     protocol.StringValue(params["cwd"]),
			"reason":  protocol.Str(params, "reason"),
		}
	}
	return call
}

// decisionFor maps ACP option ids to the decision vocabulary of the specific
// app-server request. Modern v2 requests use accept/acceptForSession/decline/
// cancel; the legacy requests use approved/approved_for_session/denied/abort.
func decisionFor(method, optionID string) string {
	legacy := method == MethodLegacyExecApproval || method == MethodLegacyPatchApproval
	switch optionID {
	case protocol.PermissionAllowOnce:
		if legacy {
			return "approved"
		}
		return "accept"
	case protocol.PermissionAllowAlways:
		if legacy {
			return "approved_for_session"
		}
		return "acceptForSession"
	case protocol.PermissionRejectAlways:
		if legacy {
			return "abort"
		}
		return "cancel"
	default:
		if legacy {
			return "denied"
		}
		return "decline"
	}
}

func fallbackDecision(method, fallback string) string {
	if strings.EqualFold(fallback, "accept") {
		return decisionFor(method, protocol.PermissionAllowOnce)
	}
	return decisionFor(method, protocol.PermissionRejectOnce)
}
