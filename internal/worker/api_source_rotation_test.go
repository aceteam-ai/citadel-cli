package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// Tests for the multi-queue rotation (issue #704): the per-node queue must be
// polled on every iteration of the rotation so a node-pinned job waits at most
// one round trip, while the fungible tag queues stay on the round robin.

const testPerNodeQueue = "jobs:v1:shell:org_test:node:1297"

// consumeCall is one recorded POST to the consume endpoint.
type consumeCall struct {
	Queue   string
	BlockMs int
}

// fakeConsumeServer is an httptest server standing in for the Redis API proxy.
// respond decides what a consume request returns: a job, nothing, or an error.
type fakeConsumeServer struct {
	*httptest.Server

	mu    sync.Mutex
	calls []consumeCall
}

// consumeResponder returns (job, httpStatus). A nil job with status 200 means
// "queue empty"; a non-200 status makes the consume call fail.
type consumeResponder func(call consumeCall, nth int) (*redisapi.StreamMessage, int)

func newFakeConsumeServer(t *testing.T, respond consumeResponder) *fakeConsumeServer {
	t.Helper()
	f := &fakeConsumeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fabric/redis/ping" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/api/fabric/redis/jobs/consume" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req redisapi.ConsumeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := consumeCall{Queue: req.Queue, BlockMs: req.BlockMs}
		f.mu.Lock()
		f.calls = append(f.calls, call)
		nth := len(f.calls)
		f.mu.Unlock()

		msg, status := respond(call, nth)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"queue rejected"}`))
			return
		}
		resp := redisapi.ConsumeResponse{}
		if msg != nil {
			resp.Messages = []redisapi.StreamMessage{*msg}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeConsumeServer) recorded() []consumeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]consumeCall(nil), f.calls...)
}

func (f *fakeConsumeServer) queueSequence() []string {
	var out []string
	for _, c := range f.recorded() {
		out = append(out, c.Queue)
	}
	return out
}

// alwaysEmpty is the default responder: every queue is empty.
func alwaysEmpty(consumeCall, int) (*redisapi.StreamMessage, int) {
	return nil, http.StatusOK
}

func newConnectedSource(t *testing.T, srv *fakeConsumeServer, queues []string, blockMs int) *APISource {
	t.Helper()
	s := NewAPISource(APISourceConfig{
		BaseURL:    srv.URL,
		Token:      "test-token",
		QueueNames: queues,
		BlockMs:    blockMs,
	})
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	return s
}

func jobMessage(jobID string) *redisapi.StreamMessage {
	return &redisapi.StreamMessage{
		ID: "1-0",
		Data: redisapi.StreamMessageData{
			JobID:   jobID,
			Type:    "SHELL_COMMAND",
			Payload: `{"command":"hostname"}`,
		},
	}
}

func TestSplitPriorityQueues(t *testing.T) {
	queues := []string{
		"jobs:v1:cpu-general",
		testPerNodeQueue,
		"jobs:v1:tag:gpu:rtx3090",
		"jobs:v1:shell:org_test",
	}
	priority, rotating := splitPriorityQueues(queues)
	if len(priority) != 1 || priority[0] != testPerNodeQueue {
		t.Fatalf("priority = %v, want [%s]", priority, testPerNodeQueue)
	}
	want := []string{"jobs:v1:cpu-general", "jobs:v1:tag:gpu:rtx3090", "jobs:v1:shell:org_test"}
	if strings.Join(rotating, ",") != strings.Join(want, ",") {
		t.Fatalf("rotating = %v, want %v", rotating, want)
	}
}

func TestQueueBlockBudget(t *testing.T) {
	tests := []struct {
		name                       string
		totalBlockMs               int
		rotating, priority         int
		wantRotatingMs, wantPrioMs int
	}{
		{
			// No per-node queue: identical to the pre-#704 formula
			// (BlockMs/len(queues) floored at minQueueBlockMs).
			name:         "no priority queue keeps old formula",
			totalBlockMs: 5000, rotating: 2, priority: 0,
			wantRotatingMs: 2500, wantPrioMs: 1,
		},
		{
			name:         "no priority queue hits the floor",
			totalBlockMs: 5000, rotating: 12, priority: 0,
			wantRotatingMs: 500, wantPrioMs: 1,
		},
		{
			// 12 queues (11 fungible + per-node): rotating polls hit the
			// floor, and the per-node poll gets the short block.
			name:         "many queues floor the rotating block",
			totalBlockMs: 5000, rotating: 11, priority: 1,
			wantRotatingMs: 500, wantPrioMs: 100,
		},
		{
			// Small queue set: the priority budget is subtracted so a full
			// cycle still fits inside the configured block timeout.
			name:         "few queues keep the cycle within BlockMs",
			totalBlockMs: 5000, rotating: 1, priority: 1,
			wantRotatingMs: 4900, wantPrioMs: 100,
		},
		{
			// A tiny configured block must never produce blockMs < 1, which
			// the server rejects.
			name:         "tiny budget stays valid",
			totalBlockMs: 1, rotating: 1, priority: 1,
			wantRotatingMs: 500, wantPrioMs: 100,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotRotating, gotPrio := queueBlockBudget(tc.totalBlockMs, tc.rotating, tc.priority)
			if gotRotating != tc.wantRotatingMs {
				t.Errorf("rotating block = %d, want %d", gotRotating, tc.wantRotatingMs)
			}
			if tc.priority > 0 && gotPrio != tc.wantPrioMs {
				t.Errorf("priority block = %d, want %d", gotPrio, tc.wantPrioMs)
			}
			if gotPrio < 1 {
				t.Errorf("priority block = %d, must be >= 1 (server rejects 0)", gotPrio)
			}
		})
	}
}

// TestNextMulti_PollsPerNodeQueueEveryIteration is the core of #704: over one
// rotation the per-node queue is visited before every fungible queue, so its
// revisit interval is one round trip rather than one full cycle.
func TestNextMulti_PollsPerNodeQueueEveryIteration(t *testing.T) {
	srv := newFakeConsumeServer(t, alwaysEmpty)
	queues := []string{
		testPerNodeQueue,
		"jobs:v1:tag:gpu:rtx3090",
		"jobs:v1:tag:engine:vllm",
		"jobs:v1:shell:org_test",
	}
	s := newConnectedSource(t, srv, queues, 5000)

	job, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if job != nil {
		t.Fatalf("Next returned a job from empty queues: %+v", job)
	}

	seq := srv.queueSequence()
	// 3 fungible queues => 3 iterations, each: per-node then one fungible.
	if len(seq) != 6 {
		t.Fatalf("consume calls = %d (%v), want 6", len(seq), seq)
	}
	for i, q := range seq {
		if i%2 == 0 {
			if q != testPerNodeQueue {
				t.Errorf("call %d = %q, want the per-node queue on every iteration (seq %v)", i, q, seq)
			}
			continue
		}
		if q == testPerNodeQueue {
			t.Errorf("call %d = %q, want a fungible queue (seq %v)", i, q, seq)
		}
	}

	// Every fungible queue is still visited exactly once per cycle.
	seen := map[string]int{}
	for _, q := range seq {
		seen[q]++
	}
	for _, q := range queues[1:] {
		if seen[q] != 1 {
			t.Errorf("fungible queue %s polled %d times per cycle, want 1", q, seen[q])
		}
	}
	if seen[testPerNodeQueue] != 3 {
		t.Errorf("per-node queue polled %d times, want 3 (once per iteration)", seen[testPerNodeQueue])
	}

	// The per-node poll uses the short block; the rotating polls share what is
	// left of the configured block timeout, so a full cycle still takes about
	// BlockMs (here 3*1566 + 3*100 = 4998ms).
	wantRotatingMs, wantPrioMs := queueBlockBudget(5000, 3, 1)
	for _, c := range srv.recorded() {
		if c.Queue == testPerNodeQueue {
			if c.BlockMs != wantPrioMs {
				t.Errorf("per-node blockMs = %d, want %d", c.BlockMs, wantPrioMs)
			}
			continue
		}
		if c.BlockMs != wantRotatingMs {
			t.Errorf("fungible blockMs = %d, want %d", c.BlockMs, wantRotatingMs)
		}
	}
}

// TestNextMulti_PerNodeJobPickedUpWithinOneRoundTrip covers the latency claim:
// a job that lands on the per-node queue right after it was polled is picked up
// after at most one intervening fungible poll, not after a full rotation.
func TestNextMulti_PerNodeJobPickedUpWithinOneRoundTrip(t *testing.T) {
	var perNodePolls int
	var mu sync.Mutex
	srv := newFakeConsumeServer(t, func(call consumeCall, _ int) (*redisapi.StreamMessage, int) {
		if call.Queue != testPerNodeQueue {
			return nil, http.StatusOK
		}
		mu.Lock()
		defer mu.Unlock()
		perNodePolls++
		if perNodePolls == 1 {
			// Job is enqueued just after the first visit.
			return nil, http.StatusOK
		}
		return jobMessage("job-pinned"), http.StatusOK
	})

	queues := []string{
		testPerNodeQueue,
		"jobs:v1:tag:gpu:rtx3090",
		"jobs:v1:tag:engine:vllm",
		"jobs:v1:tag:vram:24",
		"jobs:v1:tag:meeting:org_test",
		"jobs:v1:shell:org_test",
	}
	s := newConnectedSource(t, srv, queues, 5000)

	job, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if job == nil {
		t.Fatal("Next returned no job, want the per-node job")
	}
	if job.ID != "job-pinned" {
		t.Errorf("job ID = %q, want job-pinned", job.ID)
	}
	if job.SourceQueue != testPerNodeQueue {
		t.Errorf("SourceQueue = %q, want %q", job.SourceQueue, testPerNodeQueue)
	}

	seq := srv.queueSequence()
	// per-node (empty), one fungible, per-node (job) => 3 calls. Before #704
	// this took 6 calls: one full rotation over every queue.
	if len(seq) != 3 {
		t.Fatalf("consume calls = %d (%v), want 3 (one intervening fungible poll)", len(seq), seq)
	}
	if seq[0] != testPerNodeQueue || seq[2] != testPerNodeQueue {
		t.Errorf("call sequence = %v, want per-node first and third", seq)
	}
	if seq[1] == testPerNodeQueue {
		t.Errorf("call sequence = %v, want a fungible queue in between", seq)
	}
}

// TestNextMulti_NoPerNodeQueueIsUnchanged is the fallback: a queue set with no
// per-node queue behaves exactly as it did before #704.
func TestNextMulti_NoPerNodeQueueIsUnchanged(t *testing.T) {
	srv := newFakeConsumeServer(t, alwaysEmpty)
	queues := []string{
		"jobs:v1:cpu-general",
		"jobs:v1:tag:gpu:rtx3090",
		"jobs:v1:shell:org_test",
	}
	s := newConnectedSource(t, srv, queues, 5000)

	if _, err := s.Next(context.Background()); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}

	seq := srv.queueSequence()
	if strings.Join(seq, ",") != strings.Join(queues, ",") {
		t.Fatalf("call sequence = %v, want a flat round robin %v", seq, queues)
	}
	// Old formula: 5000/3 = 1666ms per queue.
	for _, c := range srv.recorded() {
		if c.BlockMs != 5000/len(queues) {
			t.Errorf("blockMs for %s = %d, want %d", c.Queue, c.BlockMs, 5000/len(queues))
		}
	}

	// The rotation still advances across calls.
	if _, err := s.Next(context.Background()); err != nil {
		t.Fatalf("second Next returned error: %v", err)
	}
	seq = srv.queueSequence()
	if len(seq) != 2*len(queues) {
		t.Fatalf("consume calls after two cycles = %d, want %d", len(seq), 2*len(queues))
	}
}

// TestNextMulti_SingleQueueUnchanged verifies the single-queue path still makes
// one request with the full configured block timeout.
func TestNextMulti_SingleQueueUnchanged(t *testing.T) {
	srv := newFakeConsumeServer(t, alwaysEmpty)
	s := newConnectedSource(t, srv, []string{testPerNodeQueue}, 5000)

	if _, err := s.Next(context.Background()); err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	calls := srv.recorded()
	if len(calls) != 1 {
		t.Fatalf("consume calls = %d, want 1", len(calls))
	}
	if calls[0].BlockMs != 5000 {
		t.Errorf("blockMs = %d, want the full configured 5000", calls[0].BlockMs)
	}
}

// TestNextMulti_ErrorAccounting guards the failure denominator: the per-node
// queue is polled many times per cycle, so counting failed polls rather than
// failed queues would either fake a total outage or hide a real one.
func TestNextMulti_ErrorAccounting(t *testing.T) {
	queues := []string{
		testPerNodeQueue,
		"jobs:v1:tag:gpu:rtx3090",
		"jobs:v1:shell:org_test",
	}

	t.Run("only the per-node queue fails", func(t *testing.T) {
		srv := newFakeConsumeServer(t, func(call consumeCall, _ int) (*redisapi.StreamMessage, int) {
			if call.Queue == testPerNodeQueue {
				return nil, http.StatusForbidden
			}
			return nil, http.StatusOK
		})
		s := newConnectedSource(t, srv, queues, 5000)

		job, err := s.Next(context.Background())
		if err != nil {
			t.Fatalf("Next returned error %v, want nil (other queues are healthy)", err)
		}
		if job != nil {
			t.Fatalf("Next returned job %+v, want nil", job)
		}
	})

	t.Run("only a fungible queue fails", func(t *testing.T) {
		srv := newFakeConsumeServer(t, func(call consumeCall, _ int) (*redisapi.StreamMessage, int) {
			if call.Queue == "jobs:v1:tag:gpu:rtx3090" {
				return nil, http.StatusForbidden
			}
			return nil, http.StatusOK
		})
		s := newConnectedSource(t, srv, queues, 5000)

		if _, err := s.Next(context.Background()); err != nil {
			t.Fatalf("Next returned error %v, want nil (other queues are healthy)", err)
		}
	})

	t.Run("every queue fails", func(t *testing.T) {
		srv := newFakeConsumeServer(t, func(consumeCall, int) (*redisapi.StreamMessage, int) {
			return nil, http.StatusInternalServerError
		})
		s := newConnectedSource(t, srv, queues, 5000)

		if _, err := s.Next(context.Background()); err == nil {
			t.Fatal("Next returned nil error, want an error so the runner backs off")
		}
	})
}

// TestNextMulti_ContextCancellationStopsRotation makes sure a cancelled context
// aborts the interleaved rotation promptly instead of finishing the cycle.
func TestNextMulti_ContextCancellationStopsRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := newFakeConsumeServer(t, func(consumeCall, int) (*redisapi.StreamMessage, int) {
		cancel() // cancel during the very first poll
		return nil, http.StatusOK
	})
	queues := []string{
		testPerNodeQueue,
		"jobs:v1:tag:gpu:rtx3090",
		"jobs:v1:shell:org_test",
	}
	s := newConnectedSource(t, srv, queues, 5000)

	if _, err := s.Next(ctx); err == nil {
		t.Fatal("Next returned nil error after cancellation, want context error")
	}
	if n := len(srv.recorded()); n > 2 {
		t.Errorf("consume calls after cancellation = %d, want the rotation to stop promptly", n)
	}
}
