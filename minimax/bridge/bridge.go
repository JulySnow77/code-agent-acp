// Package bridge implements a transparent, bidirectional ACP stdio proxy.
// This is the Go equivalent of putting a Go process between an ACP editor and
// `mcode acp`: every request, response and notification is forwarded unchanged.
// It is useful for tracing, capability rewriting, or exposing an existing ACP
// agent under a different launch command.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sync"

	"minimax-acp/acp"
)

type Options struct {
	Logger *log.Logger
	// DownstreamObserver / UpstreamObserver see frames on each side. They must
	// never write to the ACP stdout stream used by the downstream transport.
	DownstreamObserver func(acp.FrameDirection, []byte)
	UpstreamObserver   func(acp.FrameDirection, []byte)
}

type Bridge struct {
	down *acp.Conn
	up   *acp.Conn
	opts Options

	mu       sync.Mutex
	downToUp map[string]acp.RequestID
	upToDown map[string]acp.RequestID
}

// New wires two Conns. downstream is normally os.Stdin/os.Stdout (the editor),
// upstream is the stdio of the spawned `mcode acp` process.
func New(downstream, upstream acp.Transport, opts Options) *Bridge {
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	b := &Bridge{
		down:     acp.NewConn(downstream, acp.ConnOptions{Logger: logger, Observer: opts.DownstreamObserver}),
		up:       acp.NewConn(upstream, acp.ConnOptions{Logger: logger, Observer: opts.UpstreamObserver}),
		opts:     opts,
		downToUp: make(map[string]acp.RequestID),
		upToDown: make(map[string]acp.RequestID),
	}
	b.wire()
	return b
}

func (b *Bridge) wire() {
	// Client/editor -> agent.
	b.down.OnDefaultRequestWithID(func(ctx context.Context, id acp.RequestID, method string, params json.RawMessage) (any, *acp.RPCError) {
		return b.forward(ctx, id, b.up, method, params, true)
	})
	b.down.OnDefaultNotification(func(_ context.Context, method string, params json.RawMessage) {
		if method == acp.MethodCancelRequest {
			params = b.rewriteCancel(params, true)
		}
		if err := b.up.NotifyRaw(method, params); err != nil {
			b.logf("bridge: downstream notification %s: %v", method, err)
		}
	})
	// $/cancel_request needs an exact handler because Conn intercepts it.
	b.down.OnNotification(acp.MethodCancelRequest, func(_ context.Context, raw json.RawMessage) {
		_ = b.up.NotifyRaw(acp.MethodCancelRequest, b.rewriteCancel(raw, true))
	})

	// Agent -> client/editor (session/update, session/request_permission, fs/*,
	// terminal/* and any vendor extension).
	b.up.OnDefaultRequestWithID(func(ctx context.Context, id acp.RequestID, method string, params json.RawMessage) (any, *acp.RPCError) {
		return b.forward(ctx, id, b.down, method, params, false)
	})
	b.up.OnDefaultNotification(func(_ context.Context, method string, params json.RawMessage) {
		if err := b.down.NotifyRaw(method, params); err != nil {
			b.logf("bridge: upstream notification %s: %v", method, err)
		}
	})
	b.up.OnNotification(acp.MethodCancelRequest, func(_ context.Context, raw json.RawMessage) {
		_ = b.down.NotifyRaw(acp.MethodCancelRequest, b.rewriteCancel(raw, false))
	})
}

// forward maps ids both ways so $/cancel_request can be translated. Params and
// results stay raw: unknown fields from either implementation survive the hop.
func (b *Bridge) forward(ctx context.Context, inID acp.RequestID, to *acp.Conn, method string, params json.RawMessage, downstream bool) (any, *acp.RPCError) {
	outID := to.AllocID()
	b.mu.Lock()
	if downstream {
		b.downToUp[inID.Key()] = outID
		b.upToDown[outID.Key()] = inID
	} else {
		b.upToDown[inID.Key()] = outID
		b.downToUp[outID.Key()] = inID
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if downstream {
			delete(b.downToUp, inID.Key())
			delete(b.upToDown, outID.Key())
		} else {
			delete(b.upToDown, inID.Key())
			delete(b.downToUp, outID.Key())
		}
		b.mu.Unlock()
	}()

	res, err := to.CallRawWithID(ctx, outID, method, params)
	if err != nil {
		var rpcErr *acp.RPCError
		if errors.As(err, &rpcErr) {
			return nil, rpcErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, acp.ErrConnClosed) {
			return nil, acp.Errorf(acp.CodeRequestCancelled, "bridge forward %s: %v", method, err)
		}
		b.logf("bridge: forward %s: %v", method, err)
		return nil, acp.Errorf(acp.CodeInternalError, "bridge forward %s: %v", method, err)
	}
	return res, nil
}

// rewriteCancel translates an id belonging to one connection into the id used
// on the other. downToUp maps editor ids -> agent ids, upToDown the reverse.
func (b *Bridge) rewriteCancel(params json.RawMessage, downstream bool) json.RawMessage {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(params, &body); err != nil {
		return params
	}
	rawID, ok := body["requestId"]
	if !ok {
		return params
	}
	var id acp.RequestID
	if err := id.UnmarshalJSON(rawID); err != nil {
		return params
	}
	b.mu.Lock()
	var mapped acp.RequestID
	var found bool
	if downstream {
		mapped, found = b.downToUp[id.Key()]
	} else {
		mapped, found = b.upToDown[id.Key()]
	}
	b.mu.Unlock()
	if !found {
		return params
	}
	encoded, err := json.Marshal(map[string]any{"requestId": mapped})
	if err != nil {
		return params
	}
	return encoded
}

func (b *Bridge) logf(format string, args ...any) {
	if b.opts.Logger != nil {
		b.opts.Logger.Printf(format, args...)
	}
}

// Run starts both read loops and returns when either side closes. Both Conns
// are closed before returning so no goroutine is left behind.
func (b *Bridge) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	go func() { errCh <- b.down.Run(ctx) }()
	go func() { errCh <- b.up.Run(ctx) }()
	err := <-errCh
	b.down.Close()
	b.up.Close()
	if errors.Is(err, io.EOF) || errors.Is(err, acp.ErrConnClosed) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (b *Bridge) Close() {
	b.down.Close()
	b.up.Close()
}
