package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A persistent (encrypted) profile is decrypted into the session's working dir
// while the browser runs and removed on teardown. A crash or kill skips that
// teardown, leaving plaintext cookies behind until a later sweep. The marker
// below ties such a dir to the sealed profile it came from, so the profile's
// owner can find and remove it, for example before telling the user that every
// saved login was deleted (aceteam#13503 S7).

// profileMarkerName is the marker file in a persistent session's working dir.
// cobrowseprofile excludes it from the sealed archive.
const profileMarkerName = ".citadel-profile"

// SessionProfileIdentity is implemented by a SessionProfile that can name the
// sealed profile it decrypts (cobrowseprofile.Handle returns its blob path).
type SessionProfileIdentity interface {
	ProfileIdentity() string
}

type profileMarker struct {
	Identity   string `json:"identity"`
	OwnerPID   int    `json:"owner_pid"`
	OwnerStart int64  `json:"owner_start,omitempty"`
}

// writeProfileMarker records which sealed profile, and which owner process,
// a persistent session's working dir belongs to. It must succeed before any
// plaintext is written there, so the dir can always be traced.
func writeProfileMarker(dir string, profile SessionProfile) error {
	id, ok := profile.(SessionProfileIdentity)
	if !ok {
		return nil
	}
	data, err := json.Marshal(profileMarker{Identity: id.ProfileIdentity(), OwnerPID: os.Getpid(), OwnerStart: currentProcessStartToken()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, profileMarkerName), data, 0o600)
}

// Retry budget for removing a stopped session's working dir. Vars for tests.
var (
	sessionRemoveAttempts = 25
	sessionRemoveDelay    = 200 * time.Millisecond
)

// Retry budget for removing an orphaned plaintext profile. Vars for tests.
var (
	orphanRemoveAttempts = 10
	orphanRemoveDelay    = 200 * time.Millisecond
)

// ErrProfilePlaintextRemains is wrapped by CleanOrphanedProfileSessions when a
// plaintext working copy of an owned profile could not be removed.
var ErrProfilePlaintextRemains = errors.New("a decrypted copy of a saved profile is still on disk")

// CleanOrphanedProfileSessions finds session working dirs under baseDir whose
// marker names a profile for which owns returns true, and removes the ones
// whose owner process is gone (an interrupted session). On Linux the recorded
// browser and Xvfb are killed first, only when their identity still verifies;
// on Windows the owner's Job Object already ended them. A dir whose owner is
// still running, or that cannot be removed, is left in place and reported:
// the error wraps ErrProfilePlaintextRemains and names every such dir. A
// missing baseDir means nothing to clean.
func CleanOrphanedProfileSessions(baseDir string, owns func(identity string) bool) error {
	if _, err := os.Lstat(baseDir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// Trust markers only in a session dir that is ours and private, as the
	// orphan sweep does: elsewhere another account could plant one.
	if err := ensureCobrowseBaseDir(baseDir); err != nil {
		return fmt.Errorf("co-browse session dir %s is not private to this user, so Citadel will not clean it: %w", baseDir, err)
	}
	entries, err := os.ReadDir(baseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read co-browse session dir %s: %w", baseDir, err)
	}
	var remain []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(baseDir, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, profileMarkerName))
		if err != nil {
			continue // not a persistent-profile session dir
		}
		var mk profileMarker
		if json.Unmarshal(data, &mk) != nil || mk.Identity == "" || !owns(mk.Identity) {
			continue
		}
		if mk.OwnerPID == os.Getpid() || sessionOwnerAlive(mk.OwnerPID, mk.OwnerStart) {
			remain = append(remain, fmt.Sprintf("%s (in use by running process %d)", dir, mk.OwnerPID))
			continue
		}
		if _, browserPID, xvfbPID := readSessionPidfile(dir); browserPID > 0 || xvfbPID > 0 {
			killOrphanSessionProcs(browserPID, xvfbPID)
		}
		if err := removeAllRetry(dir, orphanRemoveAttempts, orphanRemoveDelay); err != nil {
			remain = append(remain, fmt.Sprintf("%s (%v)", dir, err))
			continue
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			remain = append(remain, fmt.Sprintf("%s (still present after removal)", dir))
		}
	}
	if len(remain) > 0 {
		return fmt.Errorf("%w: %v", ErrProfilePlaintextRemains, remain)
	}
	return nil
}
