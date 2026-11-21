//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// If the staging dir cannot be removed, teardown must not seal the profile
// and must say so.
func TestUnremovableStagingDirIsNeverSealed(t *testing.T) {
	prevA, prevD := sessionRemoveAttempts, sessionRemoveDelay
	sessionRemoveAttempts, sessionRemoveDelay = 1, time.Millisecond
	t.Cleanup(func() { sessionRemoveAttempts, sessionRemoveDelay = prevA, prevD })
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	saw := false
	persisted := &persistCounter{saw: &saw}
	st, err := m.StartSessionWithProfile("", persisted)
	if err != nil {
		t.Fatal(err)
	}
	pinDirUnremovable(t, filepath.Join(st.Profile, cobrowseStagingDirName))
	err = m.Stop(st.ID)
	if err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("Stop = %v, want the staging removal failure", err)
	}
	if saw {
		t.Fatal("the profile was sealed while staged files could not be removed")
	}
}

type persistCounter struct{ saw *bool }

func (persistCounter) Materialize(string) error { return nil }
func (p persistCounter) Persist(string) error   { *p.saw = true; return nil }
func (persistCounter) Close() error             { return nil }

// An unreadable staging dir refuses the stage: the file count fails closed.
func TestStageFileCountFailsClosedWhenDirUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	m, st := startFakeSession(t)
	stage := filepath.Join(st.Profile, cobrowseStagingDirName)
	if err := os.Chmod(stage, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stage, 0o700) })
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err == nil {
		t.Fatal("a stage into a dir whose count cannot be read must be refused")
	}
}
