package worker

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Every handler here is synthetic: no service, model, file, or node action is
// executed. Buffered observations and positive completion fences avoid treating
// a goroutine's entry as retirement of its lane or in-flight ownership.
type laneFIFOHandler struct {
	jobType string
	started chan string
	release <-chan struct{}
}

func (h *laneFIFOHandler) CanHandle(jobType string) bool { return jobType == h.jobType }

func (h *laneFIFOHandler) Execute(ctx context.Context, job *Job, _ StreamWriter) (*JobResult, error) {
	h.started <- job.ID
	if h.release != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"fixture": true}}, nil
}

func waitLaneFIFOJobs(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic lane jobs did not retire")
	}
}

func dispatchLaneFIFOJob(t *testing.T, ctx context.Context, r *Runner, l *lane, id string, wg *sync.WaitGroup) {
	t.Helper()
	job := &Job{ID: id, Type: JobTypeServiceStart, Payload: map[string]any{}}
	// Mirror the real fetch loop's dispatch reservation, then use the actual
	// claim and dispatch functions. No buffered source is activated here.
	r.drainMu.Lock()
	r.dispatchingJobs++
	r.drainMu.Unlock()
	proceed, stream, start := r.claimJob(ctx, job)
	if !proceed {
		r.drainMu.Lock()
		r.dispatchingJobs--
		r.drainMu.Unlock()
		t.Fatal("synthetic job was not claimed")
	}
	r.dispatchLane(ctx, l, job, stream, start, wg)
}

func TestRunnerFIFOClaimOrderWithExecSlotHeld(t *testing.T) {
	const count = 12
	source := NewMockJobSource("fifo", nil)
	handler := &laneFIFOHandler{jobType: JobTypeServiceStart, started: make(chan string, count)}
	r := NewRunner(source, []JobHandler{handler}, RunnerConfig{State: NewWorkerState(), ActivityFn: func(string, string) {}})
	l := newLane("serialized-fifo", count, 1, false)
	l.exec <- struct{}{}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { <-l.exec }) }
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); release(); waitLaneFIFOJobs(t, &wg) })
	want := make([]string, count)
	for i := range want {
		want[i] = fmt.Sprintf("claim-%02d", i)
		dispatchLaneFIFOJob(t, ctx, r, l, want[i], &wg)
	}
	if got := l.snapshot(); got.Queued != count || got.Executing != 0 || r.ActiveJobs() != count {
		t.Fatalf("queued claim accounting changed: %+v active=%d", got, r.ActiveJobs())
	}
	release()
	waitLaneFIFOJobs(t, &wg)
	if got := jobIDs(source.AckedJobs()); !reflect.DeepEqual(got, want) {
		t.Fatalf("serialized completion order = %v, claim order = %v", got, want)
	}
	if got := r.state.Snapshot(); got.InFlight != 0 || got.Executing != 0 || got.Processed != count || r.ActiveJobs() != 0 {
		t.Fatalf("finished jobs retained ownership: %+v active=%d", got, r.ActiveJobs())
	}
	if got := l.snapshot(); got.Queued != 0 || got.Executing != 0 {
		t.Fatalf("finished lane retained queued/executing work: %+v", got)
	}
	assertLaneExecQueue(t, l)
}

func TestRunnerFIFOMiddleTimeoutIncludesPredecessorWait(t *testing.T) {
	source := NewMockJobSource("fifo", nil)
	handler := &laneFIFOHandler{jobType: JobTypeLLMInference, started: make(chan string, 3)}
	fast := NewRunner(source, []JobHandler{handler}, RunnerConfig{State: NewWorkerState(), ActivityFn: func(string, string) {}})
	slow := NewRunner(source, []JobHandler{handler}, RunnerConfig{State: NewWorkerState(), ActivityFn: func(string, string) {}})
	fast.inferenceQueueWait = 10 * time.Millisecond
	slow.inferenceQueueWait = time.Second
	l := newLane("inference-fifo", 3, 1, true)
	first, middle, last := l.reserveExecTicket(), l.reserveExecTicket(), l.reserveExecTicket()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		l.releaseExecTicket(first)
		l.releaseExecTicket(middle)
		l.releaseExecTicket(last)
		waitLaneFIFOJobs(t, &wg)
	})
	launch := func(r *Runner, id string, ticket *laneExecTicket, stream StreamWriter, done chan bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job := &Job{ID: id, Type: JobTypeLLMInference, Payload: map[string]any{"model": "fixture"}}
			ok := r.runLaneJob(ctx, l, job, stream, time.Now(), ticket)
			if done != nil {
				done <- ok
			}
		}()
	}
	// The exec slot is FREE. The middle job must still exhaust its budget
	// behind the first ticket, before the predecessor has even been started.
	middleDone := make(chan bool, 1)
	middleStream := &MockStreamWriter{}
	launch(fast, "middle", middle, middleStream, middleDone)
	select {
	case ok := <-middleDone:
		if !ok {
			t.Fatal("queue expiry must retain the warming success contract")
		}
	case <-time.After(time.Second):
		t.Fatal("queue budget excluded predecessor wait")
	}
	if !middleStream.ended || middleStream.endResult["status"] != "model_warming" || len(source.NackedJobs()) != 0 {
		t.Fatal("middle expiry lost its warming terminal acknowledgement")
	}
	assertLaneExecQueue(t, l, first, last)
	select {
	case id := <-handler.started:
		t.Fatalf("job executed before its predecessor: %s", id)
	default:
	}
	launch(slow, "last", last, &MockStreamWriter{}, nil)
	launch(slow, "first", first, &MockStreamWriter{}, nil)
	waitLaneFIFOJobs(t, &wg)
	if len(handler.started) != 2 {
		t.Fatalf("surviving jobs did not both execute: starts=%d", len(handler.started))
	}
	got := []string{<-handler.started, <-handler.started}
	if !reflect.DeepEqual(got, []string{"first", "last"}) {
		t.Fatalf("timeout let a successor leapfrog: %v", got)
	}
	if got := jobIDs(source.AckedJobs()); !reflect.DeepEqual(got, []string{"middle", "first", "last"}) {
		t.Fatalf("warming or surviving completion contract changed: %v", got)
	}
	assertLaneExecQueue(t, l)
}

func TestRunnerFIFOShutdownNacksQueuedChainWithoutReleasingExec(t *testing.T) {
	const count = 12
	source := NewMockJobSource("fifo", nil)
	handler := &laneFIFOHandler{jobType: JobTypeServiceStart, started: make(chan string, count)}
	r := NewRunner(source, []JobHandler{handler}, RunnerConfig{State: NewWorkerState(), ActivityFn: func(string, string) {}})
	l := newLane("shutdown-fifo", count, 1, false)
	l.exec <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		select {
		case <-l.exec:
		default:
			t.Error("queued cancellation consumed an externally held execution slot")
		}
		waitLaneFIFOJobs(t, &wg)
	})
	for i := 0; i < count; i++ {
		dispatchLaneFIFOJob(t, ctx, r, l, fmt.Sprintf("queued-%d", i), &wg)
	}
	cancel()
	waitLaneFIFOJobs(t, &wg)
	if len(source.NackedJobs()) != count || len(source.AckedJobs()) != 0 || len(handler.started) != 0 {
		t.Fatalf("shutdown executed or terminally acknowledged queued jobs: nack=%d ack=%d starts=%d", len(source.NackedJobs()), len(source.AckedJobs()), len(handler.started))
	}
	if got := r.state.Snapshot(); got.InFlight != 0 || got.Executing != 0 || r.ActiveJobs() != 0 {
		t.Fatalf("shutdown retained ownership: %+v active=%d", got, r.ActiveJobs())
	}
	assertLaneExecQueue(t, l)
}

func TestRunnerFIFOOrdersAcquisitionWithoutSerializingConcurrentLane(t *testing.T) {
	source := NewMockJobSource("fifo", nil)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	handler := &laneFIFOHandler{jobType: JobTypeServiceStart, started: make(chan string, 2), release: release}
	r := NewRunner(source, []JobHandler{handler}, RunnerConfig{State: NewWorkerState(), ActivityFn: func(string, string) {}})
	l := newLane("concurrent-fifo", 2, 2, false)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); unblock(); waitLaneFIFOJobs(t, &wg) })
	dispatchLaneFIFOJob(t, ctx, r, l, "first", &wg)
	dispatchLaneFIFOJob(t, ctx, r, l, "second", &wg)
	for i := 0; i < 2; i++ {
		select {
		case <-handler.started:
		case <-time.After(time.Second):
			t.Fatal("ticket ownership serialized a lane with two execution slots")
		}
	}
	if got := l.snapshot(); got.Executing != 2 || got.Queued != 0 {
		t.Fatalf("execution capacity changed: %+v", got)
	}
	unblock()
	waitLaneFIFOJobs(t, &wg)
	assertLaneExecQueue(t, l)
}

func TestRunnerFIFOAlreadyCancelledOrExpiredNeverExecutes(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired-%t", expired), func(t *testing.T) {
			source := NewMockJobSource("fifo", nil)
			handler := &laneFIFOHandler{jobType: JobTypeLLMInference, started: make(chan string, 1)}
			r := NewRunner(source, []JobHandler{handler}, RunnerConfig{State: NewWorkerState(), ActivityFn: func(string, string) {}})
			r.inferenceQueueWait = 0
			l := newLane("preflight-fifo", 1, 1, expired)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !expired {
				cancel()
			}
			job := &Job{ID: "preflight", Type: JobTypeLLMInference, Payload: map[string]any{}}
			r.runLaneJob(ctx, l, job, &MockStreamWriter{}, time.Now(), l.reserveExecTicket())
			if len(handler.started) != 0 || len(l.exec) != 0 {
				t.Fatal("an already-cancelled/expired queue acquired execution authority")
			}
			if expired && len(source.AckedJobs()) != 1 || !expired && len(source.NackedJobs()) != 1 {
				t.Fatal("pre-execution refusal changed warming/cancellation contracts")
			}
			assertLaneExecQueue(t, l)
		})
	}
}
