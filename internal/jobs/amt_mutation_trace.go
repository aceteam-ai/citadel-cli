package jobs

import (
	"context"
	"sync/atomic"
)

type amtMutationTraceKey struct{}

// AMTMutationTrace is shared by an AMT handler and its runner. A watchdog may
// abandon the handler while a mutating network write is still in progress, so
// the runner cannot rely only on the handler's eventual error.
type AMTMutationTrace struct {
	inFlight atomic.Int32
	possible atomic.Bool
}

func WithAMTMutationTrace(ctx context.Context) (context.Context, *AMTMutationTrace) {
	trace := &AMTMutationTrace{}
	return context.WithValue(ctx, amtMutationTraceKey{}, trace), trace
}

func AMTMutationTraceFrom(ctx context.Context) *AMTMutationTrace {
	trace, _ := ctx.Value(amtMutationTraceKey{}).(*AMTMutationTrace)
	return trace
}

// BeginWrite covers the interval when a watchdog could return before the
// network write reports how many bytes reached the target.
func (t *AMTMutationTrace) BeginWrite() {
	if t != nil {
		t.inFlight.Add(1)
	}
}

func (t *AMTMutationTrace) EndWrite(mayHaveApplied bool) {
	if t == nil {
		return
	}
	if mayHaveApplied {
		t.possible.Store(true)
	}
	t.inFlight.Add(-1)
}

func (t *AMTMutationTrace) Possible() bool {
	// Read the active write first. EndWrite stores possible before clearing
	// inFlight, so a reader spanning that transition sees at least one state.
	return t != nil && (t.inFlight.Load() > 0 || t.possible.Load())
}
