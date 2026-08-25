package cobrowseprofile

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// The test vault runs full Argon2id (paramsOverride is private to nodevault), so
// it is created exactly once for the whole package and shared across tests. Each
// OpenHandle still pays one Unlock; tests are written to keep those few.
const testPIN = "123456"

var (
	sharedVaultOnce sync.Once
	sharedVaultDir  string
)

// testVault returns the shared configured vault plus the baseDir profiles live
// under. The vault dir and the profile store share one baseDir, matching
// production (both under the node config dir).
func testVault(t *testing.T) (*nodevault.Vault, string) {
	t.Helper()
	sharedVaultOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cobrowseprofile-vault-*")
		if err != nil {
			t.Fatalf("mkdir vault base: %v", err)
		}
		v := nodevault.Open(dir)
		if err := v.SetPIN(testPIN, nodevault.DefaultPolicy(), true); err != nil {
			t.Fatalf("SetPIN: %v", err)
		}
		sharedVaultDir = dir
	})
	return nodevault.Open(sharedVaultDir), sharedVaultDir
}

// uniqueName gives each test its own profile so the shared baseDir/vault does not
// leak state between tests.
func uniqueName(t *testing.T) string {
	t.Helper()
	// Test names contain '/' and '_'; map to the allowed [a-z0-9-] alphabet.
	name := "p-" + t.Name()
	out := make([]byte, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, byte(r))
		case r >= 'A' && r <= 'Z':
			out = append(out, byte(r-'A'+'a'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// writeCookie drops a recognizable "cookie" file into a profile working dir.
func writeCookie(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir cookie: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write cookie: %v", err)
	}
}

// TestEncryptAtRest is the core zero-knowledge acceptance test: after persisting a
// profile that contains a secret cookie, the stored blob on disk must NOT contain
// the plaintext cookie. Mutation check: storing the tar unencrypted, or skipping
// Seal, makes the secret appear in the blob and this fails.
func TestEncryptAtRest(t *testing.T) {
	v, baseDir := testVault(t)
	name := uniqueName(t)
	secret := "SECRET-COOKIE-VALUE-9f3a1b"

	h, err := OpenHandle(baseDir, name, testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle: %v", err)
	}
	work := t.TempDir()
	if err := h.Materialize(work); err != nil {
		t.Fatalf("Materialize (empty): %v", err)
	}
	writeCookie(t, work, "Default/Cookies", secret)
	if err := h.Persist(work); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	_ = h.Close()

	blob, err := os.ReadFile(storePath(baseDir, name))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if bytes.Contains(blob, []byte(secret)) {
		t.Fatal("stored profile blob contains the plaintext cookie: not encrypted at rest")
	}
	if len(blob) == 0 {
		t.Fatal("stored profile blob is empty")
	}
}

// TestRoundTripPersistsChange verifies the correct-PIN round trip: a change written
// in one session is visible after re-unlocking in the next. Mutation check: if
// Persist did not seal the working dir (or Materialize did not extract it), the
// cookie is missing on the second open.
func TestRoundTripPersistsChange(t *testing.T) {
	v, baseDir := testVault(t)
	name := uniqueName(t)
	secret := "logged-in-token-42"

	h1, err := OpenHandle(baseDir, name, testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle #1: %v", err)
	}
	work1 := t.TempDir()
	if err := h1.Materialize(work1); err != nil {
		t.Fatalf("Materialize #1: %v", err)
	}
	writeCookie(t, work1, "Default/Cookies", secret)
	writeCookie(t, work1, "Default/Cookies-journal", "wal-uncommitted")
	if err := h1.Persist(work1); err != nil {
		t.Fatalf("Persist #1: %v", err)
	}
	_ = h1.Close()

	h2, err := OpenHandle(baseDir, name, testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle #2: %v", err)
	}
	defer h2.Close()
	work2 := t.TempDir()
	if err := h2.Materialize(work2); err != nil {
		t.Fatalf("Materialize #2: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(work2, "Default/Cookies"))
	if err != nil {
		t.Fatalf("read restored cookie: %v", err)
	}
	if string(got) != secret {
		t.Fatalf("restored cookie = %q, want %q", got, secret)
	}
	// The sqlite -journal sidecar must survive (uncommitted login state).
	if _, err := os.Stat(filepath.Join(work2, "Default/Cookies-journal")); err != nil {
		t.Errorf("journal sidecar not persisted: %v", err)
	}
}

// TestWrongPINFailsClosed verifies a wrong PIN yields no handle and no session. The
// profile lock must be released so a later correct PIN still works. Mutation check:
// if OpenHandle ignored the Unlock error, err would be nil here.
func TestWrongPINFailsClosed(t *testing.T) {
	v, baseDir := testVault(t)
	name := uniqueName(t)

	if _, err := OpenHandle(baseDir, name, "000000", v); err == nil {
		t.Fatal("OpenHandle with wrong PIN returned no error")
	}
	// The lock must have been released: a correct PIN now succeeds.
	h, err := OpenHandle(baseDir, name, testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle after wrong PIN (lock leaked?): %v", err)
	}
	_ = h.Close()
}

// TestEmptyPINFailsClosed verifies an absent PIN is rejected (nodevault rejects the
// empty PIN for free; the handle must surface that).
func TestEmptyPINFailsClosed(t *testing.T) {
	v, baseDir := testVault(t)
	if _, err := OpenHandle(baseDir, uniqueName(t), "", v); err == nil {
		t.Fatal("OpenHandle with empty PIN returned no error")
	}
}

// TestReset discards the stored profile so the next session starts fresh. Mutation
// check: if Reset did not remove the store, the cookie would still materialize.
func TestReset(t *testing.T) {
	v, baseDir := testVault(t)
	name := uniqueName(t)

	h, err := OpenHandle(baseDir, name, testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle: %v", err)
	}
	work := t.TempDir()
	_ = h.Materialize(work)
	writeCookie(t, work, "Default/Cookies", "to-be-wiped")
	if err := h.Persist(work); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	_ = h.Close()

	if _, err := os.Stat(storePath(baseDir, name)); err != nil {
		t.Fatalf("store missing before reset: %v", err)
	}
	if err := Reset(baseDir, name); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := os.Stat(storePath(baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("store still present after reset: %v", err)
	}
	// Reset of an already-absent profile is not an error.
	if err := Reset(baseDir, name); err != nil {
		t.Fatalf("Reset of absent profile: %v", err)
	}
}

// TestSingleSessionLock enforces the v1 single-session-per-profile constraint. The
// busy path returns BEFORE any Unlock, so this test pays only two Argon2id runs.
func TestSingleSessionLock(t *testing.T) {
	v, baseDir := testVault(t)
	name := uniqueName(t)

	h1, err := OpenHandle(baseDir, name, testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle #1: %v", err)
	}
	if _, err := OpenHandle(baseDir, name, testPIN, v); err != ErrProfileBusy {
		t.Fatalf("second OpenHandle = %v, want ErrProfileBusy", err)
	}
	// Reset must also refuse while the profile is live.
	if err := Reset(baseDir, name); err != ErrProfileBusy {
		t.Fatalf("Reset while live = %v, want ErrProfileBusy", err)
	}
	_ = h1.Close()

	// After Close the lock is free again.
	h2, err := OpenHandle(baseDir, name, testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle after Close: %v", err)
	}
	_ = h2.Close()
}

// TestCloseIdempotent verifies Close is safe to call multiple times (the manager
// calls it from more than one cleanup path).
func TestCloseIdempotent(t *testing.T) {
	v, baseDir := testVault(t)
	h, err := OpenHandle(baseDir, uniqueName(t), testPIN, v)
	if err != nil {
		t.Fatalf("OpenHandle: %v", err)
	}
	_ = h.Close()
	_ = h.Close() // must not panic or double-release into another profile's lock
}

// TestInvalidName rejects unsafe profile names before they reach the filesystem or
// the AEAD context. No vault needed (validation precedes Unlock).
func TestInvalidName(t *testing.T) {
	v, baseDir := testVault(t)
	for _, bad := range []string{"", "../escape", "a/b", "UPPER", "space name", "dot.dot", string(make([]byte, 65))} {
		if _, err := OpenHandle(baseDir, bad, testPIN, v); err == nil {
			t.Errorf("OpenHandle(%q) accepted an invalid name", bad)
		}
		if err := Reset(baseDir, bad); err == nil {
			t.Errorf("Reset(%q) accepted an invalid name", bad)
		}
	}
}
