package platform

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// This composes the durable components, not the production manager. Runtime
// effects are value-only fakes; no browser, display, credential or timer runs.
func TestSuspendRepeatedThrowawayCyclesAcrossStoreRestarts(t *testing.T) {
	s, dir, live, initialCapture := suspendIntentFixture(t)
	initialCapture.clear()
	t.Cleanup(func() { _ = s.close() })
	id, firstGeneration := live.ID, live.Generation
	restart := func(want sessionRecord) {
		t.Helper()
		if err := s.close(); err != nil {
			t.Fatal(err)
		}
		next, err := openSuspendIntentStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		s = next
		got, err := s.read(id)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("restart changed the durable lifecycle checkpoint", err)
		}
	}
	fence := func() error { return nil }
	var priorIntent, priorSuspended, priorResuming sessionRecord
	for cycle := 0; cycle < 3; cycle++ {
		now := suspendTestNow.Add(time.Duration(cycle) * time.Hour)
		manifest := suspendTestManifest()
		manifest.SessionID, manifest.Generation = id, live.Generation
		manifest.Tabs[0].URL = fmt.Sprintf("https://example.invalid/cycle/%d", cycle)
		manifest.Tabs[0].ScrollX, manifest.Tabs[0].ScrollY = float64(cycle*17), float64(cycle*29)
		capture, err := prepareSuspendCapture(manifest, false, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, now, fence)
		capture.clear()
		if err != nil || intent.Generation != firstGeneration+uint64(cycle) {
			t.Fatal("fresh suspend did not retain its live generation", err)
		}
		restart(intent)
		if cycle > 0 {
			// The previous live completion deliberately retained its old
			// payload. GC must remove that orphan, not this same-ID capture.
			old, err := s.payloads.read(priorResuming, nil)
			if err != nil || old.Generation != priorResuming.Suspension.CaptureGeneration {
				t.Fatal("old capture disappeared before bounded cleanup", err)
			}
			// Exercise stale retries while their payload still exists, so a
			// missing payload cannot accidentally supply the refusal.
			stops := 0
			if _, err := s.completeSuspending(suspendPagesContext(t), priorIntent, now,
				func(context.Context, suspendStopIdentity) error { stops++; return nil }); err == nil || stops != 0 {
				t.Fatal("stale prior-cycle completion crossed the runtime boundary")
			}
			plan, err := planReopenRecord(priorSuspended, priorSuspended.OwnerUserID, false, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.stageResuming(suspendPagesContext(t), priorSuspended, plan, now, fence); err == nil {
				t.Fatal("stale prior-cycle reopen replaced the fresh capture")
			}
			current, err := s.read(id)
			if err != nil || !reflect.DeepEqual(current, intent) {
				t.Fatal("refused stale retries changed the fresh intent", err)
			}
			result, err := s.sweepOrphanPayloads(suspendPagesContext(t), "")
			if err != nil || result.Removed != 1 || result.Failed != 0 {
				t.Fatal("same-ID recapture did not release exactly the previous generation", err)
			}
			if _, err := s.payloads.read(priorResuming, nil); err == nil {
				t.Fatal("old capture remained after acknowledged cleanup")
			}
		}
		read, err := s.payloads.read(intent, nil)
		if err != nil || !reflect.DeepEqual(read, manifest) {
			t.Fatal("restart or orphan cleanup changed current tabs or coordinates", err)
		}
		stops := 0
		suspended, err := s.completeSuspending(suspendPagesContext(t), intent, now.Add(time.Second),
			func(ctx context.Context, identity suspendStopIdentity) error {
				stops++
				if ctx.Err() != nil || identity != suspendIdentity(intent) {
					t.Error("fake teardown received a different runtime incarnation")
				}
				return nil
			})
		if err != nil || stops != 1 || suspended.State != RecordSuspended || suspended.ID != id ||
			suspended.Suspension.CaptureGeneration != live.Generation ||
			suspended.Suspension.ExpiresAt != now.Add(throwawaySuspendTTL).UTC().Format(time.RFC3339Nano) {
			t.Fatal("suspend completion reused a prior capture or TTL", err)
		}
		restart(suspended)
		plan, err := planReopenRecord(suspended, suspended.OwnerUserID, false, now.Add(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		resuming, err := s.stageResuming(suspendPagesContext(t), suspended, plan, now.Add(2*time.Second), fence)
		if err != nil || resuming.Generation != live.Generation+1 || !reflect.DeepEqual(resuming.Suspension, suspended.Suspension) {
			t.Fatal("reopen reused a generation or refreshed immutable retention", err)
		}
		restart(resuming)
		retained, err := s.sweepOrphanPayloads(suspendPagesContext(t), "")
		if err != nil || retained.Removed != 0 || retained.Failed != 0 {
			t.Fatal("advanced live generation made its immutable capture look orphaned", err)
		}
		read, err = s.payloads.read(resuming, nil)
		if err != nil || !reflect.DeepEqual(read, manifest) {
			t.Fatal("reopen looked up its live generation instead of capture generation", err)
		}
		proposal := resuming
		proposal.State, proposal.Suspension = RecordLive, nil
		proposal.StartedAt = now.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
		proposal.Profile, proposal.Browser, proposal.BrowserID = "/synthetic/reopened", "chrome", fmt.Sprintf("browser-cycle-%d", cycle)
		proposal.BrowserPID, proposal.BrowserStart, proposal.DebugPort = 100+cycle, uint64(200+cycle), 9900+cycle
		proofs := 0
		completed, err := s.completeResuming(suspendPagesContext(t), resuming, proposal, now.Add(4*time.Second), fence,
			func(ctx context.Context, identity suspendStopIdentity) error {
				proofs++
				if ctx.Err() != nil || identity != suspendIdentity(proposal) {
					t.Error("fake restoration received a different runtime incarnation")
				}
				return nil
			})
		if err != nil || proofs != 1 || completed.ID != id || completed.Generation != firstGeneration+uint64(cycle)+1 ||
			completed.State != RecordLive || completed.Suspension != nil || completed.Persistent {
			t.Fatal("repeated reopen failed to publish the next same-ID throwaway runtime", err)
		}
		restart(completed)
		priorIntent, priorSuspended, priorResuming = intent, suspended, resuming
		live = completed
	}
	result, err := s.sweepOrphanPayloads(suspendPagesContext(t), "")
	if err != nil || result.Removed != 1 || result.Failed != 0 {
		t.Fatal("final live completion did not release its last capture", err)
	}
	restart(live)
	result, err = s.sweepOrphanPayloads(suspendPagesContext(t), "")
	if err != nil || result.Removed != 0 || result.Failed != 0 {
		t.Fatal("restart repeated acknowledged capture cleanup", err)
	}
}
