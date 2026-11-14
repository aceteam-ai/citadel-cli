package cobrowseprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// Store is a vault and the sealed profiles it protects, both under one
// directory. Citadel local mode uses one Store per OS user, rooted at its
// local state dir (aceteam#13503 S7). That vault is separate from the node
// master-PIN vault: its only secret is the at-rest passphrase, so no short
// access PIN ever wraps the key that seals saved logins (#820).
type Store struct {
	dir string
}

// NewStore returns the Store rooted at dir. It does not touch disk.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir is the Store's root directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) vault() *nodevault.Vault { return nodevault.Open(s.dir) }

// VaultState describes the passphrase without unlocking anything.
type VaultState struct {
	// Configured is true once a passphrase has been set.
	Configured bool `json:"configured"`
	// Strong is the vault header's claim that its secret meets the at-rest
	// threshold. The header is unauthenticated, so this only chooses which
	// message to show; UnlockAtRest checks the secret itself.
	Strong bool `json:"strong"`
}

// State reports whether a passphrase is set. A vault that cannot be checked
// is an error, never "not set" (which would offer to create a second one).
func (s *Store) State() (VaultState, error) {
	v := s.vault()
	ok, err := v.Configured()
	if err != nil {
		return VaultState{}, err
	}
	if !ok {
		return VaultState{}, nil
	}
	return VaultState{Configured: true, Strong: v.Status().MeetsThreshold}, nil
}

// Policy is the policy this Store's vault enforces.
func (s *Store) Policy() nodevault.Policy { return nodevault.LoadPolicy(s.dir) }

// SetPassphrase creates the vault. It refuses a passphrase below the at-rest
// threshold and requires the data-loss acknowledgement.
func (s *Store) SetPassphrase(passphrase string, ackDataLoss bool) error {
	return s.vault().SetPassphrase(passphrase, s.Policy(), ackDataLoss)
}

// ChangePassphrase rotates the passphrase, re-wrapping the existing key. Saved
// profiles stay readable.
func (s *Store) ChangePassphrase(current, passphrase string, ackDataLoss bool) error {
	return s.vault().ChangePassphrase(current, passphrase, s.Policy(), ackDataLoss)
}

// Lockout is the vault's failed-attempt lockout.
func (s *Store) Lockout() *nodevault.Lockout { return s.vault().Lockout() }

// ValidateName checks a profile name without touching disk.
func (s *Store) ValidateName(name string) error { return validateName(name) }

// Open unlocks profile name with passphrase (see OpenHandle).
func (s *Store) Open(name, passphrase string) (*Handle, error) {
	return OpenHandle(s.dir, name, passphrase, s.vault())
}

// PrivateDirs are the directories the Store writes: the vault and the sealed
// profiles. A caller creates them owner-only (on Windows with an explicit
// DACL) before the Store first writes; this package does not import the
// platform helpers that do so.
func (s *Store) PrivateDirs() []string {
	return []string{filepath.Join(s.dir, nodevault.DirName), filepath.Join(s.dir, profilesDirName)}
}

// Profiles lists the saved profile names (those with a sealed blob).
func (s *Store) Profiles() ([]string, error) { return s.namesWith(profileFileExt) }

// namesWith lists valid profile names that have a file ending in each suffix
// given, merged and sorted.
func (s *Store) namesWith(suffixes ...string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, profilesDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		for _, suf := range suffixes {
			n := e.Name()
			if !strings.HasSuffix(n, suf) {
				continue
			}
			n = strings.TrimSuffix(n, suf)
			if validateName(n) == nil && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// ErrProfilesInUse is returned by Forget while any profile is open.
var ErrProfilesInUse = errors.New("cobrowseprofile: a saved profile is open in a running session; stop it first")

// Forget deletes every saved profile and then the vault (its header and
// pepper), for a user who forgot the passphrase: under the no-recovery model
// nothing else can be done with the data. It first takes the lock of every
// profile that has a sealed blob or a lock file (a profile opened for the
// first time has only the lock) and refuses while any is held, so it cannot
// race a session's re-seal. The lockout history is kept, so forgetting cannot
// be used to reset it.
func (s *Store) Forget() error {
	names, err := s.namesWith(profileFileExt, profileFileExt+lockFileExt)
	if err != nil {
		return err
	}
	var held []string
	defer func() {
		for _, sp := range held {
			releaseProfileLock(sp)
		}
	}()
	for _, n := range names {
		sp := storePath(s.dir, n)
		if err := acquireProfileLock(sp); err != nil {
			if errors.Is(err, ErrProfileBusy) {
				return ErrProfilesInUse
			}
			return err
		}
		held = append(held, sp)
	}
	for _, n := range names {
		if err := os.Remove(storePath(s.dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cobrowseprofile: delete profile %s: %w", n, err)
		}
	}
	return s.vault().Remove()
}
