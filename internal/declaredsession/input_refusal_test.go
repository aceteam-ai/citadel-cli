package declaredsession

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestOnlyTransientInputAndResizeRefusalsAreDropped(t *testing.T) {
	for _, op := range []string{"input", "resize", "take_control", "attach", "ping", "detach", "status", ""} {
		for _, test := range []struct {
			err  error
			soft bool
		}{
			{ErrRole, true}, {ErrBusy, false}, {fmt.Errorf("wrapped: %w", ErrRole), true},
			{errors.Join(ErrRole, ErrRole), true}, {errors.Join(ErrRole, ErrBusy), false}, {ErrStopped, false}, {ErrProtocol, false},
			{ErrPeer, false}, {errChildIO, false}, {ErrUnavailable, false}, {nil, false},
			{errors.New("declared session controller required"), false},
			{errors.Join(ErrRole, ErrStopped), false}, {errors.Join(ErrBusy, ErrProtocol), false},
			{errors.Join(ErrRole, errors.New("unknown failure")), false},
		} {
			want := test.soft && (op == "input" || op == "resize")
			if got := dropRefusedAttachmentInput(op, test.err); got != want {
				t.Fatalf("drop op=%q err=%v: %v, want %v", op, test.err, got, want)
			}
		}
	}
}

// This net.Conn is an in-memory framed script, not a socket or PTY. Reads wait
// for the next supplied frame; cancellation interrupts them through deadlines.
type refusalConnection struct {
	input       chan []byte
	written     chan wireFrame
	interrupted chan struct{}
	once        sync.Once
	reader      *bytes.Reader
	mu          sync.Mutex
	output      []byte
}

func newRefusalConnection() *refusalConnection {
	return &refusalConnection{input: make(chan []byte, 8), written: make(chan wireFrame, 32), interrupted: make(chan struct{})}
}

func (conn *refusalConnection) Read(data []byte) (int, error) {
	for conn.reader == nil || conn.reader.Len() == 0 {
		select {
		case <-conn.interrupted:
			return 0, io.EOF
		case raw := <-conn.input:
			conn.reader = bytes.NewReader(raw)
		}
	}
	return conn.reader.Read(data)
}

func (conn *refusalConnection) Write(data []byte) (int, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.output = append(conn.output, data...)
	for len(conn.output) >= 4 {
		size := int(binary.BigEndian.Uint32(conn.output[:4]))
		if len(conn.output) < size+4 {
			break
		}
		out, err := decodeFrame(conn.output[4 : size+4])
		if err != nil {
			return 0, err
		}
		conn.written <- out
		conn.output = conn.output[size+4:]
	}
	return len(data), nil
}

func (conn *refusalConnection) Close() error {
	conn.once.Do(func() { close(conn.interrupted) })
	return nil
}
func (*refusalConnection) LocalAddr() net.Addr  { return memoryAddress{} }
func (*refusalConnection) RemoteAddr() net.Addr { return memoryAddress{} }
func (conn *refusalConnection) SetDeadline(until time.Time) error {
	return conn.SetReadDeadline(until)
}
func (conn *refusalConnection) SetReadDeadline(until time.Time) error {
	if !until.IsZero() && !until.After(time.Now()) {
		return conn.Close()
	}
	return nil
}
func (*refusalConnection) SetWriteDeadline(time.Time) error { return nil }

func sendRefusalFrame(t *testing.T, conn *refusalConnection, request wireFrame) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	conn.input <- append(header[:], raw...)
}

func nextRefusalFrame(t *testing.T, conn *refusalConnection, op string) wireFrame {
	t.Helper()
	select {
	case got := <-conn.written:
		if got.Op != op {
			t.Fatalf("attachment produced %q, want %q (reason %q)", got.Op, op, got.Reason)
		}
		return got
	case <-time.After(time.Second):
		t.Fatalf("attachment did not produce %q on its existing connection", op)
		return wireFrame{}
	}
}

type refusalObservation struct {
	err       error
	unchanged bool
	data      []byte
	cols      uint16
	rows      uint16
}

func TestAttachmentDropsRefusedFramesButPreservesRoleOutputPingAndFreshInput(t *testing.T) {
	for _, reason := range []string{"role_transfer", "restart_wait"} {
		for _, op := range []string{"input", "resize"} {
			t.Run(reason+"/"+op, func(t *testing.T) {
				h, _ := holderFixture()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h.ctx = ctx
				child := newFakeChild()
				h.child = child
				h.terminal = &terminalIO{queue: make(chan inputOperation, 1), done: make(chan struct{}), generation: h.state.ChildGeneration}
				h.cols, h.rows = 80, 24
				server := &holderServers{holder: h, ctx: ctx}
				conn := newRefusalConnection()
				defer conn.Close()
				observed := make(chan refusalObservation, 8)
				actorDone := make(chan struct{})
				go func() {
					defer close(actorDone)
					refused := false
					for {
						select {
						case <-ctx.Done():
							return
						case request := <-h.requests:
							if request.op == "leave" {
								delete(h.viewers, request.viewer.id)
								return
							}
							if request.frame.Op == "attach" {
								h.viewers[request.viewer.id] = request.viewer
								h.controller = request.viewer.id
								switch reason {
								case "role_transfer":
									h.controller = "different-controller"
									h.controlPending = true
								case "restart_wait":
									h.state.Phase = "restart_wait"
								}
								request.reply <- holderReply{frame: frame("role")}
								continue
							}
							if request.frame.Op == "detach" {
								request.reply <- holderReply{}
								continue
							}
							beforeOwner, beforeCols, beforeRows, queued := h.controller, h.cols, h.rows, len(h.terminal.queue)
							reply := h.viewerOperation(request)
							observation := refusalObservation{err: reply.err}
							if !refused {
								observation.unchanged = h.controller == beforeOwner && h.cols == beforeCols && h.rows == beforeRows && len(h.terminal.queue) == queued
								child.mu.Lock()
								observation.unchanged = observation.unchanged && len(child.writes) == 0 && len(child.calls) == 0
								child.mu.Unlock()
								if queued != 0 {
									pending := <-h.terminal.queue
									observation.unchanged = observation.unchanged && string(pending.data) == "already-admitted"
								}
								// The next authorized state is explicit in this fake actor;
								// no refused frame is retained or automatically replayed.
								h.controller, h.controlPending, h.state.Phase = request.viewer.id, false, "running"
								h.send(request.viewer, frame("role"))
								h.publishOutput([]byte("live-after-refusal"))
								refused = true
							} else if request.frame.Op == "input" || request.frame.Op == "resize" {
								select {
								case admitted := <-h.terminal.queue:
									observation.data, observation.cols, observation.rows = admitted.data, admitted.cols, admitted.rows
								default:
								}
							}
							request.reply <- reply
							observed <- observation
						}
					}
				}()
				attachmentDone := make(chan struct{})
				attach := frame("attach")
				attach.Incarnation, attach.Cols, attach.Rows = h.state.Incarnation, 80, 24
				go func() {
					defer close(attachmentDone)
					server.serveAttachment(ctx, cancel, &framedConn{conn: conn}, attach)
				}()
				nextRefusalFrame(t, conn, "role")
				request := frame(op)
				if op == "input" {
					request.Data = []byte("must-be-dropped")
				} else {
					request.Cols, request.Rows = 999, 998
				}
				sendRefusalFrame(t, conn, request)
				var refusal refusalObservation
				select {
				case refusal = <-observed:
				case <-time.After(time.Second):
					t.Fatal("actor did not examine refused frame")
				}
				if !errors.Is(refusal.err, ErrRole) || !refusal.unchanged {
					t.Fatalf("frame was not refused before admission/ownership/dimensions: %+v", refusal)
				}
				nextRefusalFrame(t, conn, "role")
				if output := nextRefusalFrame(t, conn, "output"); string(output.Data) != "live-after-refusal" {
					t.Fatal("refused frame severed later output")
				}
				sendRefusalFrame(t, conn, frame("ping"))
				nextRefusalFrame(t, conn, "pong")
				<-observed
				fresh := frame("input")
				fresh.Data = []byte("fresh-authorized")
				sendRefusalFrame(t, conn, fresh)
				select {
				case accepted := <-observed:
					if accepted.err != nil || string(accepted.data) != "fresh-authorized" {
						t.Fatalf("fresh input not admitted or old input replayed: %+v", accepted)
					}
				case <-time.After(time.Second):
					t.Fatal("fresh input could not use the surviving connection")
				}
				resize := frame("resize")
				resize.Cols, resize.Rows = 100, 40
				sendRefusalFrame(t, conn, resize)
				select {
				case accepted := <-observed:
					if accepted.err != nil || accepted.cols != 100 || accepted.rows != 40 || len(accepted.data) != 0 {
						t.Fatalf("fresh resize not admitted: %+v", accepted)
					}
				case <-time.After(time.Second):
					t.Fatal("fresh resize could not use the surviving connection")
				}
				sendRefusalFrame(t, conn, frame("detach"))
				select {
				case <-attachmentDone:
				case <-time.After(time.Second):
					t.Fatal("attachment did not retain finite detach")
				}
				cancel()
				select {
				case <-actorDone:
				case <-time.After(time.Second):
					t.Fatal("fake actor did not stop")
				}
			})
		}
	}
}

func TestAttachmentStillDisconnectsForHardRefusalsAndMalformedFrames(t *testing.T) {
	for _, test := range []struct {
		name   string
		op     string
		err    error
		reason string
	}{
		{"stopped input", "input", ErrStopped, "holder_stopped"},
		{"busy input", "input", ErrBusy, "busy"},
		{"busy resize", "resize", ErrBusy, "busy"},
		{"role takeover", "take_control", ErrRole, "controller_required"},
		{"busy takeover", "take_control", ErrBusy, "busy"},
		{"malformed input", "input", nil, "protocol_error"},
		{"slow viewer", "", nil, "slow_viewer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _ := holderFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := &holderServers{holder: h, ctx: ctx}
			conn := newRefusalConnection()
			defer conn.Close()
			triggerSlow := make(chan struct{}, 1)
			actorDone := make(chan struct{})
			calls := 0
			go func() {
				defer close(actorDone)
				var viewer *holderViewer
				for {
					select {
					case <-ctx.Done():
						return
					case <-triggerSlow:
						viewer.shutdown("slow_viewer")
					case request := <-h.requests:
						if request.op == "leave" {
							return
						}
						if request.frame.Op == "attach" {
							viewer = request.viewer
							request.reply <- holderReply{frame: frame("role")}
							continue
						}
						calls++
						request.reply <- holderReply{err: test.err}
					}
				}
			}()
			attachmentDone := make(chan struct{})
			attach := frame("attach")
			attach.Incarnation, attach.Cols, attach.Rows = h.state.Incarnation, 80, 24
			go func() {
				defer close(attachmentDone)
				server.serveAttachment(ctx, cancel, &framedConn{conn: conn}, attach)
			}()
			nextRefusalFrame(t, conn, "role")
			if test.name == "slow viewer" {
				triggerSlow <- struct{}{}
			} else {
				request := frame(test.op)
				if request.Op == "input" {
					request.Data = []byte("never-authorized")
				} else if request.Op == "resize" {
					request.Cols, request.Rows = 80, 24
				}
				if test.name == "malformed input" {
					request.Reason = "untrusted extra field"
				}
				sendRefusalFrame(t, conn, request)
				if test.name == "busy input" {
					later := frame("input")
					later.Data = []byte("later-paste-chunk-must-not-run")
					sendRefusalFrame(t, conn, later)
				}
			}
			if refusal := nextRefusalFrame(t, conn, "error"); refusal.Reason != test.reason || len(refusal.Data) != 0 {
				t.Fatal("hard refusal lost its static finite disconnect", refusal)
			}
			select {
			case <-attachmentDone:
			case <-time.After(time.Second):
				t.Fatal("hard refusal kept the attachment alive")
			}
			cancel()
			select {
			case <-actorDone:
			case <-time.After(time.Second):
				t.Fatal("hard refusal left a fake actor running")
			}
			wantCalls := 1
			if test.name == "malformed input" || test.name == "slow viewer" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("hard refusal crossed the wrong actor boundary: %d calls, want %d", calls, wantCalls)
			}
		})
	}
}
