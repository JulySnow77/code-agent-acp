package bridge

import (
	"strings"

	"claude-acp-bridge/internal/protocol"
)

// permissionDecision asks the ACP client via session/request_permission and
// maps the selected option onto the Claude can_use_tool behavior. The CLI
// requires updatedInput on allow and a message on deny.
func (s *Server) permissionDecision(session *Session, request protocol.Message) (protocol.Message, *protocol.RPCError) {
	toolName := protocol.Str(request, "tool_name")
	toolUseID := protocol.Str(request, "tool_use_id")
	input := request["input"]
	options := []protocol.Message{
		{"optionId": protocol.PermissionAllowOnce, "name": "允许一次", "kind": protocol.PermissionAllowOnce},
		{"optionId": protocol.PermissionAllowAlways, "name": "本次会话始终允许", "kind": protocol.PermissionAllowAlways},
		{"optionId": protocol.PermissionRejectOnce, "name": "拒绝", "kind": protocol.PermissionRejectOnce},
		{"optionId": protocol.PermissionRejectAlways, "name": "拒绝并中止", "kind": protocol.PermissionRejectAlways},
	}
	result, callErr := s.callClient(s.ctx, protocol.MethodSessionRequestPermission, protocol.Message{
		"sessionId": session.ID,
		"toolCall":  permissionToolCall(toolUseID, toolName, input, request),
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
	switch selected {
	case protocol.PermissionAllowOnce, protocol.PermissionAllowAlways:
		updated := input
		if updated == nil {
			updated = protocol.Message{}
		}
		return claudeAllow(updated), nil
	case protocol.PermissionRejectOnce, protocol.PermissionRejectAlways:
		return claudeDeny("User denied this action"), nil
	default:
		return nil, rpcError(protocol.CodeInternalError, "permission reply has no optionId")
	}
}

// permissionToolCall builds the ACP tool_call shown in the permission prompt.
func permissionToolCall(toolUseID, toolName string, input any, request protocol.Message) protocol.Message {
	call := protocol.Message{
		"toolCallId": toolUseID,
		"title":      truncate(firstNonEmpty(toolName, "Claude 请求工具权限"), 160),
		"kind":       toolKind(toolName),
		"status":     protocol.ToolStatusPending,
	}
	if input != nil {
		call["rawInput"] = input
	}
	if reason := firstNonEmpty(
		protocol.Str(request, "decision_reason"),
		protocol.Str(request, "description"),
	); reason != "" {
		call["rawInput"] = mergeReason(call["rawInput"], stripANSI(reason))
	}
	return call
}

func mergeReason(rawInput any, reason string) protocol.Message {
	base := protocol.AsMessage(rawInput)
	out := protocol.Message{}
	for key, value := range base {
		out[key] = value
	}
	out["reason"] = reason
	return out
}

// claudeAllow builds the can_use_tool "allow" behavior object.
func claudeAllow(updatedInput any) protocol.Message {
	if updatedInput == nil {
		updatedInput = protocol.Message{}
	}
	return protocol.Message{
		"behavior":     protocol.ClaudeBehaviorAllow,
		"updatedInput": updatedInput,
	}
}

// claudeDeny builds the can_use_tool "deny" behavior object.
func claudeDeny(message string) protocol.Message {
	if strings.TrimSpace(message) == "" {
		message = "User denied this action"
	}
	return protocol.Message{
		"behavior": protocol.ClaudeBehaviorDeny,
		"message":  message,
	}
}

func fallbackBehavior(fallback string, request protocol.Message) protocol.Message {
	if strings.EqualFold(fallback, "accept") {
		input := request["input"]
		if input == nil {
			input = protocol.Message{}
		}
		return claudeAllow(input)
	}
	return claudeDeny("permission denied (client cannot answer session/request_permission)")
}

// stripANSI removes terminal escape sequences from decision_reason. The CLI's
// own schema notes that this field may carry ANSI escapes.
func stripANSI(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] == 0x1b { // ESC
			for i < len(text) && text[i] != 'm' {
				i++
			}
			if i < len(text) {
				i++
			}
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}
