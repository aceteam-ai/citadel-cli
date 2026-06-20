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
