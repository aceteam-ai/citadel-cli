package jobs

import (
	"context"
	"runtime"
	"testing"
)

// A watchdog may read Possible while EndWrite moves from an active write to
// the sticky possible-mutation state. It must never observe a definite failure
// after BeginWrite and before or after a successful EndWrite.
func TestAMTMutationTraceTransitionNeverLooksDefinite(t *testing.T) {
	for i := 0; i < 1000; i++ {
		_, trace := WithAMTMutationTrace(context.Background())
		trace.BeginWrite()
		done := make(chan struct{})
		go func() {
			trace.EndWrite(true)
			close(done)
		}()
		for {
			if !trace.Possible() {
				t.Fatalf("possible mutation disappeared across write completion at iteration %d", i)
			}
			select {
			case <-done:
				if !trace.Possible() {
					t.Fatal("completed write lost its possible-mutation state")
				}
				goto next
			default:
				runtime.Gosched()
			}
		}
	next:
	}
}
