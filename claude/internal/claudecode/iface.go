package claudecode

import (
	"context"

	"claude-acp-bridge/internal/protocol"
)

// Session is the subset of one Claude CLI process the bridge depends on.
// Defined here (rather than in bridge) so fakes can satisfy it without an
// import cycle between bridge and its tests.
type Session interface {
	SetHandlers(onEvent func(protocol.Message), onControl func(requestID string, request protocol.Message))
	SendUserMessage(content []any) error
	RespondControl(requestID string, response protocol.Message) error
	Interrupt(ctx context.Context) error
	SetModel(ctx context.Context, model string) error
	SetMode(ctx context.Context, mode string) error
	InitInfo() protocol.Message
	Done() <-chan struct{}
	Close() error
}

// Factory opens one CLI process per ACP session.
type Factory interface {
	Open(ctx context.Context, cfg Config) (Session, error)
}
