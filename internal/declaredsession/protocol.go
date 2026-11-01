package declaredsession

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"
)

// These are proposed S1 implementation ceilings, not native acceptance results.
const (
	ProtocolVersion = 2
	maxFrameBytes   = 64 << 10
	maxInputBytes   = 8 << 10
	outputChunkSize = 16 << 10
	viewerQueueSize = 32
	operationWait   = 5 * time.Second
	heartbeatEvery  = 15 * time.Second
	peerReadWait    = 45 * time.Second
)

var (
	ErrProtocol = errors.New("declared session protocol refused")
	ErrPeer     = errors.New("declared session peer refused")
	ErrRole     = errors.New("declared session controller required")
	ErrStopped  = errors.New("declared session holder stopped")
)

// Hello is an observation authenticated to the local socket peer, not an
// ownership proof. The manager independently compares the live scope/process.
type Hello struct {
	ProtocolVersion     int             `json:"protocol_version"`
	BinaryVersion       string          `json:"binary_version"`
	Name                string          `json:"name"`
	Incarnation         string          `json:"incarnation"`
	Holder              ProcessIdentity `json:"holder"`
	Scope               ScopeIdentity   `json:"scope"`
	DeclarationRevision string          `json:"declaration_revision"`
	ChildGeneration     uint64          `json:"child_generation"`
	Phase               string          `json:"phase"`
}

type AttachOptions struct {
	ReadOnly    bool
	TakeControl bool
	Cols        uint16
	Rows        uint16
}

// AttachEvent.Data is intentionally sensitive PTY output, not diagnostic text.
type AttachEvent struct {
	Kind     string
	Data     []byte
	ReadOnly bool
	Cols     uint16
	Rows     uint16
	Reason   string
}

type wireFrame struct {
	Version     int    `json:"version"`
	Op          string `json:"op"`
	Incarnation string `json:"incarnation,omitempty"`
	ReadOnly    bool   `json:"readonly,omitempty"`
	TakeControl bool   `json:"take_control,omitempty"`
	Cols        uint16 `json:"cols,omitempty"`
	Rows        uint16 `json:"rows,omitempty"`
	Data        []byte `json:"data,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Sequence    uint64 `json:"sequence,omitempty"`
	Hello       *Hello `json:"hello,omitempty"`
	State       *State `json:"state,omitempty"`
}

// Reject duplicate and noncanonical fields at every object depth before typed
// decoding. encoding/json folds field names, so exact duplicate detection alone
// cannot stop op/OP or Unicode-fold aliases selecting a second interpretation.
func uniqueJSON(dec *json.Decoder, depth int) error {
	if depth > 16 {
		return ErrProtocol
	}
	token, err := dec.Token()
	if err != nil {
		return ErrProtocol
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			name, ok := key.(string)
			if err != nil || !ok || !canonicalRecordKey(name) || seen[name] {
				return ErrProtocol
			}
			seen[name] = true
			if uniqueJSON(dec, depth+1) != nil {
				return ErrProtocol
			}
		}
	case '[':
		for dec.More() {
			if uniqueJSON(dec, depth+1) != nil {
				return ErrProtocol
			}
		}
	default:
		return ErrProtocol
	}
	if _, err := dec.Token(); err != nil {
		return ErrProtocol
	}
	return nil
}

func decodeFrame(raw []byte) (wireFrame, error) {
	var frame wireFrame
	if len(raw) == 0 || len(raw) > maxFrameBytes || raw[0] != '{' || !utf8.Valid(raw) {
		return frame, ErrProtocol
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	if uniqueJSON(check, 0) != nil {
		return frame, ErrProtocol
	}
	if _, err := check.Token(); err != io.EOF {
		return frame, ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil || frame.Version != ProtocolVersion || frame.Op == "" {
		return wireFrame{}, ErrProtocol
	}
	return frame, nil
}

type framedConn struct {
	conn net.Conn
	once sync.Once
	gate chan struct{} // one bounded write, never the holder actor/fanout lock
}

func deadline(ctx context.Context, limit time.Duration) time.Time {
	until := time.Now().Add(limit)
	if end, ok := ctx.Deadline(); ok && end.Before(until) {
		return end
	}
	return until
}

func interruptDeadline(ctx context.Context, interrupt func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); interrupt() })
	return func() {
		if !stop() {
			<-done // do not let an old callback corrupt the next operation's deadline
		}
	}
}

func (f *framedConn) read(ctx context.Context, limit time.Duration) (wireFrame, error) {
	if ctx.Err() != nil {
		return wireFrame{}, ErrStopped
	}
	if f.conn.SetReadDeadline(deadline(ctx, limit)) != nil {
		return wireFrame{}, ErrProtocol
	}
	stop := interruptDeadline(ctx, func() { _ = f.conn.SetReadDeadline(time.Now()) })
	defer stop()
	var header [4]byte
	if n, err := io.ReadFull(f.conn, header[:]); err != nil {
		if err == io.EOF && n == 0 {
			// Viewer EOF says only this connection closed, never job/flush success.
			return wireFrame{}, io.EOF
		}
		return wireFrame{}, ErrProtocol
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameBytes {
		return wireFrame{}, ErrProtocol
	}
	raw := make([]byte, int(size))
	if _, err := io.ReadFull(f.conn, raw); err != nil {
		return wireFrame{}, ErrProtocol
	}
	return decodeFrame(raw)
}

func (f *framedConn) write(ctx context.Context, frame wireFrame) error {
	raw, err := json.Marshal(frame)
	if err != nil || len(raw) == 0 || len(raw) > maxFrameBytes {
		return ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, operationWait)
	defer cancel()
	f.once.Do(func() { f.gate = make(chan struct{}, 1) })
	select {
	case f.gate <- struct{}{}:
		defer func() { <-f.gate }()
	case <-ctx.Done():
		return ErrStopped
	}
	if ctx.Err() != nil || f.conn.SetWriteDeadline(deadline(ctx, operationWait)) != nil {
		return ErrStopped
	}
	stop := interruptDeadline(ctx, func() { _ = f.conn.SetWriteDeadline(time.Now()) })
	defer stop()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	for _, part := range [][]byte{header[:], raw} {
		for len(part) != 0 {
			n, err := f.conn.Write(part)
			if err != nil || n <= 0 {
				return ErrProtocol
			}
			part = part[n:]
		}
	}
	return nil
}

func frame(op string) wireFrame { return wireFrame{Version: ProtocolVersion, Op: op} }

func validDimensions(cols, rows uint16) bool {
	return cols >= 1 && cols <= 1000 && rows >= 1 && rows <= 1000
}

func validClientFrame(f wireFrame, attached bool) bool {
	// No client-supplied state, hello, reason or sequence can become authority
	// or a reflected diagnostic. Per-operation fields form a closed allowlist.
	if f.State != nil || f.Hello != nil || f.Reason != "" || f.Sequence != 0 {
		return false
	}
	switch f.Op {
	case "attach":
		return !attached && f.Incarnation != "" && len(f.Data) == 0 && validDimensions(f.Cols, f.Rows) && !(f.ReadOnly && f.TakeControl)
	case "input":
		return attached && f.Incarnation == "" && !f.ReadOnly && !f.TakeControl && f.Cols == 0 && f.Rows == 0 && len(f.Data) > 0 && len(f.Data) <= maxInputBytes
	case "resize":
		return attached && f.Incarnation == "" && !f.ReadOnly && !f.TakeControl && len(f.Data) == 0 && validDimensions(f.Cols, f.Rows)
	case "take_control", "detach", "ping":
		return attached && f.Incarnation == "" && !f.ReadOnly && !f.TakeControl && f.Cols == 0 && f.Rows == 0 && len(f.Data) == 0
	case "status":
		return !attached && f.Incarnation == "" && !f.ReadOnly && !f.TakeControl && f.Cols == 0 && f.Rows == 0 && len(f.Data) == 0
	case "stop", "restart", "reload":
		return !attached && f.Incarnation != "" && !f.ReadOnly && !f.TakeControl && f.Cols == 0 && f.Rows == 0 && len(f.Data) == 0
	}
	return false
}
