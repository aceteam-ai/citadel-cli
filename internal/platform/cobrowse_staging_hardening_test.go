package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestValidateStagedNameDeviceAndFormatNames(t *testing.T) {
	for _, n := range []string{"COM0", "LPT0", "com0.txt", "COM\u00b9", "LPT\u00b2.pdf", "COM\u00b3", "a\u202eb.pdf", "a\u200bb", "\ufeffname"} {
		if err := ValidateStagedName(n); !errors.Is(err, ErrStagedNameInvalid) {
			t.Errorf("%q should be refused, got %v", n, err)
		}
	}
	for _, n := range []string{"COM", "COMX", "LPT10", "COM1x.txt"} {
		if err := ValidateStagedName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
}

// The symlink refusal must come from the plain-directory check, not from the
// permission bits a symlink happens to report. The target here is a real 0700
// directory, so only the symlink check can refuse it.
func TestStagingDirSymlinkToPrivateDirRefusedByTypeCheck(t *testing.T) {
	m, st := startFakeSession(t)
	s, _ := m.sessionForAction(st.ID)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(st.Profile, cobrowseStagingDirName)
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, stage); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := s.ensureStagingDir()
	if !errors.Is(err, ErrStagedPathRefused) || !strings.Contains(err.Error(), "not a plain directory") {
		t.Fatalf("got %v, want the plain-directory refusal", err)
	}
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err == nil {
		t.Fatal("staging through a symlinked dir must be refused")
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Fatal("a file was written through the symlink")
	}
}

// A stage racing teardown never writes after teardown, and never recreates
// the removed session dir.
func TestStageFileRacingTeardownNeverRecreatesDirs(t *testing.T) {
	for i := 0; i < 25; i++ {
		m, st := startFakeSession(t)
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for k := 0; k < 8; k++ {
					_, _ = m.StageFile(st.ID, "f"+string(rune('a'+g))+string(rune('a'+k))+".txt", []byte("x"))
				}
			}(g)
		}
		_ = m.Stop(st.ID)
		wg.Wait()
		if _, err := os.Stat(st.Profile); !os.IsNotExist(err) {
			t.Fatalf("iteration %d: session dir exists after Stop and racing stages: %v", i, err)
		}
	}
}

func TestStageFileRefusedOnceStagingClosed(t *testing.T) {
	m, st := startFakeSession(t)
	s, _ := m.sessionForAction(st.ID)
	s.closeStaging()
	if _, err := m.StageFile(st.ID, "a.txt", []byte("x")); err == nil {
		t.Fatal("stage after close must be refused")
	}
	if _, err := os.Stat(filepath.Join(st.Profile, "a.txt")); err == nil {
		t.Fatal("nothing may be written")
	}
}

func TestStageFileRefusesWhenDiskNearlyFull(t *testing.T) {
	m, st := startFakeSession(t)
	prev := availableStageBytes
	availableStageBytes = func(string) (uint64, error) { return minFreeAfterStageBytes + 10, nil }
	t.Cleanup(func() { availableStageBytes = prev })
	if _, err := m.StageFile(st.ID, "ok.txt", []byte("0123456789")); err != nil {
		t.Fatalf("exactly at the floor should pass: %v", err)
	}
	if _, err := m.StageFile(st.ID, "big.txt", []byte("0123456789a")); err == nil {
		t.Fatal("a write that leaves less than the floor must be refused")
	}
	if _, err := os.Stat(filepath.Join(st.Profile, cobrowseStagingDirName, "big.txt")); err == nil {
		t.Fatal("a refused write must leave nothing behind")
	}
	// Unknown free space does not block.
	availableStageBytes = func(string) (uint64, error) { return 0, errors.New("unknown") }
	if _, err := m.StageFile(st.ID, "unk.txt", []byte("x")); err != nil {
		t.Fatalf("unreadable free space should fail open: %v", err)
	}
}

func TestStagingErrorsNameNoPaths(t *testing.T) {
	m, st := startFakeSession(t)
	s, _ := m.sessionForAction(st.ID)
	for _, name := range []string{"missing.pdf"} {
		_, _, err := s.resolveStagedFile(name)
		if err == nil || strings.Contains(err.Error(), st.Profile) {
			t.Fatalf("error must not carry the session path: %v", err)
		}
	}
}

func TestSetFileInputRefusesNodeThatIsNotThePendingChooser(t *testing.T) {
	m, st := startFakeSession(t)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetFileInput(st.ID, "", 42, "a.pdf"); err == nil || !strings.Contains(err.Error(), "pending file chooser") {
		t.Fatalf("no chooser pending: %v", err)
	}
	s, _ := m.sessionForAction(st.ID)
	s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(7), "mode": "selectSingle", "frameId": "F1"})
	if _, err := m.SetFileInput(st.ID, "", 42, "a.pdf"); err == nil || !strings.Contains(err.Error(), "pending file chooser") {
		t.Fatalf("a different node must be refused: %v", err)
	}
	st2, _ := m.SessionStatus(st.ID)
	if st2.FileChooser == nil || st2.FileChooser.FrameID != "F1" {
		t.Fatalf("status chooser = %+v", st2.FileChooser)
	}
}

// Start after any stop is a no-op, so a stop during launch cannot be followed
// by a watcher that redials a dead port.
func TestChooserWatchNeverStartsAfterStop(t *testing.T) {
	s := &cobrowseSession{id: "s"}
	s.stopChooserWatch()
	s.startChooserWatch(12345)
	s.mu.Lock()
	started := s.watchStop != nil
	s.mu.Unlock()
	if started {
		t.Fatal("watcher started after stop")
	}
}

func TestChooserWatchStopsWhenBrowserGone(t *testing.T) {
	s := &cobrowseSession{id: "s"}
	if s.browserAlive(9000) {
		t.Fatal("no process means not alive")
	}
	exited := make(chan struct{})
	s.proc = &cobrowseProc{debugPort: 9000, exited: exited}
	if !s.browserAlive(9000) || s.browserAlive(9001) {
		t.Fatal("alive only for its own port")
	}
	close(exited)
	if s.browserAlive(9000) {
		t.Fatal("an exited browser is not alive")
	}
}
