//go:build !windows

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinDirUnremovable makes dir impossible to remove for a non-root user: a
// read-only subdir that still holds a file.
func pinDirUnremovable(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can remove read-only directories")
	}
	sub := filepath.Join(dir, "pinned")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
}

// A decrypted orphan that cannot be removed is reported, not ignored.
func TestCleanOrphanedProfileSessionsReportsRemovalFailure(t *testing.T) {
	prevA, prevD := orphanRemoveAttempts, orphanRemoveDelay
	orphanRemoveAttempts, orphanRemoveDelay = 1, time.Millisecond
	t.Cleanup(func() { orphanRemoveAttempts, orphanRemoveDelay = prevA, prevD })
	base := trustedBaseDir(t)
	const mine = "/store/cobrowse-profiles/default.enc"
	dir := plantOrphan(t, base, "cb-0000000000000005", mine, deadPID, 0)
	pinDirUnremovable(t, dir)
	err := CleanOrphanedProfileSessions(base, func(id string) bool { return id == mine })
	if !errors.Is(err, ErrProfilePlaintextRemains) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("an unremovable orphan must be reported, got %v", err)
	}
}

// A persistent session whose decrypted working dir cannot be removed on stop
// returns that failure instead of dropping it. Mutation check: ignoring
// removeSessionDir's result makes Stop return nil.
func TestPersistentStopReportsWorkingDirRemovalFailure(t *testing.T) {
	prevA, prevD := sessionRemoveAttempts, sessionRemoveDelay
	sessionRemoveAttempts, sessionRemoveDelay = 1, time.Millisecond
	t.Cleanup(func() { sessionRemoveAttempts, sessionRemoveDelay = prevA, prevD })
	l := &sentinelCheckingLauncher{}
	l.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	st, err := m.StartSessionWithProfile("", &fakeProfile{})
	if err != nil {
		t.Fatal(err)
	}
	pinDirUnremovable(t, st.Profile)
	err = m.Stop(st.ID)
	if err == nil || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("Stop = %v, want the removal failure", err)
	}
}

// A present marker that cannot be read is reported, never skipped.
func TestCleanOrphanedProfileSessionsUnreadableMarker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 file")
	}
	base := trustedBaseDir(t)
	const mine = "/store/cobrowse-profiles/default.enc"
	dir := plantOrphan(t, base, "cb-0000000000000006", mine, deadPID, 0)
	marker := filepath.Join(dir, profileMarkerName)
	if err := os.Chmod(marker, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(marker, 0o600) })
	err := CleanOrphanedProfileSessions(base, func(id string) bool { return id == mine })
	if !errors.Is(err, ErrProfilePlaintextRemains) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("an unreadable marker must be reported, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Cookies")); err != nil {
		t.Fatalf("the dir must be left for the user to resolve: %v", err)
	}
}
