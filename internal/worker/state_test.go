package worker

import (
	"sync"
	"testing"
	"time"
)

func TestWorkerStateNilSafe(t *testing.T) {
	var s *WorkerState
	// All methods must be no-ops on a nil receiver so callers can pass nil.
	s.SetIdentity("w", "redis", "g", "1", "org")
	s.SetQueues([]string{"a"})
	s.SetPerNodeQueue("q")
	s.RecordPoll()
	s.RecordConsumeStatus(200, "")
	s.RecordJobReceived()
	s.RecordJobDone(true)
	snap := s.Snapshot()
	if snap.WorkerID != "" {
		t.Fatalf("nil snapshot should be zero, got %+v", snap)
	}
}

func TestWorkerStateSnapshot(t *testing.T) {
	s := NewWorkerState()
	s.SetIdentity("worker-1", "redis-api", "citadel-workers", "1008", "org-x")
	s.SetQueues([]string{"jobs:v1:shell:org_x", "jobs:v1:gpu-general"})
	s.SetPerNodeQueue("jobs:v1:shell:org_x:node:1008")
	s.RecordConsumeStatus(200, "")
	s.RecordPoll()
	s.RecordJobReceived()
	s.RecordJobDone(true)
	s.RecordJobReceived()
	s.RecordJobDone(false)

	snap := s.Snapshot()
	if snap.WorkerID != "worker-1" || snap.Source != "redis-api" {
		t.Fatalf("identity not recorded: %+v", snap)
	}
	if snap.ConsumerGroup != "citadel-workers" || snap.HeadscaleNodeID != "1008" || snap.OrgID != "org-x" {
		t.Fatalf("identity fields wrong: %+v", snap)
	}
	if len(snap.Queues) != 2 {
		t.Fatalf("expected 2 queues, got %v", snap.Queues)
	}
	if snap.PerNodeQueue != "jobs:v1:shell:org_x:node:1008" {
		t.Fatalf("per-node queue wrong: %q", snap.PerNodeQueue)
	}
	if snap.LastConsumeStatus != 200 {
		t.Fatalf("expected consume status 200, got %d", snap.LastConsumeStatus)
	}
	if snap.Processed != 1 || snap.Failed != 1 || snap.InFlight != 0 {
		t.Fatalf("counts wrong: processed=%d failed=%d inflight=%d", snap.Processed, snap.Failed, snap.InFlight)
	}
	if !snap.Consuming {
		t.Fatalf("expected Consuming=true right after RecordPoll")
	}
	if snap.LastPollAt == nil || snap.LastJobAt == nil {
		t.Fatalf("expected poll/job timestamps to be set")
	}
}

// TestWorkerStateQueuedExecutingSplit pins the queued-vs-executing distinction
// (citadel-cli#908): RecordJobReceived brackets the wider claimed-to-done span
// (InFlight), RecordJobExecuting/RecordJobExecuteDone bracket only actual
// handler execution. A job claimed and admitted onto a lane but not yet
// executing reads Queued=1, Executing=0; once executing it reads Queued=0,
// Executing=1; InFlight stays 1 throughout.
func TestWorkerStateQueuedExecutingSplit(t *testing.T) {
	s := NewWorkerState()

	// Claimed + admitted, waiting for a lane slot: in-flight but not executing.
	s.RecordJobReceived()
	snap := s.Snapshot()
	if snap.InFlight != 1 || snap.Queued != 1 || snap.Executing != 0 {
		t.Fatalf("after claim: InFlight=%d Queued=%d Executing=%d, want 1/1/0", snap.InFlight, snap.Queued, snap.Executing)
	}
	if snap.OldestExecutingAt != nil {
		t.Error("OldestExecutingAt must be nil while nothing is executing")
	}

	// Now executing.
	s.RecordJobExecuting()
	snap = s.Snapshot()
	if snap.InFlight != 1 || snap.Queued != 0 || snap.Executing != 1 {
		t.Fatalf("while executing: InFlight=%d Queued=%d Executing=%d, want 1/0/1", snap.InFlight, snap.Queued, snap.Executing)
	}
	if snap.OldestExecutingAt == nil {
		t.Error("OldestExecutingAt must be set while a job is executing")
	}

	// Done.
	s.RecordJobExecuteDone()
	s.RecordJobDone(true)
	snap = s.Snapshot()
	if snap.InFlight != 0 || snap.Queued != 0 || snap.Executing != 0 {
		t.Fatalf("after done: InFlight=%d Queued=%d Executing=%d, want 0/0/0", snap.InFlight, snap.Queued, snap.Executing)
	}
	if snap.OldestExecutingAt != nil {
		t.Error("OldestExecutingAt must clear when executing drains to 0")
	}
	if snap.Processed != 1 {
		t.Errorf("Processed = %d, want 1", snap.Processed)
	}
}

func TestWorkerStateConsumingFalseWhenStale(t *testing.T) {
	s := NewWorkerState()
	// Force an old poll time.
	old := time.Now().Add(-time.Hour).UnixNano()
	s.lastPollUnixNano = old
	if s.Snapshot().Consuming {
		t.Fatalf("expected Consuming=false for a stale poll time")
	}
}

func TestWorkerStateConsumeError(t *testing.T) {
	s := NewWorkerState()
	s.RecordConsumeStatus(400, "API error: bad consumer")
	snap := s.Snapshot()
	if snap.LastConsumeStatus != 400 {
		t.Fatalf("expected 400, got %d", snap.LastConsumeStatus)
	}
	if snap.LastConsumeError != "API error: bad consumer" {
		t.Fatalf("expected consume error recorded, got %q", snap.LastConsumeError)
	}
	// status<=0 should not clobber the recorded status.
	s.RecordConsumeStatus(0, "")
	if s.Snapshot().LastConsumeStatus != 400 {
		t.Fatalf("status 0 should not overwrite, got %d", s.Snapshot().LastConsumeStatus)
	}
}

func TestWorkerStateConcurrent(t *testing.T) {
	s := NewWorkerState()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RecordPoll()
			s.RecordJobReceived()
			s.RecordConsumeStatus(200, "")
			s.SetQueues([]string{"a", "b"})
			s.RecordJobDone(true)
			_ = s.Snapshot()
		}()
	}
	wg.Wait()
	if got := s.Snapshot().Processed; got != 50 {
		t.Fatalf("expected 50 processed, got %d", got)
	}
}

// TestSnapshotIdentityUnresolved pins the operator-facing half of
// citadel-cli#654. A node that never resolved its Headscale ID declines every
// target_node-addressed job, yet stays green on every other signal (it polls,
// it heartbeats, it serves untargeted work). IdentityUnresolved is the field
// that explains the resulting timeouts, so it must be asserted POSITIVELY --
// HeadscaleNodeID is omitempty, which makes a degraded node indistinguishable
// from an older payload that simply never carried the field.
func TestSnapshotIdentityUnresolved(t *testing.T) {
	unidentified := NewWorkerState()
	unidentified.SetIdentity("w", "redis", "citadel-somehost", "", "org-1")
	if snap := unidentified.Snapshot(); !snap.IdentityUnresolved {
		t.Error("expected IdentityUnresolved=true when the Headscale node ID is empty")
	}

	identified := NewWorkerState()
	identified.SetIdentity("w", "redis", "citadel-node-758", "758", "org-1")
	if snap := identified.Snapshot(); snap.IdentityUnresolved {
		t.Error("expected IdentityUnresolved=false when the Headscale node ID resolved")
	}
}

// TestWorkerState_SnapshotInFlightPairingIsConsistent is the citadel-cli#896
// regression test for the Snapshot() torn-read hardening: InFlight/
// OldestInFlightAt and Executing/OldestExecutingAt are each written together
// under inFlightMu/executingMu (RecordJobReceived/RecordJobDone and
// RecordJobExecuting/RecordJobExecuteDone respectively), so a consistent
// Snapshot() must never observe one half of a pair mid-update: a count > 0
// with a nil timestamp, or a count == 0 with a non-nil one. Drives real
// concurrent writers (not a single-goroutine sequence, which can't expose a
// torn read) against a concurrent reader long enough to exercise the window
// the fix closes.
func TestWorkerState_SnapshotInFlightPairingIsConsistent(t *testing.T) {
	s := NewWorkerState()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.RecordJobReceived()
				s.RecordJobExecuting()
				s.RecordJobExecuteDone()
				s.RecordJobDone(true)
			}
		}()
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		if snap.InFlight > 0 && snap.OldestInFlightAt == nil {
			close(stop)
			wg.Wait()
			t.Fatalf("torn read: InFlight=%d but OldestInFlightAt is nil", snap.InFlight)
		}
		if snap.InFlight == 0 && snap.OldestInFlightAt != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("torn read: InFlight=0 but OldestInFlightAt=%v", *snap.OldestInFlightAt)
		}
		if snap.Executing > 0 && snap.OldestExecutingAt == nil {
			close(stop)
			wg.Wait()
			t.Fatalf("torn read: Executing=%d but OldestExecutingAt is nil", snap.Executing)
		}
		if snap.Executing == 0 && snap.OldestExecutingAt != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("torn read: Executing=0 but OldestExecutingAt=%v", *snap.OldestExecutingAt)
		}
	}
	close(stop)
	wg.Wait()
}
