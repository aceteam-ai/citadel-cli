package declaredsession

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHolderRecordWaitDoesNotChangeManagerDeadline(t *testing.T) {
	manager := &SessionStore{}
	holderSession := &SessionStore{recordWait: holderRecordWait}
	if manager.recordLockWait() != 5*time.Second || holderSession.recordLockWait() != 50*time.Millisecond {
		t.Fatal("holder budget changed manager deadline or exceeds one flush interval")
	}
}

func TestRunHolderKeepsStartupRecordBudgetUntilPublication(t *testing.T) {
	raw, err := os.ReadFile("holder.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "func RunHolder(")
	if start < 0 {
		t.Fatal("cannot locate production holder startup")
	}
	end := strings.Index(source[start:], "\nfunc ")
	if end < 0 {
		t.Fatal("cannot locate production holder startup")
	}
	source = source[start : start+end]
	write := strings.Index(source, "if session.WriteState(state) != nil {")
	budget := strings.Index(source, "session.recordWait = holderRecordWait")
	listen := strings.Index(source, "listener, err := listenHolder(session)")
	if write < 0 || budget <= write || listen <= budget || strings.Count(source, "session.recordWait = holderRecordWait") != 1 {
		t.Fatal("startup must publish with the ordinary deadline before selecting the actor budget and publishing the handle")
	}
}

func TestDueSpawnFailureReportsDistinctQuarantineWithoutChild(t *testing.T) {
	h, records := holderFixture()
	h.nextSpawn = time.Unix(1, 0)
	spawns := 0
	h.spawn = func(Declaration, []string, []string, uint64) (holderChild, error) {
		spawns++
		return nil, ErrUnavailable
	}
	h.startDueChild(time.Unix(2, 0))
	if !h.quarantined || h.child != nil || h.state.Phase != "spawn_failed" || len(records.states) != 1 || records.states[0].Phase != "spawn_failed" {
		t.Fatal("no-child respawn failure misreported as unknown child cleanup")
	}
	h.startDueChild(time.Unix(3, 0))
	if spawns != 1 || !errors.Is(h.finalError(), ErrUnavailable) {
		t.Fatal("failed respawn quarantine authorized another child")
	}
	assertQuarantinedStopRefused(t, h)
}

func TestEventQueueOverflowReportsDistinctQuarantineAndRetainsPending(t *testing.T) {
	h, _ := holderFixture()
	h.child = newFakeChild()
	h.controller, h.epoch = "controller", 4
	h.consolePending = []byte("exact pending console")
	h.markerPending = &StopMarker{Version: 1, Name: h.state.Name, Incarnation: h.state.Incarnation, DeclarationRevision: h.state.DeclarationRevision, StoppedAt: time.Unix(1, 0)}
	h.eventsPending = make([]Event, maxPendingEvents)
	for i := range h.eventsPending {
		h.eventsPending[i] = Event{Kind: "attach", At: time.Unix(int64(i+1), 0), Incarnation: h.state.Incarnation}
	}
	pending := append([]Event(nil), h.eventsPending...)
	marker := *h.markerPending
	h.event("detach", "viewer_detached", "overflowed", "readonly")
	if !h.quarantined || h.state.Phase != "event_queue_overflow" || !h.statePending || h.controller != "" || !h.controlPending || h.epoch != 5 {
		t.Fatal("overflow did not revoke controller and preserve distinct quarantine state")
	}
	if !reflect.DeepEqual(h.eventsPending, pending) || string(h.consolePending) != "exact pending console" || h.markerPending == nil || *h.markerPending != marker || len(h.child.(*fakeHolderChild).calls) != 0 {
		t.Fatal("overflow discarded or replaced accepted pending records")
	}
	assertQuarantinedStopRefused(t, h)
}

func assertQuarantinedStopRefused(t *testing.T, h *holder) {
	t.Helper()
	reply := make(chan holderReply, 1)
	request := frame("stop")
	request.Incarnation = h.state.Incarnation
	h.request(holderRequest{frame: request, reply: reply})
	if result := <-reply; !errors.Is(result.err, ErrConflict) || result.frame.Op == "ok" || len(h.stopReplies) != 0 {
		t.Fatal("quarantined holder promised a graceful stop acknowledgment")
	}
}

func TestDistinctHolderFailurePhasesAreValidStoredRecords(t *testing.T) {
	h, _ := holderFixture()
	session := &SessionStore{paths: Paths{Name: h.state.Name}}
	for _, phase := range []string{"spawn_failed", "event_queue_overflow", "cleanup_unconfirmed"} {
		state := h.state
		state.Phase = phase
		if err := session.validateState(state); err != nil {
			t.Fatal("distinct holder failure phase rejected by storage", phase, err)
		}
	}
}
