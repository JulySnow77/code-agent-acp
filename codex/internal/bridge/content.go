package bridge

import (
	"fmt"

	"codex-acp-bridge/internal/codexapp"
	"codex-acp-bridge/internal/protocol"
)

// blocksToUserInput translates ACP ContentBlock[] into app-server UserInput[].
// The two vocabularies are close but not identical: ACP carries inline base64
// while app-server expects a URL (data: URIs are accepted) or a local path.
func blocksToUserInput(blocks []any) ([]protocol.Message, error) {
	inputs := make([]protocol.Message, 0, len(blocks))
	for index, raw := range blocks {
		block := protocol.AsMessage(raw)
		if block == nil {
			return nil, fmt.Errorf("prompt[%d] is not a ContentBlock object", index)
		}
		switch protocol.Str(block, "type") {
		case "text":
			inputs = append(inputs, protocol.Message{
				"type": "text",
				"text": protocol.Str(block, "text"),
			})

		case "image", "audio":
			blockType := protocol.Str(block, "type")
			url := protocol.Str(block, "uri")
			if url == "" {
				data := protocol.Str(block, "data")
				if data != "" {
					mime := protocol.Str(block, "mimeType")
					if mime == "" {
						mime = "application/octet-stream"
					}
					url = "data:" + mime + ";base64," + data
				}
			}
			if url == "" {
				codexapp.Diagnostic("skipping empty %s block at prompt[%d]", blockType, index)
				continue
			}
			inputs = append(inputs, protocol.Message{"type": blockType, "url": url})

		case "resource_link":
			name := protocol.Str(block, "name")
			if name == "" {
				name = protocol.Str(block, "uri")
			}
			inputs = append(inputs, protocol.Message{
				"type": "text",
				"text": fmt.Sprintf("[resource %s] %s", name, protocol.Str(block, "uri")),
			})

		case "resource":
			resource := protocol.Sub(block, "resource")
			text := protocol.Str(resource, "text")
			if text == "" {
				text = fmt.Sprintf("[embedded resource] %s", protocol.Str(resource, "uri"))
			}
			inputs = append(inputs, protocol.Message{"type": "text", "text": text})

		default:
			codexapp.Diagnostic("skipping unsupported prompt block %q", protocol.Str(block, "type"))
		}
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("prompt contains no usable content blocks")
	}
	return inputs, nil
}
