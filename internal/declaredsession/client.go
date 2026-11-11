package declaredsession

import (
	"context"
	"sync"
	"time"
)

type Client struct {
	framed *framedConn
	hello  Hello
	mu     sync.Mutex
	stream bool
	closed bool
}

func DialHolder(ctx context.Context, session *SessionStore) (*Client, error) {
	saved, present, err := session.ReadState()
	if err != nil || !present {
		return nil, ErrPeer
	}
	conn, pid, err := dialHolderSocket(ctx, session)
	if err != nil {
		return nil, err
	}
	client := &Client{framed: &framedConn{conn: conn}}
	hello, err := client.framed.read(ctx, operationWait)
	peer, peerErr := observeHolderPeer(pid)
	if err != nil || peerErr != nil || hello.Op != "hello" || hello.Hello == nil || !validHello(*hello.Hello, saved, session.Paths().Name, peer) {
		_ = conn.Close()
		return nil, ErrPeer
	}
	client.hello = *hello.Hello
	return client, nil
}

func validHello(hello Hello, saved State, name string, peer ProcessIdentity) bool {
	return hello.ProtocolVersion == ProtocolVersion && saved.ProtocolVersion == ProtocolVersion && hello.Name == name && saved.Name == name && validIncarnation(hello.Incarnation) && hello.Incarnation == saved.Incarnation && hello.Holder == peer && hello.Holder == saved.Holder && hello.Holder.PID > 0 && hello.Holder.StartTicks != 0 && hello.Holder.UID != 0 && hello.Holder.BootID != "" && hello.Scope == saved.Scope && hello.DeclarationRevision == saved.DeclarationRevision && hello.BinaryVersion == saved.BinaryVersion
}

func (c *Client) Hello() Hello { return c.hello }

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.framed.conn.Close()
}

func (c *Client) request(ctx context.Context, request wireFrame) (wireFrame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream || c.closed {
		return wireFrame{}, ErrStopped
	}
	if c.framed.write(ctx, request) != nil {
		return wireFrame{}, ErrProtocol
	}
	// Lifecycle replies follow cleanup/drain/fsync; the five-second bound is
	// for handshake/write/input, not a promise that graceful stop takes <5s.
	reply, err := c.framed.read(ctx, peerReadWait)
	if err != nil || reply.Op == "error" {
		return wireFrame{}, ErrProtocol
	}
	return reply, nil
}

func (c *Client) Status(ctx context.Context) (State, error) {
	reply, err := c.request(ctx, frame("status"))
	if err != nil || reply.Op != "status" || reply.State == nil || reply.State.Incarnation != c.hello.Incarnation || reply.State.Name != c.hello.Name {
		return State{}, ErrProtocol
	}
	return *reply.State, nil
}

func (c *Client) lifecycle(ctx context.Context, op, expected string) error {
	if expected != c.hello.Incarnation {
		return ErrConflict
	}
	request := frame(op)
	request.Incarnation = expected
	reply, err := c.request(ctx, request)
	if err != nil || reply.Op != "ok" {
		return ErrProtocol
	}
	return nil
}

func (c *Client) Stop(ctx context.Context, expected string) error {
	return c.lifecycle(ctx, "stop", expected)
}

// PrepareRestart only quiesces this incarnation. The manager alone proves the
// exact old scope gone, clears the overlay and launches a fresh declaration.
func (c *Client) PrepareRestart(ctx context.Context, expected string) error {
	return c.lifecycle(ctx, "restart", expected)
}

func (c *Client) Reload(ctx context.Context, expected string) error {
	return c.lifecycle(ctx, "reload", expected)
}

type Attachment struct {
	client   *Client
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.RWMutex
	readOnly bool
	ceiling  bool // requested --readonly is immutable, even across role messages
}

func (c *Client) Attach(ctx context.Context, opts AttachOptions) (*Attachment, error) {
	if !validDimensions(opts.Cols, opts.Rows) || (opts.ReadOnly && opts.TakeControl) {
		return nil, ErrProtocol
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream || c.closed {
		return nil, ErrStopped
	}
	request := frame("attach")
	request.Incarnation, request.ReadOnly, request.TakeControl = c.hello.Incarnation, opts.ReadOnly, opts.TakeControl
	request.Cols, request.Rows = opts.Cols, opts.Rows
	if c.framed.write(ctx, request) != nil {
		return nil, ErrProtocol
	}
	reply, err := c.framed.read(ctx, operationWait)
	if err != nil {
		return nil, ErrProtocol
	}
	if reply.Op == "error" {
		return nil, &attachmentDisconnectError{reason: attachmentDisconnectReason(reply.Reason)}
	}
	if reply.Op != "role" {
		return nil, ErrProtocol
	}
	c.stream = true
	attachCtx, cancel := context.WithCancel(ctx)
	a := &Attachment{client: c, ctx: attachCtx, cancel: cancel, done: make(chan struct{}), readOnly: reply.ReadOnly, ceiling: opts.ReadOnly}
	go a.heartbeat()
	return a, nil
}

func (a *Attachment) heartbeat() {
	defer close(a.done)
	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			_ = a.client.framed.conn.Close()
			return
		case <-ticker.C:
			if a.client.framed.write(a.ctx, frame("ping")) != nil {
				a.cancel()
			}
		}
	}
}

func (a *Attachment) ReadOnly() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ceiling || a.readOnly
}

func (a *Attachment) Next(ctx context.Context) (AttachEvent, error) {
	for {
		f, err := a.client.framed.read(ctx, peerReadWait)
		if err != nil {
			return AttachEvent{}, err
		}
		switch f.Op {
		case "error":
			return AttachEvent{}, &attachmentDisconnectError{reason: attachmentDisconnectReason(f.Reason)}
		case "pong":
			continue
		case "output":
			if len(f.Data) == 0 || len(f.Data) > outputChunkSize {
				return AttachEvent{}, ErrProtocol
			}
		case "role":
			a.mu.Lock()
			a.readOnly = f.ReadOnly
			a.mu.Unlock()
		case "dimensions", "stopped":
		default:
			return AttachEvent{}, ErrProtocol
		}
		readonly := f.ReadOnly
		if f.Op == "role" {
			readonly = a.ReadOnly()
		}
		return AttachEvent{Kind: f.Op, Data: f.Data, ReadOnly: readonly, Cols: f.Cols, Rows: f.Rows, Reason: f.Reason}, nil
	}
}

func (a *Attachment) Input(ctx context.Context, data []byte) error {
	if a.ReadOnly() {
		return ErrRole
	}
	if len(data) == 0 || len(data) > maxInputBytes {
		return ErrProtocol
	}
	f := frame("input")
	f.Data = data
	return a.client.framed.write(ctx, f)
}

func (a *Attachment) Resize(ctx context.Context, cols, rows uint16) error {
	if a.ReadOnly() {
		return ErrRole
	}
	if !validDimensions(cols, rows) {
		return ErrProtocol
	}
	f := frame("resize")
	f.Cols, f.Rows = cols, rows
	return a.client.framed.write(ctx, f)
}

func (a *Attachment) TakeControl(ctx context.Context) error {
	if a.ceiling {
		return ErrRole
	}
	return a.client.framed.write(ctx, frame("take_control"))
}

func (a *Attachment) Close() error {
	// Cancellation/connection close detaches this viewer; it never sends stop.
	a.cancel()
	<-a.done
	return nil
}
