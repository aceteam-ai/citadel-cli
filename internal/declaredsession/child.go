package declaredsession

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

var errChildIO = errors.New("declared session terminal unavailable")

// A child is retained until cleanup has proved the group/scope and Reap has
// consumed the exact exec.Cmd once. EOF alone never means process exit.
type holderChild interface {
	Identity() ProcessIdentity
	Read(context.Context, []byte) (int, error)
	Write(context.Context, []byte) (int, error)
	Resize(uint16, uint16) error
	Repaint() error
	Exited() <-chan struct{}
	SignalGroup(int) error
	Reap() (Exit, error)
	Close() error
}

type inputOperation struct {
	epoch uint64
	data  []byte
	cols  uint16
	rows  uint16
	until time.Time
	// A barrier follows revocation and all older operations. Its acknowledgement
	// is the point at which a new controller can be granted authority.
	barrier chan struct{}
}

type terminalIO struct {
	child      holderChild
	epoch      atomic.Uint64
	queue      chan inputOperation
	done       chan struct{}
	cancel     context.CancelFunc
	generation uint64
}

func newTerminalIO(ctx context.Context, child holderChild, generation uint64) *terminalIO {
	ctx, cancel := context.WithCancel(ctx)
	t := &terminalIO{child: child, queue: make(chan inputOperation, viewerQueueSize), done: make(chan struct{}), cancel: cancel, generation: generation}
	go t.run(ctx)
	return t
}

func (t *terminalIO) run(ctx context.Context) {
	defer close(t.done)
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-t.queue:
			if op.barrier != nil {
				close(op.barrier)
				continue
			}
			if op.epoch != t.epoch.Load() || time.Now().After(op.until) {
				continue
			}
			if len(op.data) == 0 {
				// Resize runs in this same serial worker. A takeover cannot be
				// granted until its following barrier has passed this operation.
				_ = t.child.Resize(op.cols, op.rows)
				continue
			}
			writeCtx, cancel := context.WithDeadline(ctx, op.until)
			remaining := op.data
			for len(remaining) > 0 && writeCtx.Err() == nil && op.epoch == t.epoch.Load() {
				// The native adapter does at most one nonblocking syscall/poll
				// slice. Revocation is rechecked before every subsequent write.
				n, err := t.child.Write(writeCtx, remaining)
				if n > 0 {
					remaining = remaining[n:]
				}
				if err != nil {
					break
				}
			}
			cancel()
		}
	}
}

func (t *terminalIO) enqueue(op inputOperation) bool {
	if op.barrier == nil {
		op.until = time.Now().Add(operationWait)
	}
	select {
	case t.queue <- op:
		return true
	default:
		return false
	}
}

func (t *terminalIO) stop() {
	t.cancel()
	<-t.done // join BEFORE closing/reusing the captured PTY descriptor
}

type outputPart struct {
	generation uint64
	data       []byte
	err        error
}

func readTerminal(ctx context.Context, child holderChild, generation uint64, parts chan<- outputPart, done chan<- struct{}) {
	defer close(done)
	for {
		buffer := make([]byte, outputChunkSize)
		n, err := child.Read(ctx, buffer)
		if n > 0 {
			select {
			case parts <- outputPart{generation: generation, data: buffer[:n]}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case parts <- outputPart{generation: generation, err: err}:
			case <-ctx.Done():
			}
			return
		}
	}
}

func waitClosed(ctx context.Context, closed <-chan struct{}, limit time.Duration) error {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-closed:
		return nil
	case <-ctx.Done():
		return errChildIO
	case <-timer.C:
		return errChildIO
	}
}

func isTerminalEOF(err error) bool { return errors.Is(err, io.EOF) }
