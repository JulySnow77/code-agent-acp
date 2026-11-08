package acp

import (
	"encoding/json"
	"fmt"
)

// Content block discriminators (schema/v1 ContentBlock).
const (
	ContentTypeText         = "text"
	ContentTypeImage        = "image"
	ContentTypeAudio        = "audio"
	ContentTypeResourceLink = "resource_link"
	ContentTypeEmbedded     = "resource"
)

// ResourceContents is the text or blob payload of an embedded resource.
type ResourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

func (r ResourceContents) MarshalJSON() ([]byte, error) {
	m := map[string]any{"uri": r.URI}
	if r.MIMEType != "" {
		m["mimeType"] = r.MIMEType
	}
	if r.Blob != "" {
		m["blob"] = r.Blob
	} else {
		m["text"] = r.Text // text is required for text resources, even when empty
	}
	return json.Marshal(m)
}

// ContentBlock is the ACP v1 union used in session/prompt and message chunks.
// Fields not relevant to its Type are ignored when marshalling, so a block
// never carries a contradictory shape. Raw preserves unknown vendor variants.
type ContentBlock struct {
	Type        string
	Text        string
	Data        string // base64 for image / audio
	MIMEType    string
	URI         string
	Name        string
	Description string
	Size        *int64
	Title       string
	Resource    *ResourceContents
	Annotations map[string]any
	Meta        map[string]any
	Raw         json.RawMessage
}

func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

func ImageBlock(data, mimeType string) ContentBlock {
	return ContentBlock{Type: ContentTypeImage, Data: data, MIMEType: mimeType}
}

func AudioBlock(data, mimeType string) ContentBlock {
	return ContentBlock{Type: ContentTypeAudio, Data: data, MIMEType: mimeType}
}

func ResourceLinkBlock(name, uri string) ContentBlock {
	return ContentBlock{Type: ContentTypeResourceLink, Name: name, URI: uri}
}

func EmbeddedResourceBlock(text, uri, mimeType string) ContentBlock {
	return ContentBlock{
		Type:     ContentTypeEmbedded,
		Resource: &ResourceContents{URI: uri, Text: text, MIMEType: mimeType},
	}
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if b.Type == "" {
		if len(b.Raw) > 0 {
			return b.Raw, nil
		}
		return nil, fmt.Errorf("acp: ContentBlock has no type")
	}
	m := map[string]any{"type": b.Type}
	if b.Annotations != nil {
		m["annotations"] = b.Annotations
	}
	if b.Meta != nil {
		m["_meta"] = b.Meta
	}
	switch b.Type {
	case ContentTypeText:
		m["text"] = b.Text
	case ContentTypeImage, ContentTypeAudio:
		m["data"], m["mimeType"] = b.Data, b.MIMEType
		if b.Type == ContentTypeImage && b.URI != "" {
			m["uri"] = b.URI
		}
	case ContentTypeResourceLink:
		m["name"], m["uri"] = b.Name, b.URI
		if b.Description != "" {
			m["description"] = b.Description
		}
		if b.MIMEType != "" {
			m["mimeType"] = b.MIMEType
		}
		if b.Title != "" {
			m["title"] = b.Title
		}
		if b.Size != nil {
			m["size"] = *b.Size
		}
	case ContentTypeEmbedded:
		if b.Resource == nil {
			return nil, fmt.Errorf("acp: embedded resource block has no resource payload")
		}
		m["resource"] = b.Resource
	default:
		if len(b.Raw) > 0 {
			return b.Raw, nil
		}
	}
	return json.Marshal(m)
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var aux struct {
		Type        string            `json:"type"`
		Text        string            `json:"text"`
		Data        string            `json:"data"`
		MIMEType    string            `json:"mimeType"`
		URI         string            `json:"uri"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Size        *int64            `json:"size"`
		Title       string            `json:"title"`
		Resource    *ResourceContents `json:"resource"`
		Annotations map[string]any    `json:"annotations"`
		Meta        map[string]any    `json:"_meta"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*b = ContentBlock{
		Type: aux.Type, Text: aux.Text, Data: aux.Data, MIMEType: aux.MIMEType,
		URI: aux.URI, Name: aux.Name, Description: aux.Description, Size: aux.Size,
		Title: aux.Title, Resource: aux.Resource, Annotations: aux.Annotations,
		Meta: aux.Meta, Raw: append([]byte(nil), data...),
	}
	return nil
}

// SessionUpdate is the open union carried by session/update notifications.
// A map keeps every vendor field (usage/config updates and future variants)
// helpers below give typed access to the common variants.
type SessionUpdate map[string]any

func (u SessionUpdate) Type() string { return u.Str("sessionUpdate") }

func (u SessionUpdate) Str(key string) string {
	if v, ok := u[key].(string); ok {
		return v
	}
	return ""
}

func (u SessionUpdate) Text() string {
	if c, ok := u.Content(); ok && c.Type == ContentTypeText {
		return c.Text
	}
	return ""
}

func (u SessionUpdate) MessageID() string  { return u.Str("messageId") }
func (u SessionUpdate) ToolCallID() string { return u.Str("toolCallId") }
func (u SessionUpdate) ToolStatus() string { return u.Str("status") }
func (u SessionUpdate) ToolTitle() string  { return u.Str("title") }
func (u SessionUpdate) ToolKind() string   { return u.Str("kind") }
func (u SessionUpdate) Title() string      { return u.Str("title") }

// Content decodes the `content` field of a message chunk update.
func (u SessionUpdate) Content() (ContentBlock, bool) {
	v, ok := u["content"]
	if !ok || v == nil {
		return ContentBlock{}, false
	}
	switch x := v.(type) {
	case ContentBlock:
		return x, true
	case *ContentBlock:
		if x != nil {
			return *x, true
		}
	case map[string]any:
		return decodeContentBytes(mustJSON(x))
	case json.RawMessage:
		return decodeContentBytes(x)
	case []byte:
		return decodeContentBytes(x)
	}
	return ContentBlock{}, false
}

func decodeContentBytes(data []byte) (ContentBlock, bool) {
	var b ContentBlock
	if err := json.Unmarshal(data, &b); err != nil {
		return ContentBlock{}, false
	}
	return b, true
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func toMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func messageChunk(variant, messageID string, block ContentBlock) SessionUpdate {
	u := SessionUpdate{"sessionUpdate": variant, "content": block}
	if messageID != "" {
		u["messageId"] = messageID
	}
	return u
}

func AgentMessageChunk(text string) SessionUpdate {
	return messageChunk(UpdateAgentMessageChunk, "", TextBlock(text))
}

func AgentMessageChunkWithID(messageID, text string) SessionUpdate {
	return messageChunk(UpdateAgentMessageChunk, messageID, TextBlock(text))
}

func UserMessageChunk(text string) SessionUpdate {
	return messageChunk(UpdateUserMessageChunk, "", TextBlock(text))
}

func AgentThoughtChunk(text string) SessionUpdate {
	return messageChunk(UpdateAgentThoughtChunk, "", TextBlock(text))
}

func PlanUpdate(entries []PlanEntry) SessionUpdate {
	return SessionUpdate{"sessionUpdate": UpdatePlan, "entries": entries}
}

func AvailableCommandsUpdate(commands []AvailableCommand) SessionUpdate {
	return SessionUpdate{"sessionUpdate": UpdateAvailableCommands, "availableCommands": commands}
}

func CurrentModeUpdate(modeID string) SessionUpdate {
	return SessionUpdate{"sessionUpdate": UpdateCurrentMode, "currentModeId": modeID}
}

func UsageUpdate(used, size int64) SessionUpdate {
	return SessionUpdate{"sessionUpdate": UpdateUsage, "used": used, "size": size}
}

func (r ToolCallReport) SessionUpdate() SessionUpdate {
	m := toMap(r)
	m["sessionUpdate"] = UpdateToolCall
	return SessionUpdate(m)
}

func (r ToolCallUpdateReport) SessionUpdate() SessionUpdate {
	m := toMap(r)
	m["sessionUpdate"] = UpdateToolCallUpdate
	return SessionUpdate(m)
}
