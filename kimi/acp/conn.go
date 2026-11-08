package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
)

var ErrConnClosed = errors.New("acp: connection closed")

// FrameDirection tells a trace observer whether a frame was read or written.
type FrameDirection string

const (
	FrameInbound  FrameDirection = "in"
	FrameOutbound FrameDirection = "out"
)

// RequestHandler serves one method. Returning an *RPCError produces an error
// response; any other value is JSON-marshalled into result.
type RequestHandler func(ctx context.Context, params json.RawMessage) (any, *RPCError)

// DefaultRequestHandler is a catch-all used by transparent proxies.
type DefaultRequestHandler func(ctx context.Context, method string, params json.RawMessage) (any, *RPCError)

// IDDefaultRequestHandler is a catch-all that also receives the inbound request
// id, which a proxy needs in order to map $/cancel_request across connections.
type IDDefaultRequestHandler func(ctx context.Context, id RequestID, method string, params json.RawMessage) (any, *RPCError)

type NotificationHandler func(ctx context.Context, params json.RawMessage)

type DefaultNotificationHandler func(ctx context.Context, method string, params json.RawMessage)

// ConnOptions controls logging, tracing and cancellation behaviour.
type ConnOptions struct {
	// Logger receives diagnostics; it must never write to an ACP stdout stream.
	Logger *log.Logger
	// Observer sees every raw frame (useful for --trace ndjson evidence).
	Observer func(FrameDirection, []byte)
	// SendCancelRequest emits $/cancel_request when an outbound Call's context
	// is cancelled. Off by default: ACP v1 uses session/cancel for prompt turns.
	SendCancelRequest bool
}

type callResult struct {
	result json.RawMessage
	err    *RPCError
}

type pendingCall struct {
	ch chan callResult
}

type notifyJob struct {
	ctx     context.Context
	method  string
	params  json.RawMessage
	barrier chan struct{} // non-nil for FlushNotifications sentinel jobs
}

// Conn is a full-duplex JSON-RPC endpoint. Either peer may issue requests, so
// Conn has no client/agent distinction; client.Client and agent.Agent add the
// ACP semantics on top of it.
type Conn struct {
	tr   Transport
	opts ConnOptions

	nextID atomic.Int64

	mu            sync.Mutex
	pending       map[string]*pendingCall
	inflight      map[string]context.CancelFunc
	reqHandlers   map[string]RequestHandler
	notifHandlers map[string]NotificationHandler
	defaultReq    DefaultRequestHandler
	defaultReqID  IDDefaultRequestHandler
	defaultNotif  DefaultNotificationHandler
	closeErr      error
	baseCtx       context.Context
	notifyCh      chan notifyJob
	writeMu       sync.Mutex

	done      chan struct{}
	closeOnce sync.Once
}

func NewConn(tr Transport, opts ...ConnOptions) *Conn {
	o := ConnOptions{}
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Logger == nil {
		o.Logger = log.New(io.Discard, "", 0)
	}
	return &Conn{
		tr:            tr,
		opts:          o,
		pending:       make(map[string]*pendingCall),
		inflight:      make(map[string]context.CancelFunc),
		reqHandlers:   make(map[string]RequestHandler),
		notifHandlers: make(map[string]NotificationHandler),
		baseCtx:       context.Background(),
		notifyCh:      make(chan notifyJob, 512),
		done:          make(chan struct{}),
	}
}

func (c *Conn) OnRequest(method string, h RequestHandler) *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqHandlers[method] = h
	return c
}

func (c *Conn) OnNotification(method string, h NotificationHandler) *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifHandlers[method] = h
	return c
}

func (c *Conn) OnDefaultRequest(h DefaultRequestHandler) *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultReq = h
	return c
}

// OnDefaultRequestWithID registers an id-aware catch-all (see
// IDDefaultRequestHandler). It takes precedence over OnDefaultRequest.
func (c *Conn) OnDefaultRequestWithID(h IDDefaultRequestHandler) *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultReqID = h
	return c
}

func (c *Conn) OnDefaultNotification(h DefaultNotificationHandler) *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultNotif = h
	return c
}

func (c *Conn) Done() <-chan struct{} { return c.done }

// FlushNotifications waits until every notification already received has been
// handled. ACP requires session/update frames to precede the session/prompt
// response; because notifications are processed on an ordered background
// worker, callers that need the updates before using a result should flush.
func (c *Conn) FlushNotifications(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	select {
	case c.notifyCh <- notifyJob{barrier: done}:
	case <-c.done:
		return c.closedError()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-c.done:
		return c.closedError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AllocID reserves a fresh request id. Transparent proxies use it together
// with CallRawWithID to keep an explicit id mapping between two connections.
func (c *Conn) AllocID() RequestID { return IntID(c.nextID.Add(1)) }

// CallRaw issues a request and returns the raw result. A JSON-RPC error
// response is returned as *RPCError (use errors.As to inspect the code).
func (c *Conn) CallRaw(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	return c.CallRawWithID(ctx, c.AllocID(), method, params)
}

// CallRawWithID is CallRaw with a caller-chosen id.
func (c *Conn) CallRawWithID(ctx context.Context, id RequestID, method string, params json.RawMessage) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if method == "" {
		return nil, errors.New("acp: empty method")
	}
	if id.IsZero() {
		return nil, errors.New("acp: empty request id")
	}
	p := &pendingCall{ch: make(chan callResult, 1)}

	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return nil, c.closedError()
	default:
	}
	c.pending[id.Key()] = p
	c.mu.Unlock()

	frame, err := NewRequestFrame(id, method, params)
	if err != nil {
		c.removePending(id)
		return nil, err
	}
	if err := c.write(frame); err != nil {
		c.removePending(id)
		return nil, err
	}

	select {
	case r := <-p.ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-ctx.Done():
		c.removePending(id)
		if c.opts.SendCancelRequest {
			// ctx is already done here; Notify would refuse to send. Build the
			// cancellation frame directly so the peer can abort its work.
			if f, err := NewNotificationFrame(MethodCancelRequest, map[string]any{"requestId": id}); err == nil {
				_ = c.write(f)
			}
		}
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.closedError()
	}
}

// Call issues a typed request and decodes result into out (out may be nil).
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	raw, err := marshalParam(params)
	if err != nil {
		return err
	}
	res, err := c.CallRaw(ctx, method, raw)
	if err != nil {
		return err
	}
	if out == nil || len(res) == 0 || string(res) == "null" {
		return nil
	}
	return json.Unmarshal(res, out)
}

// NotifyRaw sends a notification (no response expected).
func (c *Conn) NotifyRaw(method string, params json.RawMessage) error {
	frame, err := NewNotificationFrame(method, params)
	if err != nil {
		return err
	}
	return c.write(frame)
}

func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	raw, err := marshalParam(params)
	if err != nil {
		return err
	}
	return c.NotifyRaw(method, raw)
}

func (c *Conn) write(frame []byte) error {
	select {
	case <-c.done:
		return c.closedError()
	default:
	}
	if c.opts.Observer != nil {
		c.opts.Observer(FrameOutbound, frame)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.tr.Write(frame)
}

func (c *Conn) removePending(id RequestID) {
	c.mu.Lock()
	delete(c.pending, id.Key())
	c.mu.Unlock()
}

func (c *Conn) closedError() error {
	c.mu.Lock()
	cause := c.closeErr
	c.mu.Unlock()
	if cause == nil || errors.Is(cause, ErrConnClosed) || errors.Is(cause, io.EOF) {
		return ErrConnClosed
	}
	return fmt.Errorf("%w: %v", ErrConnClosed, cause)
}

func (c *Conn) shutdown(cause error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closeErr = cause
		for k := range c.pending {
			delete(c.pending, k)
		}
		c.mu.Unlock()
		close(c.done)
		_ = c.tr.Close()
	})
}

// Close terminates the connection and unblocks all pending calls.
func (c *Conn) Close() { c.shutdown(ErrConnClosed) }

// Run reads frames until EOF or error. It must be started before making calls;
// inbound requests run concurrently, notifications run in order on one worker.
func (c *Conn) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	c.baseCtx = runCtx
	c.mu.Unlock()

	go c.notifyLoop(runCtx)
	defer c.Close()

	for {
		frame, err := c.tr.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.opts.Logger.Printf("acp: read failed: %v", err)
			}
			c.shutdown(err)
			return err
		}
		if c.opts.Observer != nil {
			c.opts.Observer(FrameInbound, frame)
		}
		msg, perr := ParseMessage(frame)
		if perr != nil {
			c.opts.Logger.Printf("acp: rejecting frame: %v", perr)
			errFrame, _ := NewErrorFrameNullID(perr)
			_ = c.write(errFrame)
			continue
		}
		switch msg.Type {
		case MessageResponse:
			c.deliver(msg)
		case MessageRequest:
			c.dispatchRequest(msg)
		case MessageNotification:
			c.dispatchNotification(runCtx, msg)
		}
	}
}

func (c *Conn) deliver(msg *Message) {
	if !msg.HasID {
		c.opts.Logger.Printf("acp: ignoring response without id")
		return
	}
	key := msg.ID.Key()
	c.mu.Lock()
	p := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if p == nil {
		c.opts.Logger.Printf("acp: unsolicited response id=%s", key)
		return
	}
	r := callResult{result: msg.Result}
	if msg.Err != nil {
		r.err = msg.Err
	}
	select {
	case p.ch <- r:
	default:
	}
}

type idRequestHandler func(ctx context.Context, id RequestID, params json.RawMessage) (any, *RPCError)

func (c *Conn) requestHandler(method string) idRequestHandler {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h, ok := c.reqHandlers[method]; ok {
		return func(ctx context.Context, _ RequestID, params json.RawMessage) (any, *RPCError) {
			return h(ctx, params)
		}
	}
	if c.defaultReqID != nil {
		d := c.defaultReqID
		return func(ctx context.Context, id RequestID, params json.RawMessage) (any, *RPCError) {
			return d(ctx, id, method, params)
		}
	}
	if c.defaultReq != nil {
		d := c.defaultReq
		return func(ctx context.Context, _ RequestID, params json.RawMessage) (any, *RPCError) {
			return d(ctx, method, params)
		}
	}
	return nil
}

func (c *Conn) dispatchRequest(msg *Message) {
	h := c.requestHandler(msg.Method)
	ctx, cancel := context.WithCancel(c.baseContext())
	key := msg.ID.Key()
	c.mu.Lock()
	c.inflight[key] = cancel
	c.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.opts.Logger.Printf("acp: handler panic for %s: %v", msg.Method, r)
				errFrame, _ := NewErrorFrame(msg.ID, Errorf(CodeInternalError, "handler panic: %v", r))
				_ = c.write(errFrame)
			}
			cancel()
			c.mu.Lock()
			delete(c.inflight, key)
			c.mu.Unlock()
		}()

		var result any
		var rpcErr *RPCError
		if h == nil {
			rpcErr = MethodNotFound(msg.Method)
		} else {
			result, rpcErr = h(ctx, msg.ID, msg.Params)
		}
		if rpcErr == nil && ctx.Err() != nil {
			rpcErr = Errorf(CodeRequestCancelled, "request cancelled: %s", msg.Method)
		}

		var frame []byte
		var err error
		if rpcErr != nil {
			frame, err = NewErrorFrame(msg.ID, rpcErr)
		} else {
			frame, err = NewResultFrame(msg.ID, result)
		}
		if err != nil {
			c.opts.Logger.Printf("acp: cannot encode response for %s: %v", msg.Method, err)
			frame, _ = NewErrorFrame(msg.ID, Errorf(CodeInternalError, "cannot encode result: %v", err))
		}
		if err := c.write(frame); err != nil {
			c.opts.Logger.Printf("acp: cannot write response for %s: %v", msg.Method, err)
		}
	}()
}

func (c *Conn) baseContext() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.baseCtx == nil {
		return context.Background()
	}
	return c.baseCtx
}

func (c *Conn) dispatchNotification(ctx context.Context, msg *Message) {
	if msg.Method == MethodCancelRequest {
		c.handleCancelRequest(msg.Params)
	}
	job := notifyJob{ctx: ctx, method: msg.Method, params: msg.Params}
	select {
	case c.notifyCh <- job:
	case <-c.done:
	default:
		// Extremely deep queue: keep the read loop alive, sacrifice ordering.
		c.opts.Logger.Printf("acp: notification queue full, %s handled out of order", msg.Method)
		go c.handleNotification(job)
	}
}

func (c *Conn) notifyLoop(ctx context.Context) {
	for {
		select {
		case job := <-c.notifyCh:
			c.handleNotification(job)
		case <-c.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *Conn) handleNotification(job notifyJob) {
	if job.barrier != nil {
		close(job.barrier)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.opts.Logger.Printf("acp: notification panic for %s: %v", job.method, r)
		}
	}()
	c.mu.Lock()
	h := c.notifHandlers[job.method]
	d := c.defaultNotif
	c.mu.Unlock()
	switch {
	case h != nil:
		h(job.ctx, job.params)
	case d != nil:
		d(job.ctx, job.method, job.params)
	default:
		// Unknown notifications are ignored by design; ACP is extensible.
	}
}

func (c *Conn) handleCancelRequest(params json.RawMessage) {
	var body struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &body); err != nil || len(body.RequestID) == 0 {
		return
	}
	var id RequestID
	if err := id.UnmarshalJSON(body.RequestID); err != nil {
		return
	}
	c.mu.Lock()
	cancel := c.inflight[id.Key()]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
