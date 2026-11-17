package bridge

import (
	"fmt"

	"claude-acp-bridge/internal/claudecode"
	"claude-acp-bridge/internal/protocol"
)

// blocksToContent translates ACP ContentBlock[] into an Anthropic content
// block array for a Claude stream-json user message. The vocabularies are close
// but not identical: ACP uses uri/data/mimeType for images, while the CLI wants
// {type:"image",source:{type:"base64"|"url",...}}.
func blocksToContent(blocks []any) ([]any, error) {
	content := make([]any, 0, len(blocks))
	for index, raw := range blocks {
		block := protocol.AsMessage(raw)
		if block == nil {
			return nil, fmt.Errorf("prompt[%d] is not a ContentBlock object", index)
		}
		switch protocol.Str(block, "type") {
		case "text":
			content = append(content, protocol.Message{
				"type": "text",
				"text": protocol.Str(block, "text"),
			})

		case "image":
			source, ok := imageSource(block)
			if !ok {
				claudecode.Diagnostic("skipping empty image block at prompt[%d]", index)
				continue
			}
			content = append(content, protocol.Message{"type": "image", "source": source})

		case "resource_link":
			name := protocol.Str(block, "name")
			if name == "" {
				name = protocol.Str(block, "uri")
			}
			content = append(content, protocol.Message{
				"type": "text",
				"text": fmt.Sprintf("[resource %s] %s", name, protocol.Str(block, "uri")),
			})

		case "resource":
			resource := protocol.Sub(block, "resource")
			text := protocol.Str(resource, "text")
			if text == "" {
				text = fmt.Sprintf("[embedded resource] %s", protocol.Str(resource, "uri"))
			}
			content = append(content, protocol.Message{"type": "text", "text": text})

		default:
			claudecode.Diagnostic("skipping unsupported prompt block %q", protocol.Str(block, "type"))
		}
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("prompt contains no usable content blocks")
	}
	return content, nil
}

// imageSource maps an ACP image block to an Anthropic image source. Inline
// base64 is preferred; a plain uri becomes a URL source.
func imageSource(block protocol.Message) (protocol.Message, bool) {
	if data := protocol.Str(block, "data"); data != "" {
		mediaType := protocol.Str(block, "mimeType")
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return protocol.Message{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		}, true
	}
	if uri := protocol.Str(block, "uri"); uri != "" {
		return protocol.Message{"type": "url", "url": uri}, true
	}
	return nil, false
}

// mcpConfigFile writes ACP mcpServers to a temporary JSON config the CLI can
// consume via --mcp-config. Returns an empty path when there are no servers.
// The caller owns the returned path and should remove it on session close.
func mcpConfigFile(servers []any) (string, error) {
	if len(servers) == 0 {
		return "", nil
	}
	entries := protocol.Message{}
	for _, raw := range servers {
		item := protocol.AsMessage(raw)
		if item == nil || protocol.Str(item, "name") == "" {
			continue
		}
		name := protocol.Str(item, "name")
		entry := protocol.Message{}
		switch protocol.Str(item, "type") {
		case "http", "sse", "streamable-http":
			entry["type"] = protocol.Str(item, "type")
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
				entry["headers"] = headers
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
			if len(env) > 0 {
				entry["env"] = env
			}
		}
		entries[name] = entry
	}
	if len(entries) == 0 {
		return "", nil
	}
	body, err := protocol.Encode(protocol.Message{"mcpServers": entries})
	if err != nil {
		return "", err
	}
	path, err := writeTempConfig(body)
	if err != nil {
		return "", err
	}
	return path, nil
}
