package declaredsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type replayChunk struct {
	sequence uint64
	data     []byte
}

type replayRing struct {
	limit  int
	bytes  int
	chunks []replayChunk
}

const maxReplayChunks = 1024

func (r *replayRing) append(sequence uint64, data []byte) {
	if r.limit <= 0 || len(data) > r.limit {
		// Never turn a PTY chunk into a truncated UTF-8/escape-sequence tail.
		return
	}
	for (r.bytes+len(data) > r.limit || len(r.chunks) >= maxReplayChunks) && len(r.chunks) > 0 {
		r.bytes -= len(r.chunks[0].data)
		r.chunks[0] = replayChunk{}
		r.chunks = r.chunks[1:]
	}
	r.chunks = append(r.chunks, replayChunk{sequence: sequence, data: data})
	r.bytes += len(data)
}

func (r *replayRing) snapshot() []replayChunk {
	// Chunks are immutable; copying only the headers keeps the bounded backlog
	// separate from the live per-viewer queue without copying sensitive bytes.
	return append([]replayChunk(nil), r.chunks...)
}

type holderViewer struct {
	id      string
	ceiling bool // immutable requested/policy readonly ceiling
	queue   chan wireFrame
	cancel  context.CancelFunc
	cols    uint16
	rows    uint16
}

type holderRequest struct {
	op     string
	viewer *holderViewer
	frame  wireFrame
	reply  chan holderReply
	sent   chan struct{}
}

type holderReply struct {
	frame  wireFrame
	replay []replayChunk
	err    error
}

type controlReady struct {
	viewer     string
	epoch      uint64
	generation uint64
	ok         bool
}

func randomIdentity() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", ErrUnavailable
	}
	return hex.EncodeToString(raw[:]), nil
}

func (h *holder) send(viewer *holderViewer, f wireFrame) bool {
	select {
	case viewer.queue <- f:
		return true
	default:
		// No socket write or slow-reader wait occurs inside the holder actor.
		h.removeViewer(viewer.id, "slow_viewer")
		return false
	}
}

func (h *holder) publishOutput(data []byte) {
	h.sequence++
	h.ring.append(h.sequence, data)
	f := frame("output")
	f.Sequence, f.Data = h.sequence, data
	for _, viewer := range h.viewers {
		h.send(viewer, f)
	}
}

func (h *holder) removeViewer(id, reason string) {
	viewer, exists := h.viewers[id]
	if !exists {
		return
	}
	delete(h.viewers, id)
	viewer.cancel()
	if h.controller == id {
		h.controller = ""
		h.transfer("")
	}
	h.event("detach", reason, id, "readonly")
}

// transfer immediately revokes old queued input, but grants the next role only
// after the serial PTY worker acknowledges its barrier. No stale operation can
// race a grant, including an old write that had already reached a poll syscall.
func (h *holder) transfer(id string) bool {
	if h.controlPending || h.terminal == nil {
		return false
	}
	h.epoch++
	h.terminal.epoch.Store(h.epoch)
	h.controller = ""
	h.controlPending = true
	for _, viewer := range h.viewers {
		role := frame("role")
		role.ReadOnly = true
		// A full queue is removed directly here rather than recursively trying
		// to start another controller transfer.
		select {
		case viewer.queue <- role:
		default:
			delete(h.viewers, viewer.id)
			viewer.cancel()
		}
	}
	terminal, epoch, generation := h.terminal, h.epoch, h.state.ChildGeneration
	go func() {
		barrier := make(chan struct{})
		ok := false
		select {
		case terminal.queue <- inputOperation{barrier: barrier}:
			select {
			case <-barrier:
				ok = true
			case <-terminal.done:
			case <-h.ctx.Done():
			}
		case <-terminal.done:
		case <-h.ctx.Done():
		}
		select {
		case h.controls <- controlReady{viewer: id, epoch: epoch, generation: generation, ok: ok}:
		case <-h.ctx.Done():
		}
	}()
	return true
}

func (h *holder) acceptControl(result controlReady) {
	if result.epoch != h.epoch || result.generation != h.state.ChildGeneration {
		return
	}
	h.controlPending = false
	viewer := h.viewers[result.viewer]
	if !result.ok || viewer == nil || viewer.ceiling || h.declaration.Attach.Local != "interactive" || h.terminal == nil {
		return
	}
	h.controller = viewer.id
	if h.terminal.enqueue(inputOperation{epoch: h.epoch, cols: viewer.cols, rows: viewer.rows}) {
		h.cols, h.rows = viewer.cols, viewer.rows
	}
	role := frame("role")
	role.ReadOnly = false
	h.send(viewer, role)
	h.event("controller_changed", "controller_transfer", viewer.id, "controller")
}

func (h *holder) attach(request holderRequest) holderReply {
	viewer := request.viewer
	if h.declaration.Attach.Local == "deny" || len(h.viewers) >= h.declaration.Attach.MaxViewers || request.frame.Incarnation != h.state.Incarnation {
		return holderReply{err: ErrRole}
	}
	viewer.ceiling = request.frame.ReadOnly || h.declaration.Attach.Local != "interactive"
	viewer.cols, viewer.rows = request.frame.Cols, request.frame.Rows
	h.viewers[viewer.id] = viewer
	// Registration and the backlog high-water snapshot are one actor step.
	// Subsequent output only enters this viewer's live queue.
	backlog := h.ring.snapshot()
	role := frame("role")
	role.ReadOnly = true
	if !viewer.ceiling && (h.controller == "" || request.frame.TakeControl) {
		h.transfer(viewer.id)
	}
	dimensions := frame("dimensions")
	dimensions.Cols, dimensions.Rows = h.cols, h.rows
	h.send(viewer, dimensions)
	if h.child != nil && !h.cleaning && !h.quarantined {
		_ = h.child.Repaint() // holder-owned same-size repaint, not viewer resize
	}
	h.event("attach", "", viewer.id, "readonly")
	return holderReply{frame: role, replay: backlog}
}

func (h *holder) viewerOperation(request holderRequest) holderReply {
	viewer := h.viewers[request.viewer.id]
	if viewer == nil {
		return holderReply{err: ErrStopped}
	}
	switch request.frame.Op {
	case "detach":
		h.removeViewer(viewer.id, "viewer_detached")
	case "ping":
		h.send(viewer, frame("pong"))
	case "take_control":
		if viewer.ceiling || h.declaration.Attach.Local != "interactive" || !h.transfer(viewer.id) {
			return holderReply{err: ErrRole}
		}
	case "input", "resize":
		if viewer.ceiling || h.controller != viewer.id || h.controlPending || h.terminal == nil || h.state.Phase != "running" {
			return holderReply{err: ErrRole}
		}
		op := inputOperation{epoch: h.epoch, data: append([]byte(nil), request.frame.Data...), cols: request.frame.Cols, rows: request.frame.Rows}
		if !h.terminal.enqueue(op) {
			return holderReply{err: ErrBusy}
		}
		if request.frame.Op == "resize" {
			h.cols, h.rows = request.frame.Cols, request.frame.Rows
		}
	default:
		return holderReply{err: ErrProtocol}
	}
	return holderReply{}
}
