package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const localTestPassphrase = "lantern orbit pebble saffron"

// TestLocalPassphraseLifecycle drives the terminal commands' cores against a
// temp dir: a PIN is refused (#820), a passphrase is set, changed, and the
// forget path deletes saved logins only when no owner is running.
func TestLocalPassphraseLifecycle(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runLocalPassphraseStatus(dir, &out); err != nil || !strings.Contains(out.String(), "not set") {
		t.Fatalf("status before set: %q %v", out.String(), err)
	}
	if err := runLocalPassphraseSet(dir, "654321", &out); !errors.Is(err, nodevault.ErrWeakSecret) {
		t.Fatalf("a PIN must be refused: %v", err)
	}
	if err := runLocalPassphraseSet(dir, localTestPassphrase, &out); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := runLocalPassphraseSet(dir, localTestPassphrase, &out); err == nil {
		t.Fatal("a second set must be refused")
	}
	if err := runLocalPassphraseChange(dir, "not the passphrase at all", "another four random words", &out); err == nil ||
		!strings.Contains(err.Error(), "not right") {
		t.Fatalf("change with a wrong current passphrase: %v", err)
	}
	const next = "violet harbor canvas thimble"
	if err := runLocalPassphraseChange(dir, localTestPassphrase, next, &out); err != nil {
		t.Fatalf("change: %v", err)
	}
	h, err := cobrowseprofile.NewStore(dir).Open("default", next)
	if err != nil {
		t.Fatalf("open with the new passphrase: %v", err)
	}
	_ = h.Persist(t.TempDir())
	_ = h.Close()

	owner, err := localcobrowse.AcquireOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runLocalPassphraseForget(dir, &out); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("forget while an owner runs: %v", err)
	}
	owner.Release()
	if err := runLocalPassphraseForget(dir, &out); err != nil {
		t.Fatalf("forget: %v", err)
	}
	out.Reset()
	if err := runLocalPassphraseStatus(dir, &out); err != nil || !strings.Contains(out.String(), "not set") ||
		!strings.Contains(out.String(), "Saved profiles: none") {
		t.Fatalf("status after forget: %q %v", out.String(), err)
	}
}

// TestLocalProfileStoreFriendlyErrors: the viewer sees one plain line, and
// errors.Is still finds the vault error.
func TestLocalProfileStoreFriendlyErrors(t *testing.T) {
	dir := t.TempDir()
	ps := localProfileStore{dir: func() string { return dir }}
	if _, err := ps.Open("default", localTestPassphrase); !errors.Is(err, nodevault.ErrNotConfigured) ||
		strings.Contains(err.Error(), "nodevault") {
		t.Fatalf("open before setup: %v", err)
	}
	if err := ps.SetPassphrase("654321", true); !errors.Is(err, nodevault.ErrWeakSecret) || !strings.Contains(err.Error(), "four or more") {
		t.Fatalf("weak setup: %v", err)
	}
	if err := ps.SetPassphrase(localTestPassphrase, true); err != nil {
		t.Fatal(err)
	}
	_, err := ps.Open("default", "lantern orbit pebble saffrom")
	if !errors.Is(err, nodevault.ErrWrongPIN) || err.Error() != "That passphrase is not right." {
		t.Fatalf("wrong passphrase: %v", err)
	}
	p, err := ps.Open("default", localTestPassphrase)
	if err != nil || p == nil {
		t.Fatalf("open: %v", err)
	}
	_ = p.Close()
}

// TestLocalModeProfileSessionLocked: through the real runtime wiring, a
// profile session starts locked and never launches a browser until unlocked.
func TestLocalModeProfileSessionLocked(t *testing.T) {
	dir := t.TempDir()
	fb := newCmdFakeBrowser()
	rt := newLocalCobrowseRuntime(localCobrowseOptions{
		ownerDir: func() string { return dir },
		browser:  fb,
		policy:   func(context.Context, string) error { return nil },
	})
	defer rt.close()
	res, err := rt.svc.Start(context.Background(), "", "g", "default")
	if err != nil || res.State != localcobrowse.StateLocked {
		t.Fatalf("start = %+v %v", res, err)
	}
	if len(fb.live) != 0 {
		t.Fatal("no browser may start while the profile is locked")
	}
	if err := rt.svc.HumanSetupAndUnlock(context.Background(), res.SessionID, localTestPassphrase, true); err != nil {
		t.Fatalf("setup and unlock: %v", err)
	}
	if !fb.live[res.SessionID] {
		t.Fatal("unlock did not start the browser under the agent's session id")
	}
	// A wrong passphrase on a second profile is audited, and no audit file
	// ever holds either passphrase.
	const wrong = "lantern orbit pebble saffrom"
	res2, _ := rt.svc.Start(context.Background(), "", "g", "other")
	if err := rt.svc.HumanUnlock(context.Background(), res2.SessionID, wrong); err == nil {
		t.Fatal("a wrong passphrase must not unlock")
	}
	var unlockRecords int
	err = filepath.WalkDir(filepath.Join(dir, localaudit.DirName), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for _, secret := range []string{localTestPassphrase, wrong} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("%s holds a passphrase", p)
			}
		}
		unlockRecords += strings.Count(string(data), `"unlock"`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if unlockRecords < 2 {
		t.Fatalf("unlock attempts not audited (%d records)", unlockRecords)
	}
}

// TestSignalDuringCloseWaitsForSeal: a SIGTERM that arrives while the deferred
// close (after stdin EOF) is still sealing sessions must not exit until that
// teardown has finished. Mutation check: a close that returns early for a
// second caller lets exit run while StopAll is still blocked.
func TestSignalDuringCloseWaitsForSeal(t *testing.T) {
	fb := newCmdFakeBrowser()
	fb.stopAllEntered, fb.stopAllGate = make(chan struct{}), make(chan struct{})
	rt := newLocalCobrowseRuntime(localCobrowseOptions{
		ownerDir: func() string { return t.TempDir() },
		browser:  fb,
		policy:   func(context.Context, string) error { return nil },
	})
	if _, err := rt.svc.Start(context.Background(), "", "g", ""); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan struct{})
	go func() { rt.close(); close(firstDone) }()
	<-fb.stopAllEntered

	sigs := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	go closeOnSignal(rt, sigs, func(code int) { exited <- code })
	sigs <- os.Interrupt
	select {
	case <-exited:
		t.Fatal("exited while the first close was still sealing sessions")
	case <-time.After(150 * time.Millisecond):
	}
	close(fb.stopAllGate)
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never exited after close finished")
	}
	fb.mu.Lock()
	done := fb.stopAllDone
	fb.mu.Unlock()
	if !done {
		t.Fatal("exited before every session was stopped")
	}
	<-firstDone
	rt.close() // idempotent
}

// TestSecretReaderPipedSharesOneReaderAndKeepsWhitespace: piped `change`
// reads the current and the new passphrase from one buffered reader (a second
// reader would lose the buffered second line), and neither the CLI nor the
// viewer trims a passphrase, so a value set on one surface unlocks on the other.
func TestSecretReaderPipedSharesOneReaderAndKeepsWhitespace(t *testing.T) {
	in := &secretReader{r: bufio.NewReader(strings.NewReader(" old words here \r\nnew words with space \n")), prompts: io.Discard}
	cur, err := in.read("Current: ")
	if err != nil || cur != " old words here " {
		t.Fatalf("current = %q %v", cur, err)
	}
	next, err := in.readConfirmed("New: ", "Repeat: ")
	if err != nil || next != "new words with space " {
		t.Fatalf("new = %q %v", next, err)
	}
	if _, err := in.read("More: "); err == nil {
		t.Fatal("reading past the input must fail, not return an empty passphrase")
	}
	empty := &secretReader{r: bufio.NewReader(strings.NewReader("\n")), prompts: io.Discard}
	if _, err := empty.read("P: "); err == nil {
		t.Fatal("an empty passphrase must be refused")
	}
}

// TestPassphraseWithSpacesRoundTripsAcrossSurfaces: a passphrase with leading
// and trailing spaces set through the CLI core unlocks through the viewer's
// store adapter unchanged.
func TestPassphraseWithSpacesRoundTripsAcrossSurfaces(t *testing.T) {
	dir := t.TempDir()
	const pass = "  lantern orbit pebble saffron  "
	if err := runLocalPassphraseSet(dir, pass, io.Discard); err != nil {
		t.Fatal(err)
	}
	ps := localProfileStore{dir: func() string { return dir }}
	p, err := ps.Open("default", pass)
	if err != nil {
		t.Fatalf("the viewer path could not unlock a CLI-set passphrase: %v", err)
	}
	_ = p.Close()
	if _, err := ps.Open("default", strings.TrimSpace(pass)); err == nil {
		t.Fatal("a trimmed value must not match: no surface trims")
	}
}

// TestLocalStoreDirsArePrivate: the store creates its folders owner-only.
func TestLocalStoreDirsArePrivate(t *testing.T) {
	dir := t.TempDir()
	if err := runLocalPassphraseSet(dir, localTestPassphrase, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, d := range cobrowseprofile.NewStore(dir).PrivateDirs() {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s missing: %v", d, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is not owner-only: %v", d, info.Mode().Perm())
		}
		if err := platform.EnsurePrivateDir(d); err != nil {
			t.Fatalf("%s does not pass the private-dir check: %v", d, err)
		}
	}
}

// plantSessionDir writes a synthetic co-browse session working dir holding a
// decrypted cookie, marked as belonging to identity and owner.
func plantSessionDir(t *testing.T, base, name, identity string, owner int) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Default", "Cookies"), []byte("synthetic-plaintext-cookie"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf(`{"identity":%q,"owner_pid":%d}`, identity, owner)
	if err := os.WriteFile(filepath.Join(dir, ".citadel-profile"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func forgetFixture(t *testing.T) (dir, base string, s *cobrowseprofile.Store) {
	t.Helper()
	dir, base = t.TempDir(), filepath.Join(t.TempDir(), "sessions")
	if err := platform.EnsurePrivateDir(base); err != nil {
		t.Fatal(err)
	}
	prev := localSessionsBaseDir
	localSessionsBaseDir = func() string { return base }
	t.Cleanup(func() { localSessionsBaseDir = prev })
	if err := runLocalPassphraseSet(dir, localTestPassphrase, io.Discard); err != nil {
		t.Fatal(err)
	}
	s = cobrowseprofile.NewStore(dir)
	h, err := s.Open("default", localTestPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Persist(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	return dir, base, s
}

// TestForgetRemovesInterruptedSessionPlaintext: a session that was killed left
// a decrypted copy of a saved profile; forget removes it before reporting that
// every saved login was deleted. Mutation check: a forget that skips the
// session folder leaves the cookie file and still prints success.
func TestForgetRemovesInterruptedSessionPlaintext(t *testing.T) {
	dir, base, s := forgetFixture(t)
	identity := filepath.Join(dir, "cobrowse-profiles", "default.enc")
	orphan := plantSessionDir(t, base, "cb-0000000000000011", identity, 2147483000)
	other := plantSessionDir(t, base, "cb-0000000000000012", filepath.Join(t.TempDir(), "cobrowse-profiles", "default.enc"), 2147483000)
	var out bytes.Buffer
	if err := runLocalPassphraseForget(dir, &out); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("decrypted profile left on disk after forget: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("another store's session dir must not be touched: %v", err)
	}
	if st, _ := s.State(); st.Configured || !strings.Contains(out.String(), "Deleted every saved login") {
		t.Fatalf("forget did not finish: %+v %q", st, out.String())
	}
}

// TestForgetRefusesWhilePlaintextRemains: a decrypted copy that cannot be
// removed (here: its owner process is still running) makes forget refuse,
// name the folder, and delete nothing.
func TestForgetRefusesWhilePlaintextRemains(t *testing.T) {
	dir, base, s := forgetFixture(t)
	identity := filepath.Join(dir, "cobrowse-profiles", "default.enc")
	live := plantSessionDir(t, base, "cb-0000000000000013", identity, os.Getpid())
	var out bytes.Buffer
	err := runLocalPassphraseForget(dir, &out)
	if err == nil || !strings.Contains(err.Error(), live) || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("forget = %v, want a refusal naming %s", err, live)
	}
	if strings.Contains(out.String(), "Deleted every saved login") {
		t.Fatal("forget claimed success while plaintext remains")
	}
	if st, _ := s.State(); !st.Configured {
		t.Fatal("forget deleted the vault although it refused")
	}
	if names, _ := s.Profiles(); len(names) != 1 {
		t.Fatalf("forget deleted saved profiles although it refused: %v", names)
	}
}

// TestForgetRefusesOnUnverifiableMarker: a session dir whose ownership marker
// is present but cannot be read or decoded may hold decrypted cookies of a
// saved profile, so forget refuses, names it and keeps the vault. Mutation
// check: skipping such a dir lets forget delete the vault and print success.
func TestForgetRefusesOnUnverifiableMarker(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, marker string){
		"malformed": func(t *testing.T, m string) {
			if err := os.WriteFile(m, []byte("{damaged"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		// A directory in the marker's place cannot be read as a file on any OS.
		"unreadable": func(t *testing.T, m string) {
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(m, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, base, s := forgetFixture(t)
			identity := filepath.Join(dir, "cobrowse-profiles", "default.enc")
			orphan := plantSessionDir(t, base, "cb-0000000000000014", identity, 2147483000)
			damage(t, filepath.Join(orphan, ".citadel-profile"))
			var out bytes.Buffer
			err := runLocalPassphraseForget(dir, &out)
			if err == nil || !strings.Contains(err.Error(), orphan) || !strings.Contains(err.Error(), "nothing was deleted") {
				t.Fatalf("forget = %v, want a refusal naming %s", err, orphan)
			}
			if strings.Contains(out.String(), "Deleted every saved login") {
				t.Fatal("forget claimed success while a damaged-marker dir remains")
			}
			if st, _ := s.State(); !st.Configured {
				t.Fatal("forget deleted the vault although it refused")
			}
			if _, err := os.Stat(filepath.Join(orphan, "Default", "Cookies")); err != nil {
				t.Fatalf("the dir must be left in place: %v", err)
			}
		})
	}
}
