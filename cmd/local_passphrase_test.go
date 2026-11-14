package cmd

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
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
