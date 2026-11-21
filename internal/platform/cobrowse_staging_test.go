package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func startFakeSession(t *testing.T) (*CobrowseSessionManager, CobrowseSessionStatus) {
	t.Helper()
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 4)
	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	return m, st
}

func TestValidateStagedName(t *testing.T) {
	good := []string{"filing.pdf", "a", "My Doc (1).pdf", "report.final.docx", "naïve.txt", "CONSOLE.txt", "COM10.txt"}
	for _, n := range good {
		if err := ValidateStagedName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	bad := []string{
		"", ".", "..", ".hidden", "..x", "a/b", `a\b`, "/etc/passwd", `C:\x`, "C:x", "a\x00b", "a\nb",
		"CON", "con.txt", "NUL", "aux.pdf", "COM1", "lpt9.txt", "com3.log", "x.", "x ", "a:b", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b",
		strings.Repeat("a", 129), "bad\xff.txt",
	}
	for _, n := range bad {
		if err := ValidateStagedName(n); !errors.Is(err, ErrStagedNameInvalid) {
			t.Errorf("%q should be refused, got %v", n, err)
		}
	}
}

func TestStageFileWritesPrivateCreateOnly(t *testing.T) {
	m, st := startFakeSession(t)
	data := []byte("%PDF-1.4 synthetic")
	got, err := m.StageFile(st.ID, "filing.pdf", data)
	if err != nil {
		t.Fatalf("StageFile: %v", err)
	}
	sum := sha256.Sum256(data)
	if got.Size != int64(len(data)) || got.SHA256 != hex.EncodeToString(sum[:]) || got.Name != "filing.pdf" {
		t.Fatalf("result %+v", got)
	}
	path := filepath.Join(st.Profile, cobrowseStagingDirName, "filing.pdf")
	disk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(disk, data) {
		t.Fatalf("staged bytes differ: %v", err)
	}
	if runtime.GOOS != "windows" {
		di, _ := os.Stat(filepath.Dir(path))
		fi, _ := os.Stat(path)
		if di.Mode().Perm() != 0o700 || fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("modes dir=%v file=%v", di.Mode().Perm(), fi.Mode().Perm())
		}
	}
	// Create-only: the same name is refused and the first copy is untouched.
	if _, err := m.StageFile(st.ID, "filing.pdf", []byte("other")); !errors.Is(err, ErrStagedPathRefused) {
		t.Fatalf("second stage of the same name: %v", err)
	}
	if disk, _ = os.ReadFile(path); !bytes.Equal(disk, data) {
		t.Fatal("create-only write overwrote the first file")
	}
}

func TestStageFileRefusals(t *testing.T) {
	m, st := startFakeSession(t)
	if _, err := m.StageFile(st.ID, "../escape.txt", []byte("x")); !errors.Is(err, ErrStagedNameInvalid) {
		t.Fatalf("traversal name: %v", err)
	}
	if _, err := m.StageFile(st.ID, "empty.txt", nil); err == nil {
		t.Fatal("empty file must be refused")
	}
	if _, err := m.StageFile(st.ID, "big.bin", make([]byte, MaxCobrowseStagedFileBytes+1)); !errors.Is(err, ErrStagedTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	if _, err := m.StageFile("no-such", "a.txt", []byte("x")); err == nil {
		t.Fatal("unknown session must be refused")
	}
	for i := 0; i < MaxCobrowseStagedFiles; i++ {
		if _, err := m.StageFile(st.ID, "f"+string(rune('a'+i))+".txt", []byte("x")); err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
	}
	if _, err := m.StageFile(st.ID, "one-too-many.txt", []byte("x")); err == nil {
		t.Fatal("file count cap not enforced")
	}
}

func TestStageFileDoesNotFollowSymlinkLeaf(t *testing.T) {
	m, st := startFakeSession(t)
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(st.Profile, cobrowseStagingDirName, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := m.StageFile(st.ID, "link.txt", []byte("pwned")); err == nil {
		t.Fatal("staging over a symlink must be refused")
	}
	if b, _ := os.ReadFile(outside); string(b) != "victim" {
		t.Fatal("the symlink target was written through")
	}
}

func TestResolveStagedFileRefusals(t *testing.T) {
	m, st := startFakeSession(t)
	s, err := m.sessionForAction(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StageFile(st.ID, "ok.pdf", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	path, file, err := s.resolveStagedFile("ok.pdf")
	if err != nil || file.Size != 3 || !filepath.IsAbs(path) {
		t.Fatalf("resolve ok: %q %+v %v", path, file, err)
	}
	for _, name := range []string{"../ok.pdf", "..", ".", "", "sub/ok.pdf", filepath.Join(st.Profile, cobrowseStagingDirName, "ok.pdf"), "missing.pdf", ".citadel-stage"} {
		if _, _, err := s.resolveStagedFile(name); err == nil {
			t.Errorf("%q must be refused", name)
		}
	}
	// A directory of the right name is not a regular file.
	if err := os.Mkdir(filepath.Join(st.Profile, cobrowseStagingDirName, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.resolveStagedFile("adir"); !errors.Is(err, ErrStagedPathRefused) {
		t.Errorf("directory: %v", err)
	}
	// A symlink to a real file outside staging, and one to a file inside it.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(st.Profile, cobrowseStagingDirName)
	if err := os.Symlink(outside, filepath.Join(stage, "out.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(stage, "ok.pdf"), filepath.Join(stage, "in.pdf")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"out.txt", "in.pdf"} {
		if _, _, err := s.resolveStagedFile(name); !errors.Is(err, ErrStagedPathRefused) {
			t.Errorf("symlink %s: %v", name, err)
		}
	}
}

func TestResolveStagedFileRefusesSymlinkedStagingDir(t *testing.T) {
	m, st := startFakeSession(t)
	s, _ := m.sessionForAction(st.ID)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(st.Profile, cobrowseStagingDirName)
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, stage); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := s.resolveStagedFile("x.txt"); err == nil {
		t.Fatal("a symlinked staging dir must be refused")
	}
}

func TestSetFileInputArgumentAndSessionRefusals(t *testing.T) {
	m, st := startFakeSession(t)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetFileInput(st.ID, "", 0, "a.pdf", "http://example.test/"); err == nil {
		t.Error("neither selector nor node must be refused")
	}
	if _, err := m.SetFileInput(st.ID, "#f", 5, "a.pdf", "http://example.test/"); err == nil {
		t.Error("both selector and node must be refused")
	}
	// Handed off to the human: refused before any CDP call, like every action.
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetFileInput(st.ID, "#f", 0, "a.pdf", "http://example.test/"); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("handed off: %v", err)
	}
	if _, err := m.SetFileInput("no-such", "#f", 0, "a.pdf", "http://example.test/"); err == nil {
		t.Error("unknown session must be refused")
	}
}

func TestStagingDirRemovedOnStop(t *testing.T) {
	m, st := startFakeSession(t)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(st.Profile, cobrowseStagingDirName, "a.pdf")); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(st.Profile, cobrowseStagingDirName), st.Profile} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived Stop: %v", p, err)
		}
	}
}

func TestStagingDirRemovedOnStopAll(t *testing.T) {
	m, st := startFakeSession(t)
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	_ = m.StopAll()
	if _, err := os.Stat(filepath.Join(st.Profile, cobrowseStagingDirName)); !os.IsNotExist(err) {
		t.Fatalf("staging dir survived StopAll: %v", err)
	}
}

func TestStagingDirCreatedAtStartAndGoneAfterFailedStart(t *testing.T) {
	_, st := startFakeSession(t)
	if fi, err := os.Stat(filepath.Join(st.Profile, cobrowseStagingDirName)); err != nil || !fi.IsDir() {
		t.Fatalf("staging dir not created at start: %v", err)
	}
	prev := launchCobrowseProc
	launchCobrowseProc = func(string, string, LaunchOptions) (*cobrowseProc, error) {
		return nil, errors.New("browser did not start")
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
	base := trustedBaseDir(t)
	m2 := newCobrowseSessionManager(base, 2)
	if _, err := m2.StartSession(""); err == nil {
		t.Fatal("start should fail")
	}
	ents, _ := os.ReadDir(base)
	if len(ents) != 0 {
		t.Fatalf("failed start left %d entries (staging dir included)", len(ents))
	}
}

func TestStagingDirRemovedByOrphanSweep(t *testing.T) {
	base := trustedBaseDir(t)
	orphan := filepath.Join(base, "cb-orphan")
	stage := filepath.Join(orphan, cobrowseStagingDirName)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "a.pdf"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSessionPidfile(orphan, 424241, 424242, 424243, 0)
	prevKiller := pidKiller
	pidKiller = func(int) error { return nil }
	t.Cleanup(func() { pidKiller = prevKiller })
	installMatchingCmdlineReader(t)
	newCobrowseSessionManager(base, 2).sweep()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan session dir (with staged files) survived the sweep: %v", err)
	}
}

// A persistent profile is sealed from its working dir: staged files must not
// enter it.
type recordingProfile struct{ sawStaging *bool }

func (recordingProfile) Materialize(string) error { return nil }
func (r recordingProfile) Persist(dir string) error {
	_, err := os.Stat(filepath.Join(dir, cobrowseStagingDirName))
	*r.sawStaging = err == nil
	return nil
}
func (recordingProfile) Close() error { return nil }

func TestStagedFilesNeverReachPersistentProfileSeal(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	saw := true
	st, err := m.StartSessionWithProfile("", recordingProfile{&saw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StageFile(st.ID, "a.pdf", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatal(err)
	}
	if saw {
		t.Fatal("the staging dir was still present when the profile was sealed")
	}
}

func TestChooserEventsRecordAndClear(t *testing.T) {
	s := &cobrowseSession{id: "s", state: SessionRunning}
	s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(42), "mode": "selectMultiple"})
	st, _ := s.statusBase()
	if st.FileChooser == nil || st.FileChooser.BackendNodeID != 42 || !st.FileChooser.Multiple || st.FileChooser.OpenedAt == "" {
		t.Fatalf("pending chooser = %+v", st.FileChooser)
	}
	s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(7), "mode": "selectSingle"})
	if st, _ = s.statusBase(); st.FileChooser.BackendNodeID != 7 || st.FileChooser.Multiple {
		t.Fatalf("latest chooser should win: %+v", st.FileChooser)
	}
	// Garbage never records.
	s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"mode": "selectSingle"})
	// Filling a different node leaves it pending; filling that node clears it.
	s.clearChooserFor(99)
	if st, _ = s.statusBase(); st.FileChooser == nil {
		t.Fatal("a different node must not clear the chooser")
	}
	s.clearChooserFor(7)
	if st, _ = s.statusBase(); st.FileChooser != nil {
		t.Fatalf("set_file_input on the node should clear it: %+v", st.FileChooser)
	}
	// A navigation clears it; a subframe navigation does not.
	s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(8), "mode": "selectSingle"})
	s.handleChooserEvent("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "x", "parentId": "main"}})
	if st, _ = s.statusBase(); st.FileChooser == nil {
		t.Fatal("a subframe navigation must not clear the chooser")
	}
	s.handleChooserEvent("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main"}})
	if st, _ = s.statusBase(); st.FileChooser != nil {
		t.Fatalf("main-frame navigation should clear it: %+v", st.FileChooser)
	}
}

func TestActionEffectsFilesSetIsAnEffect(t *testing.T) {
	if !(ActionEffects{FilesSet: 1}).anyEffect() {
		t.Fatal("a set file is an effect")
	}
	e := &ActionIncompleteError{Action: "set_file_input", Effects: ActionEffects{Dispatched: true, FilesSet: 1}}
	if !strings.Contains(e.Error(), "set 1 file") {
		t.Fatalf("error text: %s", e.Error())
	}
}
