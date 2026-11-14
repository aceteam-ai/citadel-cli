package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCobrowseRecoveryPreassignedIDAndDuplicateRefusal(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	id := "cb-c8409b9afcfb4677b9f2813bc0110b91"
	// Empty URL avoids native/CDP navigation settling; the launcher is fake.
	st, err := m.StartSessionWithID(id, "", nil)
	if err != nil || st.ID != id {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err = m.StartSessionWithID(id, "", nil); err == nil {
		t.Fatal("duplicate session replaced")
	}
	if f.n != 1 || filepath.Base(f.profiles[0]) != id {
		t.Fatal("preassigned identity lost")
	}
	for _, bad := range []string{"../outside", "cb-xyz", "cb-C8409B9AFCFB4677B9F2813BC0110B91"} {
		if _, err = m.StartSessionWithID(bad, "", nil); err == nil {
			t.Fatal("unsafe handle accepted")
		}
	}
	if f.n != 1 {
		t.Fatal("invalid ID launched")
	}
	_ = m.Stop(id)
}
func TestCobrowseRecoveryExitEvidenceAndSingleStop(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	id := "cb-owned"
	exited := make(chan struct{})
	stops := 0
	profile := filepath.Join(m.baseDir, id)
	os.MkdirAll(profile, 0700)
	m.sessions[id] = &cobrowseSession{id: id, profile: profile, state: SessionRunning, proc: &cobrowseProc{exited: exited, stop: func() error { stops++; close(exited); return nil }}}
	if m.RecoveryState(id) != "running" {
		t.Fatal("live session missing")
	}
	confirmed, err := m.StopRecoverySession(id)
	if err != nil || !confirmed || stops != 1 {
		t.Fatalf("confirmed=%v err=%v calls=%d", confirmed, err, stops)
	}
	if m.RecoveryState(id) != "unknown" {
		t.Fatal("absence falsely terminal")
	}
	if yes, err := m.StopRecoverySession("missing"); err != nil || yes {
		t.Fatal("missing session falsely stopped")
	}
}
func TestCobrowseRecoveryExitedAndLaunchingEvidence(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	closed := make(chan struct{})
	close(closed)
	m.sessions["ended"] = &cobrowseSession{state: SessionRunning, proc: &cobrowseProc{exited: closed}}
	m.sessions["launching"] = &cobrowseSession{state: SessionLaunching}
	if m.RecoveryState("ended") != "ended" || m.RecoveryState("launching") != "unknown" {
		t.Fatal("invalid lifecycle evidence")
	}
	if confirmed, err := m.StopRecoverySession("launching"); err != nil || confirmed {
		t.Fatal("in-flight launch falsely stopped")
	}
}

func TestCobrowseRecoveryFailedStopRetainsProcessForRetry(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	exited := make(chan struct{})
	calls := 0
	m.sessions["retry"] = &cobrowseSession{id: "retry", state: SessionRunning, proc: &cobrowseProc{exited: exited, stop: func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("not stopped")
		}
		close(exited)
		return nil
	}}}
	if confirmed, err := m.StopRecoverySession("retry"); err == nil || confirmed {
		t.Fatal("failed stop confirmed")
	}
	if m.RecoveryState("retry") != "running" {
		t.Fatal("failed stop lost process custody")
	}
	if confirmed, err := m.StopRecoverySession("retry"); err != nil || !confirmed || calls != 2 {
		t.Fatalf("retry: %v %v %d", confirmed, err, calls)
	}
}

func TestCobrowseRecoveryStopNeedsObservedExit(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	exited := make(chan struct{})
	calls := 0
	m.sessions["wait"] = &cobrowseSession{id: "wait", state: SessionRunning, proc: &cobrowseProc{exited: exited, stop: func() error { calls++; return nil }}}
	if confirmed, err := m.StopRecoverySession("wait"); err == nil || confirmed {
		t.Fatal("stop return without exit falsely confirmed")
	}
	if m.RecoveryState("wait") != "running" {
		t.Fatal("lost unconfirmed process")
	}
	close(exited)
	if confirmed, err := m.StopRecoverySession("wait"); err != nil || !confirmed || calls != 2 {
		t.Fatalf("late exit not recoverable: %v %v %d", confirmed, err, calls)
	}
}
