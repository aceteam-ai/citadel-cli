package jobs

import (
	"context"
	"sync/atomic"
)

type amtMutationTraceKey struct{}

const (
	amtTraceClosed    uint64 = 1 << 63
	amtTracePossible  uint64 = 1 << 62
	amtTraceCountMask        = amtTracePossible - 1
)

// AMTMutationTrace atomically admits a mutating write or freezes the outcome.
// No first write can begin after a frozen no-write result. An already-possible
// mutation retains that result, allowing the existing bounded boot restoration
// to finish without changing the published classification.
type AMTMutationTrace struct{ state atomic.Uint64 }

func WithAMTMutationTrace(ctx context.Context) (context.Context, *AMTMutationTrace) {
	trace := &AMTMutationTrace{}
	return context.WithValue(ctx, amtMutationTraceKey{}, trace), trace
}
func AMTMutationTraceFrom(ctx context.Context) *AMTMutationTrace {
	trace, _ := ctx.Value(amtMutationTraceKey{}).(*AMTMutationTrace)
	return trace
}

// BeginWrite must succeed immediately before supplying a mutating request or
// event to the transport. A false result forbids that write.
func (t *AMTMutationTrace) BeginWrite() bool {
	if t == nil {
		return true
	}
	for {
		state := t.state.Load()
		if state&amtTraceClosed != 0 {
			return state&amtTracePossible != 0
		}
		if state&amtTraceCountMask == amtTraceCountMask {
			return false
		}
		if t.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}
func (t *AMTMutationTrace) EndWrite(mayHaveApplied bool) {
	if t == nil {
		return
	}
	for {
		state := t.state.Load()
		if state&amtTraceClosed != 0 || state&amtTraceCountMask == 0 {
			return
		}
		next := state - 1
		if mayHaveApplied {
			next |= amtTracePossible
		}
		if t.state.CompareAndSwap(state, next) {
			return
		}
	}
}
func (t *AMTMutationTrace) Possible() bool {
	if t == nil {
		return false
	}
	state := t.state.Load()
	return state&amtTracePossible != 0 || state&amtTraceCountMask != 0
}

// Close freezes one immutable result. An in-flight write is uncertain even if
// its eventual response is an authentication refusal. A completed refusal can
// still clear its in-flight state before this snapshot is taken.
func (t *AMTMutationTrace) Close() bool {
	if t == nil {
		return false
	}
	for {
		state := t.state.Load()
		if state&amtTraceClosed != 0 {
			return state&amtTracePossible != 0
		}
		next := state | amtTraceClosed
		if state&amtTraceCountMask != 0 {
			next |= amtTracePossible
		}
		if t.state.CompareAndSwap(state, next) {
			return next&amtTracePossible != 0
		}
	}
}
