package declaredsession

import (
	"context"
	"errors"
	"net"
	"sync"
)

type holderServers struct {
	holder   *holder
	listener *holderListener
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	conns    map[net.Conn]bool
	wg       sync.WaitGroup
}

func newHolderServers(ctx context.Context, holder *holder, listener *holderListener) *holderServers {
	ctx, cancel := context.WithCancel(ctx)
	s := &holderServers{holder: holder, listener: listener, ctx: ctx, cancel: cancel, conns: map[net.Conn]bool{}}
	s.wg.Add(1)
	go s.accept()
	return s
}

func (s *holderServers) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() == nil {
				select {
				case s.holder.requests <- holderRequest{op: "listener_failed"}:
				case <-s.ctx.Done():
				}
			}
			return
		}
		if _, err := sameUIDPeer(conn); err != nil {
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		// Authenticated same-user peers still have a finite pre-attach budget.
		// Queue saturation never creates unlimited handshake goroutines.
		if s.ctx.Err() != nil || len(s.conns) >= 32 {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.conns[conn] = true
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			s.serve(conn)
		}()
	}
}

func (s *holderServers) close() {
	s.cancel()
	s.listener.stopAccept()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *holderServers) ask(ctx context.Context, request holderRequest) (holderReply, error) {
	request.reply = make(chan holderReply, 1)
	select {
	case s.holder.requests <- request:
	case <-ctx.Done():
		return holderReply{}, ErrStopped
	}
	select {
	case reply := <-request.reply:
		return reply, reply.err
	case <-ctx.Done():
		return holderReply{}, ErrStopped
	}
}

func (s *holderServers) serve(conn net.Conn) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	f := &framedConn{conn: conn}
	hello, err := s.ask(ctx, holderRequest{op: "hello"})
	if err != nil || f.write(ctx, hello.frame) != nil {
		return
	}
	for {
		request, err := f.read(ctx, operationWait)
		if err != nil || !validClientFrame(request, false) {
			return
		}
		if request.Op == "attach" {
			s.serveAttachment(ctx, cancel, f, request)
			return
		}
		sent := make(chan struct{})
		reply, err := s.ask(ctx, holderRequest{frame: request, sent: sent})
		if err != nil {
			reply.frame = frame("error")
			reply.frame.Reason = "protocol_error"
		}
		writeErr := f.write(ctx, reply.frame)
		close(sent)
		if err != nil || writeErr != nil || request.Op == "stop" || request.Op == "restart" {
			return
		}
	}
}

func (s *holderServers) serveAttachment(ctx context.Context, cancel context.CancelFunc, f *framedConn, attach wireFrame) {
	id, err := randomIdentity()
	if err != nil {
		return
	}
	disconnected := make(chan string, 1)
	viewer := &holderViewer{id: id, queue: make(chan wireFrame, viewerQueueSize), cancel: cancel}
	viewer.disconnect = func(reason string) {
		if reason != "viewer_detached" {
			select {
			case disconnected <- attachmentDisconnectReason(reason):
			default:
			}
		}
		cancel()
	}
	reply, err := s.ask(ctx, holderRequest{viewer: viewer, frame: attach})
	if err != nil {
		sendAttachmentDisconnect(ctx, f, viewerOperationReason(err))
		return
	}
	defer func() {
		select {
		case s.holder.requests <- holderRequest{op: "leave", viewer: viewer}:
		case <-s.ctx.Done():
		}
	}()
	if f.write(ctx, reply.frame) != nil {
		return
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() {
			viewer.cancel()
			select {
			case reason := <-disconnected:
				sendAttachmentDisconnect(ctx, f, reason)
			default:
			}
		}()
		for _, chunk := range reply.replay {
			out := frame("output")
			out.Sequence, out.Data = chunk.sequence, chunk.data
			if f.write(ctx, out) != nil {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case out := <-viewer.queue:
				if f.write(ctx, out) != nil {
					return
				}
			}
		}
	}()
	defer func() { viewer.cancel(); <-writerDone }()
	for {
		request, err := f.read(ctx, peerReadWait)
		if err != nil {
			return
		}
		if !validClientFrame(request, true) {
			viewer.shutdown("protocol_error")
			return
		}
		if _, err := s.ask(ctx, holderRequest{op: "viewer", viewer: viewer, frame: request}); err != nil {
			if dropRefusedAttachmentInput(request.Op, err) {
				// The actor refused this frame before queue admission. Do not
				// retry or buffer it across a later role/child generation.
				continue
			}
			viewer.shutdown(viewerOperationReason(err))
			return
		}
		if request.Op == "detach" {
			return
		}
	}
}

func dropRefusedAttachmentInput(op string, err error) bool {
	if op != "input" && op != "resize" {
		return false
	}
	return onlyInputRefusal(err, 0)
}

// Only role refusals are transient frame drops. A busy queue disconnects so
// later chunks of a paste cannot continue after an omitted middle chunk.
// Wrapped/joined errors
// must contain no additional failure, and malformed wrapper chains fail closed.
func onlyInputRefusal(err error, depth int) bool {
	if err == ErrRole {
		return true
	}
	if err == nil || depth >= 8 {
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes := wrapped.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, inner := range causes {
			if !onlyInputRefusal(inner, depth+1) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyInputRefusal(wrapped.Unwrap(), depth+1)
	default:
		return false
	}
}

func viewerOperationReason(err error) string {
	switch {
	case errors.Is(err, ErrRole):
		return "controller_required"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrStopped):
		return "holder_stopped"
	default:
		return "protocol_error"
	}
}
