package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"minimax-acp/acp"
)

// Echo is a small but complete ACP agent used by `minimax-acp serve` and by the
// test-suite as a deterministic protocol fixture. It implements session
// lifecycle, streaming updates, history replay, cancellation and one reverse
// permission call; it never invokes a model or touches real user data.
type Echo struct {
	mu       sync.Mutex
	sessions map[string]*echoSession
	next     atomic.Int64
	info     acp.Implementation
}

type echoSession struct {
	id      string
	cwd     string
	created time.Time
	history []acp.SessionUpdate // replayed by session/load and session/resume
}

func NewEcho() *Echo {
	return &Echo{
		sessions: make(map[string]*echoSession),
		info:     acp.Implementation{Name: "minimax-acp-go-echo", Title: "minimax-acp Go echo agent", Version: "00.00.01"},
	}
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
}

// fixtureState is deliberately labelled as fixture data. It lets
// `chat --show-state` and session/set_mode / session/set_config_option be
// exercised end-to-end without a real model catalog. These ids are NOT
// MiniMax's real modes or models and must never be presented as such.
func fixtureState() acp.SessionStateResult {
	return acp.SessionStateResult{
		Modes: &acp.SessionModeState{
			CurrentModeID: "fixture-ask",
			AvailableModes: []acp.SessionMode{
				{ID: "fixture-ask", Name: "Ask (fixture)", Description: "fixture: ask before each action"},
				{ID: "fixture-full", Name: "Full access (fixture)", Description: "fixture: no confirmation"},
			},
		},
		ConfigOptions: []acp.SessionConfigOption{
			{
				"id": "fixture-model", "name": "Model (fixture)", "type": "select",
				"currentValue": "fixture-echo",
				"options": []any{
					map[string]any{"value": "fixture-echo", "name": "Echo (fixture)"},
					map[string]any{"value": "fixture-alt", "name": "Alt (fixture)"},
				},
			},
		},
	}
}

func (e *Echo) Handlers() HandlerFuncs {
	return HandlerFuncs{
		Initialize: func(_ context.Context, p acp.InitializeParams) (*acp.InitializeResult, *acp.RPCError) {
			if p.ProtocolVersion != acp.ProtocolVersionV1 {
				// Version negotiation: answer with the latest version we support.
			}
			caps := acp.AgentCapabilities{
				LoadSession:        true,
				PromptCapabilities: &acp.PromptCapabilities{Image: true, EmbeddedContext: true},
				MCPCapabilities:    &acp.MCPCapabilities{HTTP: true, SSE: true},
				SessionCapabilities: acp.SessionCapabilities{
					"list":                  json.RawMessage(`{}`),
					"resume":                json.RawMessage(`{}`),
					"close":                 json.RawMessage(`{}`),
					"delete":                json.RawMessage(`{}`),
					"fork":                  json.RawMessage(`{}`),
					"additionalDirectories": json.RawMessage(`{}`),
				},
				Auth: acp.AgentAuthCapabilities{"logout": json.RawMessage(`{}`)},
			}
			return &acp.InitializeResult{
				ProtocolVersion:   acp.ProtocolVersionV1,
				AgentCapabilities: caps,
				AuthMethods:       []acp.AuthMethod{},
				AgentInfo:         &e.info,
			}, nil
		},
		Authenticate: func(_ context.Context, p acp.AuthenticateParams) (*acp.EmptyResult, *acp.RPCError) {
			return nil, acp.InvalidParams("unknown auth method %q", p.MethodID)
		},
		Logout: func(_ context.Context) (*acp.EmptyResult, *acp.RPCError) {
			return &acp.EmptyResult{}, nil
		},
		NewSession: func(_ context.Context, p acp.NewSessionParams) (*acp.NewSessionResult, *acp.RPCError) {
			if p.CWD == "" {
				return nil, acp.InvalidParams("cwd is required")
			}
			s := &echoSession{id: newSessionID(), cwd: p.CWD, created: time.Now()}
			e.mu.Lock()
			e.sessions[s.id] = s
			e.mu.Unlock()
			return &acp.NewSessionResult{SessionID: s.id, SessionStateResult: fixtureState()}, nil
		},
		LoadSession: func(ctx context.Context, p acp.LoadSessionParams, emitter *Emitter) (*acp.SessionStateResult, *acp.RPCError) {
			s, err := e.lookup(p.SessionID)
			if err != nil {
				return nil, err
			}
			if err := e.replay(ctx, s, emitter); err != nil {
				return nil, acp.Errorf(acp.CodeInternalError, "replay: %v", err)
			}
			state := fixtureState()
			return &state, nil
		},
		ResumeSession: func(ctx context.Context, p acp.ResumeSessionParams, emitter *Emitter) (*acp.SessionStateResult, *acp.RPCError) {
			s, err := e.lookup(p.SessionID)
			if err != nil {
				return nil, err
			}
			if err := e.replay(ctx, s, emitter); err != nil {
				return nil, acp.Errorf(acp.CodeInternalError, "replay: %v", err)
			}
			state := fixtureState()
			return &state, nil
		},
		ForkSession: func(_ context.Context, p acp.ForkSessionParams, _ *Emitter) (json.RawMessage, *acp.RPCError) {
			parent, _ := p["sessionId"].(string)
			src, err := e.lookup(parent)
			if err != nil {
				return nil, err
			}
			copySession := &echoSession{id: newSessionID(), cwd: src.cwd, created: time.Now()}
			copySession.history = append([]acp.SessionUpdate(nil), src.history...)
			e.mu.Lock()
			e.sessions[copySession.id] = copySession
			e.mu.Unlock()
			b, _ := json.Marshal(map[string]any{"sessionId": copySession.id})
			return b, nil
		},
		ListSessions: func(_ context.Context, _ acp.ListSessionsParams) (*acp.ListSessionsResult, *acp.RPCError) {
			e.mu.Lock()
			defer e.mu.Unlock()
			out := &acp.ListSessionsResult{Sessions: make([]acp.SessionInfo, 0, len(e.sessions))}
			for _, s := range e.sessions {
				out.Sessions = append(out.Sessions, acp.SessionInfo{
					SessionID: s.id,
					CWD:       s.cwd,
					UpdatedAt: s.created.UTC().Format(time.RFC3339),
				})
			}
			return out, nil
		},
		CloseSession: func(_ context.Context, p acp.CloseSessionParams) (*acp.EmptyResult, *acp.RPCError) {
			if _, err := e.lookup(p.SessionID); err != nil {
				return nil, err
			}
			return &acp.EmptyResult{}, nil
		},
		DeleteSession: func(_ context.Context, p acp.DeleteSessionParams) (*acp.EmptyResult, *acp.RPCError) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if _, ok := e.sessions[p.SessionID]; !ok {
				return nil, acp.Errorf(acp.CodeResourceNotFound, "unknown session %q", p.SessionID)
			}
			delete(e.sessions, p.SessionID)
			return &acp.EmptyResult{}, nil
		},
		SetMode: func(_ context.Context, _ acp.SetSessionModeParams) (*acp.EmptyResult, *acp.RPCError) {
			return &acp.EmptyResult{}, nil
		},
		SetConfigOption: func(_ context.Context, _ acp.SetSessionConfigOptionParams) (*acp.EmptyResult, *acp.RPCError) {
			return &acp.EmptyResult{}, nil
		},
		Prompt: func(ctx context.Context, p acp.PromptParams, emitter *Emitter) (*acp.PromptResult, *acp.RPCError) {
			s, err := e.lookup(p.SessionID)
			if err != nil {
				return nil, err
			}
			text := collectPromptText(p.Prompt)
			e.appendHistory(s, acp.UserMessageChunk(text))
			if err := emitter.SessionUpdate(ctx, s.id, acp.UserMessageChunk(text)); err != nil {
				return nil, acp.Errorf(acp.CodeInternalError, "session/update: %v", err)
			}

			// Demo tool call: lets IDE clients exercise tool_call updates.
			if strings.Contains(strings.ToLower(text), "tool") {
				callID := fmt.Sprintf("call_%d", e.next.Add(1))
				_ = emitter.ToolCall(ctx, s.id, acp.ToolCallReport{
					ToolCallID: callID, Title: "Echo tool", Name: "echo",
					Kind: acp.ToolKindOther, Status: acp.ToolStatusPending,
				})
				status := acp.ToolStatusCompleted
				_ = emitter.ToolCallUpdate(ctx, s.id, acp.ToolCallUpdateReport{
					ToolCallID: callID, Status: &status,
					Content: []acp.ToolCallContent{acp.ToolCallText("echo tool finished")},
				})
			}

			// Demo reverse permission call; a client without the handler simply
			// returns methodNotFound and the echo turn continues.
			if strings.Contains(strings.ToLower(text), "permission") {
				_, _ = emitter.RequestPermission(ctx, acp.RequestPermissionParams{
					SessionID: s.id,
					ToolCall:  acp.ToolCallUpdateReport{ToolCallID: "call_permission", Title: acp.StringPtr("Demo permission")},
					Options: []acp.PermissionOption{
						{OptionID: "allow-once", Name: "Allow once", Kind: acp.PermissionAllowOnce},
						{OptionID: "reject-once", Name: "Reject", Kind: acp.PermissionRejectOnce},
					},
				})
			}

			reply := "echo: " + text
			messageID := fmt.Sprintf("msg_%d", e.next.Add(1))
			for _, chunk := range chunkRunes(reply, 8) {
				select {
				case <-ctx.Done():
					return &acp.PromptResult{StopReason: acp.StopCancelled}, nil
				default:
				}
				u := acp.AgentMessageChunkWithID(messageID, chunk)
				e.appendHistory(s, u)
				if err := emitter.SessionUpdate(ctx, s.id, u); err != nil {
					return nil, acp.Errorf(acp.CodeInternalError, "session/update: %v", err)
				}
			}
			return &acp.PromptResult{StopReason: acp.StopEndTurn}, nil
		},
		Cancel: func(_ context.Context, _ acp.CancelNotificationParams) {
			// Agent.register already cancels the prompt handler's context.
		},
	}
}

func (e *Echo) lookup(id string) (*echoSession, *acp.RPCError) {
	e.mu.Lock()
	s := e.sessions[id]
	e.mu.Unlock()
	if s == nil {
		return nil, acp.Errorf(acp.CodeResourceNotFound, "unknown session %q", id)
	}
	return s, nil
}

func (e *Echo) appendHistory(s *echoSession, u acp.SessionUpdate) {
	e.mu.Lock()
	s.history = append(s.history, u)
	e.mu.Unlock()
}

func (e *Echo) replay(ctx context.Context, s *echoSession, emitter *Emitter) error {
	e.mu.Lock()
	history := append([]acp.SessionUpdate(nil), s.history...)
	e.mu.Unlock()
	for _, u := range history {
		if err := emitter.SessionUpdate(ctx, s.id, u); err != nil {
			return err
		}
	}
	return nil
}

func collectPromptText(blocks []acp.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case acp.ContentTypeText:
			parts = append(parts, b.Text)
		case acp.ContentTypeResourceLink:
			parts = append(parts, "[resource_link "+b.Name+" "+b.URI+"]")
		case acp.ContentTypeImage:
			parts = append(parts, "[image "+b.MIMEType+"]")
		case acp.ContentTypeAudio:
			parts = append(parts, "[audio "+b.MIMEType+"]")
		case acp.ContentTypeEmbedded:
			if b.Resource != nil {
				parts = append(parts, "[resource "+b.Resource.URI+"]")
			}
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, " ")
}

func chunkRunes(s string, size int) []string {
	r := []rune(s)
	if len(r) == 0 {
		return []string{""}
	}
	out := make([]string, 0, (len(r)+size-1)/size)
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}
