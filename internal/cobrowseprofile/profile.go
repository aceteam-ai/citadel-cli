// Package cobrowseprofile implements an encrypted-at-rest, passphrase-unlocked
// persistent browser profile for co-browse sessions (aceteam-ai/citadel-cli#795,
// aceteam#13503 S7).
//
// A co-browse session normally launches a browser with a THROWAWAY profile dir
// that is deleted on stop (see internal/platform/cobrowse_session.go), so every
// session starts logged out. This package gives a session a PERSISTENT profile
// instead: the profile survives across sessions so logins persist, but it lives
// on disk only as ciphertext, unreadable without the vault's passphrase.
//
// Which vault: a profile is sealed under the internal/nodevault vault in the
// same base dir as its blobs. Citadel local mode uses a Store rooted at the
// user's local state dir, whose vault is its own and holds only the saved-logins
// passphrase. An enrolled node's COBROWSE_SESSION job opens profiles in the node
// config dir under the node vault, so there the master PIN must itself be a
// passphrase. Either way the vault is unlocked only through
// nodevault.Vault.UnlockAtRest, which refuses any secret below the at-rest
// threshold (#820): a short access PIN never opens a profile.
//
// Design — consume the vault primitive, invent no crypto:
//
//   - At use time the caller hands over the passphrase, the vault is unlocked
//     at-rest, and we keep only the resulting Session. We never retain the
//     passphrase or see the master key; the Session zeroes its key material on
//     Close.
//
//   - The whole profile directory is serialized to a single tar blob and sealed
//     with Session.Seal under a STABLE per-profile context label. nodevault
//     binds that label as AEAD additional data, so a blob sealed for one profile
//     cannot be unsealed as another. The sealed blob is the only at-rest form.
//
//   - On unlock we untar the blob into a caller-supplied private working dir for
//     the browser to use; on stop we re-tar, re-seal, and atomically replace the
//     stored blob, then delete the plaintext working copy.
//
//   - One profile is open in at most one session, across processes too: an OS
//     lock file sits next to each sealed blob.
//
// This package imports nodevault (a leaf) but NOT internal/network or
// internal/platform: network imports platform, so anything platform can reach
// must not pull network back in. The caller passes the base dir instead.
package cobrowseprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// profilesDirName is the subdirectory of the node config dir that holds the
// sealed profile blobs. Each profile is one file, <name>.enc.
const profilesDirName = "cobrowse-profiles"

// profileFileExt is the extension of a sealed profile blob.
const profileFileExt = ".enc"

// lockFileExt is appended to a blob's path for its cross-process lock file.
// The lock file is never removed: unlinking it would let two processes lock
// two different inodes under one name.
const lockFileExt = ".lock"

// contextPrefix is the stable AEAD context label prefix for a sealed profile.
// The profile name is appended so each profile's ciphertext is bound to its own
// context and cannot be substituted for another's. The "/v1" fixes the label so
// a future format change is a distinct, non-colliding context.
const contextPrefix = "cobrowse-profile/v1/"

// profileNamePattern constrains a profile name: it becomes both a filename
// component and part of the AEAD context string, so it must not contain path
// separators, "..", or anything exotic. Lowercase alphanumerics and dashes,
// 1..64 chars.
var profileNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// Errors surfaced to callers. ErrProfileBusy enforces the v1 single-session-per-
// profile constraint; ErrInvalidName rejects an unsafe profile name before it
// reaches the filesystem or the AEAD context.
var (
	ErrProfileBusy = errors.New("cobrowseprofile: profile is already in use by another session")
	ErrInvalidName = errors.New("cobrowseprofile: invalid profile name")
)

// profileLocks enforces single-session-per-profile WITHIN this process: only one
// live Handle may exist per absolute store path at a time. A second OpenHandle
// for a profile already in use fails with ErrProfileBusy rather than letting two
// sessions decrypt the same profile, edit divergent working copies, and race to
// re-seal (last writer would silently win). Across processes the same rule is
// held by an OS file lock next to the sealed blob (lockFile, lock_*.go), so two
// harnesses that each run their own Citadel process cannot open one profile
// twice either (aceteam#13503 S7).
var (
	profileLocksMu sync.Mutex
	profileLocks   = map[string]func(){}
)

// acquireProfileLock takes the in-process and the OS lock for storePath. It
// returns ErrProfileBusy when either is held, or the error that stopped the OS
// lock from being taken (fail closed: no lock, no handle).
func acquireProfileLock(storePath string) error {
	profileLocksMu.Lock()
	defer profileLocksMu.Unlock()
	if _, held := profileLocks[storePath]; held {
		return ErrProfileBusy
	}
	release, err := lockFile(storePath + lockFileExt)
	if err != nil {
		return err
	}
	profileLocks[storePath] = release
	return nil
}

// releaseProfileLock drops the OS lock before the in-process entry, both under
// profileLocksMu, so a new acquire in this process never sees the entry gone
// while the OS lock is still held.
func releaseProfileLock(storePath string) {
	profileLocksMu.Lock()
	defer profileLocksMu.Unlock()
	if release := profileLocks[storePath]; release != nil {
		release()
	}
	delete(profileLocks, storePath)
}

// validateName returns nil if name is a safe profile name.
func validateName(name string) error {
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q (want [a-z0-9-]{1,64})", ErrInvalidName, name)
	}
	return nil
}

// storePath returns the absolute path of the sealed blob for name under baseDir.
// name is assumed already validated.
func storePath(baseDir, name string) string {
	return filepath.Join(baseDir, profilesDirName, name+profileFileExt)
}

// Handle is a single unlocked, single-session-locked persistent profile bound to
// one nodevault Session. It implements the Materialize / Persist / Close contract
// the co-browse session manager consumes (structurally, via an interface the
// platform package declares — this package does not import platform).
//
// Lifecycle: OpenHandle acquires the per-profile lock and the Session. Materialize
// decrypts the stored profile into the browser's working dir. Persist re-encrypts
// that working dir back to the store (call only after the browser actually ran, so
// an empty/partial dir never overwrites good ciphertext). Close zeroes the Session
// and releases the lock; it is idempotent and MUST run on every path so neither the
// key nor the profile lock leaks.
type Handle struct {
	storePath string
	context   string
	sess      *nodevault.Session

	closeOnce sync.Once
}

// OpenHandle unlocks the profile for use: it validates name, reserves the
// single-session lock (in this process and across processes), unlocks the vault
// with pin through nodevault's at-rest entry point, and returns a Handle. It fails
// closed — a wrong/absent secret, a secret below the at-rest threshold (#820, so a
// short access PIN is refused even when correct), a locked-out vault, or an
// unconfigured vault all return the vault's error and yield NO handle (the caller must not fall back to a
// plaintext or throwaway profile). The pin is consumed here and never retained;
// only the resulting Session is held.
//
// v is the node master vault (nodevault.Open(nodeConfigDir)); baseDir is that same
// node config dir. On any error the profile lock is released before returning.
func OpenHandle(baseDir, name, pin string, v *nodevault.Vault) (*Handle, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nodevault.ErrNotConfigured
	}
	sp := storePath(baseDir, name)
	if err := acquireProfileLock(sp); err != nil {
		return nil, err
	}
	// Unlock AFTER taking the lock so a wrong-PIN attempt does not leave the lock
	// held; release on every failure path below. The at-rest entry point refuses
	// a secret too weak to protect the profile on disk (#820): a bare access PIN
	// can never open a persistent profile.
	sess, err := v.UnlockAtRest(pin, nodevault.LoadPolicy(baseDir))
	if err != nil {
		releaseProfileLock(sp)
		return nil, err
	}
	return &Handle{
		storePath: sp,
		context:   contextPrefix + name,
		sess:      sess,
	}, nil
}

// Materialize decrypts the stored profile into dir, which the caller has already
// created private (0700) and empty. If no stored profile exists yet (first use),
// it leaves dir empty so the browser starts a fresh profile. It fails closed: any
// unseal/extract error returns without leaving a partial profile the caller might
// treat as authentic.
func (h *Handle) Materialize(dir string) error {
	blob, err := os.ReadFile(h.storePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // first use: fresh empty profile
		}
		return fmt.Errorf("cobrowseprofile: read store: %w", err)
	}
	plain, err := h.sess.Unseal(h.context, blob)
	if err != nil {
		// Wrong context, tampered blob, or a locked session: fail closed.
		return fmt.Errorf("cobrowseprofile: unseal profile: %w", err)
	}
	defer zero(plain)
	if err := extractTar(plain, dir); err != nil {
		return fmt.Errorf("cobrowseprofile: extract profile: %w", err)
	}
	return nil
}

// Persist re-encrypts dir back to the store: it tars the working profile, seals it
// under the profile context, and atomically replaces the stored blob. Call this
// ONLY after the browser actually ran; sealing an empty or half-built dir would
// overwrite a good profile with nothing.
func (h *Handle) Persist(dir string) error {
	plain, err := buildTar(dir)
	if err != nil {
		return fmt.Errorf("cobrowseprofile: archive profile: %w", err)
	}
	defer zero(plain)
	blob, err := h.sess.Seal(h.context, plain)
	if err != nil {
		return fmt.Errorf("cobrowseprofile: seal profile: %w", err)
	}
	if err := writeFileAtomic(h.storePath, blob, 0o600); err != nil {
		return fmt.Errorf("cobrowseprofile: write store: %w", err)
	}
	return nil
}

// ProfileIdentity names the sealed profile this handle decrypts: the absolute
// path of its blob. The co-browse session manager records it in the working
// dir so a crash-leftover decrypted copy can be traced back (Store.Owns).
func (h *Handle) ProfileIdentity() string { return absPath(h.storePath) }

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// Close zeroes the Session's key material and releases the single-session lock. It
// is idempotent (safe to call from multiple cleanup paths) and never persists —
// callers decide whether to Persist first.
func (h *Handle) Close() error {
	h.closeOnce.Do(func() {
		if h.sess != nil {
			h.sess.Lock()
		}
		releaseProfileLock(h.storePath)
	})
	return nil
}

// Reset discards the encrypted profile for name under baseDir, deleting its stored
// blob so the next session starts fresh. It deliberately does NOT require the PIN:
// under the no-recovery master-PIN model a user who forgets the PIN can never
// unlock the profile again, so reset is the ONLY escape hatch and must work
// without it. It DOES take the single-session lock first, so it refuses while a
// live session holds the profile — otherwise the delete would race that session's
// Persist-on-stop and be silently undone.
func Reset(baseDir, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	sp := storePath(baseDir, name)
	if err := acquireProfileLock(sp); err != nil {
		return err
	}
	defer releaseProfileLock(sp)
	if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cobrowseprofile: reset: %w", err)
	}
	return nil
}

// zero best-effort wipes a plaintext buffer once it is no longer needed, matching
// nodevault's own hygiene. Go cannot guarantee no copy was made, but clearing the
// primary buffer shortens the window decrypted cookies sit in memory.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
