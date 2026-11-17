package bridge

import "codex-acp-bridge/internal/protocol"

// replayThread rebuilds the ACP transcript for session/load from the thread
// history returned by thread/resume. ACP hosts expect these notifications
// before the load response, so callers must invoke this synchronously.
func (s *Server) replayThread(session *Session, thread protocol.Message) {
	translator := newTranslator()
	for _, rawTurn := range protocol.AsSlice(thread["turns"]) {
		turn := protocol.AsMessage(rawTurn)
		if turn == nil {
			continue
		}
		for _, rawItem := range protocol.AsSlice(turn["items"]) {
			item := protocol.AsMessage(rawItem)
			if item == nil {
				continue
			}
			for _, update := range replayItem(translator, item) {
				s.emitUpdate(session.ID, update)
			}
		}
	}
}

func replayItem(translator *Translator, item protocol.Message) []protocol.Message {
	switch protocol.Str(item, "type") {
	case "userMessage":
		return replayContent(protocol.UpdateUserMessageChunk, item["content"])
	case "agentMessage":
		if text := protocol.Str(item, "text"); text != "" {
			return []protocol.Message{textUpdate(protocol.UpdateAgentMessageChunk, text)}
		}
		return replayContent(protocol.UpdateAgentMessageChunk, item["content"])
	case "reasoning":
		return replayContent(protocol.UpdateAgentThoughtChunk, item["content"])
	case "plan":
		update := translator.planUpdate(protocol.Str(item, "text"))
		if update == nil {
			return nil
		}
		return []protocol.Message{update}
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
		start := translator.startItem(item)
		done := translator.completeItem(item)
		return append(start, done...)
	}
	return nil
}

func replayContent(kind string, value any) []protocol.Message {
	updates := make([]protocol.Message, 0, 4)
	for _, raw := range protocol.AsSlice(value) {
		block := protocol.AsMessage(raw)
		if block == nil || protocol.Str(block, "type") != "text" {
			continue
		}
		text := protocol.Str(block, "text")
		if text == "" {
			continue
		}
		updates = append(updates, textUpdate(kind, text))
	}
	return updates
}
