// Package cobrowseprofile implements an encrypted-at-rest, PIN-unlocked
// persistent browser profile for co-browse sessions (aceteam-ai/citadel-cli#795).
//
// A co-browse session normally launches a browser with a THROWAWAY profile dir
// that is deleted on stop (see internal/platform/cobrowse_session.go), so every
// session starts logged out. This package gives a session a PERSISTENT profile
// instead: the profile survives across sessions so logins persist, but it lives
// on disk only as ciphertext, unreadable without the node master PIN.
//
// Design — consume the node master-PIN primitive, invent no crypto:
//
//   - At use time the caller unlocks the shared vault (internal/nodevault) with
//     the PIN and hands us the resulting Session. We never see the PIN or the
//     master key; we only hold the short-lived Session, which zeroes its key
//     material on Close.
//
//   - The whole profile directory is serialized to a single tar blob and sealed
//     with Session.Seal under a STABLE per-profile context label. nodevault
//     binds that label as AEAD additional data, so a blob sealed for one profile
//     cannot be unsealed as another. The sealed blob is the only at-rest form:
//     that is the zero-knowledge property #796 provides, consumed here.
//
//   - On unlock we untar the blob into a caller-supplied private working dir for
//     the browser to use; on stop we re-tar, re-seal, and atomically replace the
//     stored blob, then delete the plaintext working copy.
//
// This package imports nodevault (a leaf) but NOT internal/network: network
// imports internal/platform, so anything platform can reach must not pull
// network back in. The caller passes the node config dir as baseDir instead.
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
// re-seal (last writer would silently win). Cross-process concurrency is not
// guarded here; because every store write is atomic (temp + fsync + rename), the
// worst cross-process outcome is a lost update, never a corrupt blob (v1).
var (
	profileLocksMu sync.Mutex
	profileLocks   = map[string]bool{}
)

func acquireProfileLock(storePath string) bool {
	profileLocksMu.Lock()
	defer profileLocksMu.Unlock()
	if profileLocks[storePath] {
		return false
	}
	profileLocks[storePath] = true
	return true
}

func releaseProfileLock(storePath string) {
	profileLocksMu.Lock()
	defer profileLocksMu.Unlock()
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
// single-session lock, unlocks the vault with pin, and returns a Handle. It fails
// closed — a wrong/absent PIN, a locked-out vault, or an unconfigured vault all
// return the vault's error and yield NO handle (the caller must not fall back to a
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
	if !acquireProfileLock(sp) {
		return nil, ErrProfileBusy
	}
	// Unlock AFTER taking the lock so a wrong-PIN attempt does not leave the lock
	// held; release on every failure path below.
	sess, err := v.Unlock(pin)
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
	if !acquireProfileLock(sp) {
		return ErrProfileBusy
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
