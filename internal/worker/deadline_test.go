package worker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ctxHandler honors its context: it returns as soon as the deadline elapses,
// recording that it observed cancellation.
type ctxHandler struct {
	jobType   string
	sawCancel atomic.Bool
}

func (h *ctxHandler) CanHandle(jobType string) bool { return h.jobType == jobType }

func (h *ctxHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	<-ctx.Done()
	h.sawCancel.Store(true)
	return nil, ctx.Err()
}

// signalHandler closes done the first time it runs, so a test can observe that
// the job loop reached a later job.
type signalHandler struct {
	jobType string
	done    chan struct{}
	once    sync.Once
}

func newSignalHandler(jobType string) *signalHandler {
	return &signalHandler{jobType: jobType, done: make(chan struct{})}
}

func (h *signalHandler) CanHandle(jobType string) bool { return h.jobType == jobType }

func (h *signalHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	h.once.Do(func() { close(h.done) })
	return &JobResult{Status: JobStatusSuccess}, nil
}

// hangingHandler blocks forever (until release is closed) and deliberately
// IGNORES its context, modeling the dominant wedge case: a handler that does
// not honor cancellation. It proves the runner's watchdog -- not handler
// cooperation -- is what unblocks the job loop at the deadline (aceteam#6000).
type hangingHandler struct {
	jobType string
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func newHangingHandler(jobType string) *hangingHandler {
	return &hangingHandler{
		jobType: jobType,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (h *hangingHandler) CanHandle(jobType string) bool { return h.jobType == jobType }

func (h *hangingHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	h.once.Do(func() { close(h.started) })
	<-h.release // block until the test releases us; never observe ctx
	return &JobResult{Status: JobStatusSuccess}, nil
}

// recordingFactory hands out one MockStreamWriter per job and remembers them so
// the test can assert which terminal events were published for which job.
type recordingFactory struct {
	mu      sync.Mutex
	writers map[string]*MockStreamWriter
}

func newRecordingFactory() *recordingFactory {
	return &recordingFactory{writers: make(map[string]*MockStreamWriter)}
}

func (f *recordingFactory) factory(job *Job) StreamWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &MockStreamWriter{}
	f.writers[job.ID] = w
	return w
}

func (f *recordingFactory) get(jobID string) *MockStreamWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writers[jobID]
}

// TestExecuteWithDeadlineUnblocksHungHandler is the core regression test: a
// handler that blocks forever and ignores cancellation, dispatched with a
// per-job timeout budget, must NOT wedge the sequential job loop. Within ~the
// deadline the runner publishes the terminal error and advances to the next
// job.
func TestExecuteWithDeadlineUnblocksHungHandler(t *testing.T) {
	hang := newHangingHandler("HANG_JOB")
	// Let the leaked goroutine exit at test end instead of blocking forever.
	defer close(hang.release)

	normal := newSignalHandler("TEST_JOB")

	jobs := []*Job{
		// job-1 hangs, but carries an 80ms budget so the watchdog abandons it.
		{ID: "job-1", Type: "HANG_JOB", Payload: map[string]any{"timeout_ms": float64(80)}},
		// job-2 must still run -- proof the loop was not wedged.
		{ID: "job-2", Type: "TEST_JOB", Payload: map[string]any{}},
	}

	source := NewMockJobSource("test", jobs)
	factory := newRecordingFactory()
	runner := NewRunner(source, []JobHandler{hang, normal}, RunnerConfig{WorkerID: "test"})
	runner.WithStreamWriterFactory(factory.factory)

	// Run keeps polling until ctx is cancelled; drive it in a goroutine and stop
	// it once the loop has demonstrably reached job-2.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	start := time.Now()
	go func() {
		runner.Run(ctx)
		close(runDone)
	}()

	select {
	case <-normal.done:
		// The loop advanced to job-2 despite job-1 hanging, and did so near
		// job-1's 80ms deadline -- not the multi-second ctx bound.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("second job ran after %s; the hung job appears to have blocked the loop", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second job never ran; the loop was wedged by the hung handler")
	}
	cancel()
	<-runDone

	// The hung handler actually started (so we exercised the timeout, not a skip).
	select {
	case <-hang.started:
	default:
		t.Fatal("hanging handler never started; the timeout path was not exercised")
	}

	// job-1 published a terminal ERROR carrying the deadline reason.
	w1 := factory.get("job-1")
	if w1 == nil || !w1.errored {
		t.Fatal("job-1 did not publish a terminal error event")
	}
	if w1.erroredErr == nil || !strings.Contains(w1.erroredErr.Error(), "execution deadline") {
		t.Errorf("job-1 error = %v, want an execution-deadline message", w1.erroredErr)
	}
	if w1.erroredRecover {
		t.Error("deadline error should be published as non-recoverable")
	}

	// job-1 was Failed (terminal DLQ, NOT Nacked): a watchdog abandon must not
	// be retried into a repeated wedge (issue #548). job-2 acked.
	if f := source.FailedJobs(); len(f) != 1 || f[0].ID != "job-1" {
		t.Errorf("failed = %v, want [job-1]", f)
	}
	if n := source.NackedJobs(); len(n) != 0 {
		t.Errorf("nacked = %v, want [] (deadline abandon should Fail, not Nack)", n)
	}
	if a := source.AckedJobs(); len(a) != 1 || a[0].ID != "job-2" {
		t.Errorf("acked = %v, want [job-2]", a)
	}
	// The failure data marks it as an agent-side deadline abandon.
	if d := source.FailedData(); len(d) != 1 || d[0]["deadline_exceeded"] != true {
		t.Errorf("failed data = %v, want deadline_exceeded=true", d)
	}
}

// TestExecuteWithDeadlineHonorsCancellingHandler verifies the cooperative path:
// a handler that DOES honor ctx returns promptly when the deadline elapses, and
// the runner still reports the clear deadline error.
func TestExecuteWithDeadlineHonorsCancellingHandler(t *testing.T) {
	coop := &ctxHandler{jobType: "COOP_JOB"}
	jobs := []*Job{
		{ID: "job-1", Type: "COOP_JOB", Payload: map[string]any{"timeout_ms": float64(50)}},
	}
	source := NewMockJobSource("test", jobs)
	factory := newRecordingFactory()
	runner := NewRunner(source, []JobHandler{coop}, RunnerConfig{WorkerID: "test"})
	runner.WithStreamWriterFactory(factory.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner.Run(ctx)

	if !coop.sawCancel.Load() {
		t.Error("cooperative handler did not observe context cancellation")
	}
	w := factory.get("job-1")
	if w == nil || !w.errored || w.erroredErr == nil ||
		!strings.Contains(w.erroredErr.Error(), "execution deadline") {
		t.Errorf("job-1 terminal error = %v, want deadline message", w)
	}
}

func TestJobExecTimeout(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		wantOK  bool
		wantDur time.Duration
	}{
		{"absent", map[string]any{}, false, 0},
		{"nil payload", nil, false, 0},
		{"float64 (json)", map[string]any{"timeout_ms": float64(1500)}, true, 1500 * time.Millisecond},
		{"int", map[string]any{"timeout_ms": 250}, true, 250 * time.Millisecond},
		{"string", map[string]any{"timeout_ms": "2000"}, true, 2000 * time.Millisecond},
		{"zero (opt-out)", map[string]any{"timeout_ms": float64(0)}, false, 0},
		{"negative (opt-out)", map[string]any{"timeout_ms": float64(-5)}, false, 0},
		{"garbage string", map[string]any{"timeout_ms": "soon"}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var job *Job
			if tc.payload != nil || tc.name == "nil payload" {
				job = &Job{ID: "j", Payload: tc.payload}
			}
			dur, ok := jobExecTimeout(job)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && dur != tc.wantDur {
				t.Errorf("dur = %s, want %s", dur, tc.wantDur)
			}
		})
	}
}

// TestResolveJobTimeout covers the #548 fallback tiers: an explicit payload
// budget always wins; otherwise a generous per-class default applies; unbounded
// classes (model pulls, builds, provision) get no fallback cap; and either tier
// can be tuned or disabled by env.
func TestResolveJobTimeout(t *testing.T) {
	r := &Runner{}

	t.Run("explicit payload wins over fallback", func(t *testing.T) {
		job := &Job{Type: JobTypeLLMInference, Payload: map[string]any{"timeout_ms": float64(1234)}}
		d, ok := r.resolveJobTimeout(job)
		if !ok || d != 1234*time.Millisecond {
			t.Fatalf("got (%s, %v), want (1.234s, true)", d, ok)
		}
	})

	t.Run("default tier for an ordinary job type", func(t *testing.T) {
		d, ok := r.resolveJobTimeout(&Job{Type: JobTypeLLMInference})
		if !ok || d != defaultJobTimeoutSeconds*time.Second {
			t.Fatalf("got (%s, %v), want (%ds, true)", d, ok, defaultJobTimeoutSeconds)
		}
	})

	t.Run("transcribe uses the default tier and exceeds its own 32min self-bound", func(t *testing.T) {
		d, ok := r.resolveJobTimeout(&Job{Type: JobTypeTranscribeAudio})
		if !ok || d <= 33*time.Minute {
			t.Fatalf("got (%s, %v), want a bound comfortably above ~32min", d, ok)
		}
	})

	t.Run("long tier for a session job type", func(t *testing.T) {
		d, ok := r.resolveJobTimeout(&Job{Type: JobTypeMeetingJoin})
		if !ok || d != defaultLongJobTimeoutSeconds*time.Second {
			t.Fatalf("got (%s, %v), want (%ds, true)", d, ok, defaultLongJobTimeoutSeconds)
		}
	})

	t.Run("unbounded types get no fallback cap (must not kill model pulls)", func(t *testing.T) {
		for _, jt := range []string{JobTypeModelCachePull, JobTypeDownloadModel, JobTypeOllamaPull, JobTypeServiceStart, JobTypeAndroidBuild, JobTypeInstanceProvision} {
			if _, ok := r.resolveJobTimeout(&Job{Type: jt}); ok {
				t.Errorf("%s got a fallback cap; want unbounded (ok=false)", jt)
			}
		}
	})

	t.Run("unbounded type still honors an explicit payload budget", func(t *testing.T) {
		job := &Job{Type: JobTypeModelCachePull, Payload: map[string]any{"timeout_ms": float64(5000)}}
		if d, ok := r.resolveJobTimeout(job); !ok || d != 5*time.Second {
			t.Fatalf("got (%s, %v), want (5s, true)", d, ok)
		}
	})

	t.Run("env override tunes the default tier", func(t *testing.T) {
		t.Setenv(jobTimeoutDefaultEnvVar, "120")
		d, ok := r.resolveJobTimeout(&Job{Type: JobTypeLLMInference})
		if !ok || d != 120*time.Second {
			t.Fatalf("got (%s, %v), want (2m, true)", d, ok)
		}
	})

	t.Run("env 0 disables the default tier (unbounded)", func(t *testing.T) {
		t.Setenv(jobTimeoutDefaultEnvVar, "0")
		if _, ok := r.resolveJobTimeout(&Job{Type: JobTypeLLMInference}); ok {
			t.Fatal("default tier set to 0 should be unbounded (ok=false)")
		}
	})
}
