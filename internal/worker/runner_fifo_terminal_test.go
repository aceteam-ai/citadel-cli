package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type laneFIFOHeldTerminalStream struct {
	*MockStreamWriter
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *laneFIFOHeldTerminalStream) WriteEnd(result map[string]any) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.MockStreamWriter.WriteEnd(result)
}

type laneFIFOHeldNackSource struct {
	*MockJobSource
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *laneFIFOHeldNackSource) Nack(ctx context.Context, job *Job, err error) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.MockJobSource.Nack(ctx, job, err)
}

// runLaneJob first evaluates Done while selecting the ready ticket, then again
// while selecting the free execution slot. Cancel the real context at that
// second boundary: both exec and cancellation are ready, with an honest Err.
// Either select outcome is valid and must release all waiting authority before
// calling a potentially slow Nack. No context error is fabricated by this seam.
type laneFIFOSecondDoneContext struct {
	context.Context
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *laneFIFOSecondDoneContext) Done() <-chan struct{} {
	if c.calls.Add(1) == 2 {
		c.cancel()
	}
	return c.Context.Done()
}

// Observe the actual execution slot, not an Err-call count. This fresh lane
// has no other slot owner: pre-acquisition checks see an empty channel; the
// post-acquisition check observes the real send, cancels the underlying context,
// and returns that context's honest error. This isolates the acquired-slot
// rollback branch without a production hook or a random simultaneous select.
type laneFIFOAcquiredCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	lane     *lane
	observed atomic.Bool
}

func (c *laneFIFOAcquiredCancelContext) Err() error {
	if len(c.lane.exec) > 0 {
		c.observed.Store(true)
		c.cancel()
	}
	return c.Context.Err()
}

func startLaneFIFOTerminalJob(ctx context.Context, r *Runner, l *lane, id string, ticket *laneExecTicket, stream StreamWriter, wg *sync.WaitGroup) <-chan bool {
	done := make(chan bool, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		job := &Job{ID: id, Type: JobTypeLLMInference, Payload: map[string]any{"model": "fixture"}}
		done <- r.runLaneJob(ctx, l, job, stream, time.Now(), ticket)
	}()
	return done
}

func waitLaneFIFOTerminalEntry(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic terminal I/O did not reach its blocking gate")
	}
}

func waitLaneFIFOTerminalResult(t *testing.T, done <-chan bool) bool {
	t.Helper()
	select {
	case ok := <-done:
		return ok
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic job could not finish while another terminal operation was held")
		return false
	}
}

func TestRunnerFIFOExpiryReleasesTicketBeforeSlowWarmingEnd(t *testing.T) {
	source := NewMockJobSource("warming-terminal", nil)
	handler := &laneFIFOHandler{jobType: JobTypeLLMInference, started: make(chan string, 2)}
	expiring := NewRunner(source, []JobHandler{handler}, RunnerConfig{ActivityFn: func(string, string) {}})
	normal := NewRunner(source, []JobHandler{handler}, RunnerConfig{ActivityFn: func(string, string) {}})
	expiring.inferenceQueueWait = 0
	normal.inferenceQueueWait = time.Second
	l := newLane("inference-fifo", 2, 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	stream := &laneFIFOHeldTerminalStream{MockStreamWriter: &MockStreamWriter{}, entered: make(chan struct{}), release: release}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		unblock()
		cancel()
		waitLaneFIFOJobs(t, &wg)
	})
	first := l.reserveExecTicket()
	firstDone := startLaneFIFOTerminalJob(ctx, expiring, l, "expired", first, stream, &wg)
	waitLaneFIFOTerminalEntry(t, stream.entered)
	assertLaneExecTicketRemoved(t, first)
	assertLaneExecQueue(t, l)
	if len(l.exec) != 0 {
		t.Fatal("expired waiter retained an execution slot during warming publication")
	}

	// This tests ticket/slot retirement directly, not release of admission or
	// in-flight accounting before terminal I/O. Those remain caller-owned.
	secondDone := startLaneFIFOTerminalJob(ctx, normal, l, "normal", l.reserveExecTicket(), &MockStreamWriter{}, &wg)
	if !waitLaneFIFOTerminalResult(t, secondDone) {
		t.Fatal("healthy successor did not succeed while warming publication was blocked")
	}
	if got := source.AckedJobs(); len(got) != 1 || got[0].ID != "normal" {
		t.Fatal("healthy successor did not terminally acknowledge before warming publication resumed")
	}
	select {
	case <-firstDone:
		t.Fatal("warming operation retired despite its unopened terminal gate")
	default:
	}
	if len(handler.started) != 1 || <-handler.started != "normal" {
		t.Fatal("expired waiter executed, or healthy successor never executed")
	}
	unblock()
	if !waitLaneFIFOTerminalResult(t, firstDone) {
		t.Fatal("queue expiry lost its warming-success contract")
	}
	waitLaneFIFOJobs(t, &wg)
	if !stream.ended || stream.endResult["status"] != "model_warming" || len(source.AckedJobs()) != 2 || len(source.NackedJobs()) != 0 {
		t.Fatal("slow warming publication changed the terminal acknowledgement contract")
	}
	assertLaneExecQueue(t, l)
	if len(l.exec) != 0 {
		t.Fatal("finished terminal fixture retained an execution slot")
	}
}

func TestRunnerFIFOCancelReleasesSlotAndTicketBeforeSlowNack(t *testing.T) {
	t.Run("simultaneous slot and cancellation", func(t *testing.T) {
		runLaneFIFOSlowNackFixture(t, false)
	})
	t.Run("cancellation after observed acquisition", func(t *testing.T) {
		runLaneFIFOSlowNackFixture(t, true)
	})
}

func runLaneFIFOSlowNackFixture(t *testing.T, cancelAfterAcquisition bool) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	blocked := &laneFIFOHeldNackSource{MockJobSource: NewMockJobSource("cancel-terminal", nil), entered: make(chan struct{}), release: release}
	normalSource := NewMockJobSource("normal-terminal", nil)
	handler := &laneFIFOHandler{jobType: JobTypeLLMInference, started: make(chan string, 2)}
	cancelling := NewRunner(blocked, []JobHandler{handler}, RunnerConfig{ActivityFn: func(string, string) {}})
	normal := NewRunner(normalSource, []JobHandler{handler}, RunnerConfig{ActivityFn: func(string, string) {}})
	// Keep real queue expiry out of the cancellation-boundary oracle.
	cancelling.inferenceQueueWait = time.Hour
	normal.inferenceQueueWait = time.Second
	l := newLane("inference-fifo", 2, 1, true)
	base, cancelFirst := context.WithCancel(context.Background())
	secondDoneCtx := &laneFIFOSecondDoneContext{Context: base, cancel: cancelFirst}
	acquiredCtx := &laneFIFOAcquiredCancelContext{Context: base, cancel: cancelFirst, lane: l}
	var firstCtx context.Context = secondDoneCtx
	if cancelAfterAcquisition {
		firstCtx = acquiredCtx
	}
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		unblock()
		cancelFirst()
		cancelSecond()
		waitLaneFIFOJobs(t, &wg)
	})
	first := l.reserveExecTicket()
	firstDone := startLaneFIFOTerminalJob(firstCtx, cancelling, l, "cancelled", first, &MockStreamWriter{}, &wg)
	waitLaneFIFOTerminalEntry(t, blocked.entered)
	if base.Err() != context.Canceled {
		t.Fatal("fixture did not cancel the real underlying context")
	}
	if cancelAfterAcquisition {
		if !acquiredCtx.observed.Load() {
			t.Fatal("fixture did not observe actual execution-slot ownership before cancellation")
		}
	} else if secondDoneCtx.calls.Load() != 2 {
		t.Fatal("fixture failed to cancel at execution-slot selection")
	}
	assertLaneExecTicketRemoved(t, first)
	assertLaneExecQueue(t, l)
	if len(l.exec) != 0 {
		t.Fatal("cancelled waiter retained an acquired execution slot during Nack")
	}
	if len(handler.started) != 0 || len(blocked.NackedJobs()) != 0 {
		t.Fatal("cancelled waiter executed or completed its gated Nack")
	}

	secondDone := startLaneFIFOTerminalJob(secondCtx, normal, l, "normal", l.reserveExecTicket(), &MockStreamWriter{}, &wg)
	if !waitLaneFIFOTerminalResult(t, secondDone) {
		t.Fatal("healthy successor did not succeed while cancellation Nack was blocked")
	}
	if got := normalSource.AckedJobs(); len(got) != 1 || got[0].ID != "normal" {
		t.Fatal("healthy successor did not acknowledge before cancellation Nack resumed")
	}
	select {
	case <-firstDone:
		t.Fatal("cancelled operation retired despite its unopened Nack gate")
	default:
	}
	if len(handler.started) != 1 || <-handler.started != "normal" {
		t.Fatal("cancelled waiter executed, or healthy successor never executed")
	}
	unblock()
	if waitLaneFIFOTerminalResult(t, firstDone) {
		t.Fatal("cancelled waiter incorrectly reported successful execution")
	}
	waitLaneFIFOJobs(t, &wg)
	if len(blocked.NackedJobs()) != 1 || len(blocked.AckedJobs()) != 0 || len(normalSource.NackedJobs()) != 0 {
		t.Fatal("slow cancellation changed Nack/success acknowledgement ownership")
	}
	assertLaneExecQueue(t, l)
	if len(l.exec) != 0 {
		t.Fatal("finished cancellation fixture retained an execution slot")
	}
}
