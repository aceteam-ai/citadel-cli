// Package botprofile is the at-rest store for meeting-bot browser profiles
// (aceteam-ai/citadel-cli#1761, browser runtime epic #1757, slice C3).
//
// A bot profile is unattended: no human types a passphrase when a meeting
// starts, so the passphrase vault behind internal/cobrowseprofile cannot guard
// it. Instead the profile key is a two-part split (decision D2):
//
//	key = HKDF-SHA256(node share || AceTeam share)
//
// The node share is a random 256-bit key file on this node, created on first
// use, never transmitted and never logged. The AceTeam share is held per
// identity row by the platform and handed to this node per job through a
// ShareProvider (slice A0 of the epic). Neither a copy of the disk nor a copy
// of the AceTeam database opens a profile alone.
//
// This package does not fork the profile machinery: it reuses the
// cobrowseprofile archive, single-session lock and atomic write through
// cobrowseprofile.OpenSealedHandle and only supplies the key source (a Sealer).
//
// Like cobrowseprofile this is a leaf: callers pass the node config dir
// (network.GetNodeConfigDir() in production, t.TempDir() in tests). Nothing
// here resolves a config dir itself.
package botprofile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
)

// DirName is the store directory under the node config dir. Job path sandboxes
// refuse it (internal/jobs.denyBotProfiles).
const DirName = "bot-profiles"

// ShareLen is the length in bytes of both key shares.
const ShareLen = 32

// Dir returns the store directory under nodeConfigDir.
func Dir(nodeConfigDir string) string { return filepath.Join(nodeConfigDir, DirName) }

// Errors. None carries key material, share bytes or provider text.
var (
	ErrInvalidIdentity  = errors.New("botprofile: invalid org or identity id")
	ErrShareUnavailable = errors.New("botprofile: AceTeam share unavailable")
	ErrBadShare         = errors.New("botprofile: key share has the wrong length")
	ErrNewerChromium    = errors.New("botprofile: profile was sealed by a newer Chromium major than this runtime")
	ErrUnknownChromium  = errors.New("botprofile: runtime Chromium major is unknown")
	ErrOpenFailed       = errors.New("botprofile: cannot open profile (wrong key, wrong org or identity, or corrupt blob)")
	ErrLocked           = errors.New("botprofile: key material has been cleared")
)

// ShareProvider releases the AceTeam share for one identity row. Production
// wiring (slice A0) takes it from the job payload after the platform
// authenticates this node; it must return exactly ShareLen bytes and must not
// keep them beyond the job. Open drops the provider's error text, so a
// provider error cannot leak into logs through this package.
type ShareProvider interface {
	AceTeamShare(ctx context.Context, orgID, identityID string) ([]byte, error)
}

// Identity names one profile: one (org, bot account, node) identity row.
type Identity struct {
	OrgID      string
	IdentityID string
}

var identityIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,59}$`)

// validate checks the ids. The identity id becomes the blob file name, so it is
// held to the cobrowseprofile name alphabet; the org id only enters key
// derivation and AAD, so it only has to be non-empty, bounded and printable.
func (id Identity) validate() error {
	if !identityIDPattern.MatchString(id.IdentityID) {
		return fmt.Errorf("%w: identity id", ErrInvalidIdentity)
	}
	if id.OrgID == "" || len(id.OrgID) > 128 {
		return fmt.Errorf("%w: org id", ErrInvalidIdentity)
	}
	for _, r := range id.OrgID {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%w: org id", ErrInvalidIdentity)
		}
	}
	return nil
}

// ProfileName is the cobrowseprofile name (blob file stem) for the identity.
func (id Identity) ProfileName() string { return "bot-" + id.IdentityID }

// ParseChromiumMajor extracts the major from a Chromium version string such as
// "120.0.6099.109". It is strict so a garbage probe result cannot read as a
// valid major.
func ParseChromiumMajor(version string) (int, error) {
	head, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	n, err := strconv.Atoi(head)
	if err != nil || n <= 0 || n > 9999 {
		return 0, ErrUnknownChromium
	}
	return n, nil
}

// Open returns the unlocked Handle for the identity's profile.
//
// It loads or creates the node share under Dir(nodeConfigDir), fetches the
// AceTeam share, derives the key and takes the single-session lock. It fails
// closed at every step and yields no handle. chromiumMajor is the major of the
// runtime that will run the profile: Materialize refuses a blob sealed by a
// newer major, and Persist records chromiumMajor in the sealed metadata.
//
// Plaintext shares are zeroed before Open returns; the derived key lives only
// in the returned handle's sealer, which Close clears.
func Open(ctx context.Context, nodeConfigDir string, id Identity, shares ShareProvider, chromiumMajor int) (*cobrowseprofile.Handle, error) {
	if err := id.validate(); err != nil {
		return nil, err
	}
	if chromiumMajor <= 0 {
		return nil, ErrUnknownChromium
	}
	if shares == nil {
		return nil, ErrShareUnavailable
	}
	dir := Dir(nodeConfigDir)
	nodeShare, err := loadOrCreateNodeShare(dir)
	if err != nil {
		return nil, err
	}
	defer zero(nodeShare)
	aceShare, err := shares.AceTeamShare(ctx, id.OrgID, id.IdentityID)
	if err != nil {
		return nil, ErrShareUnavailable // provider text dropped on purpose
	}
	defer zero(aceShare)
	key, err := deriveKey(nodeShare, aceShare, id)
	if err != nil {
		return nil, err
	}
	s, err := newSealer(key, id, chromiumMajor)
	zero(key)
	if err != nil {
		return nil, err
	}
	h, err := cobrowseprofile.OpenSealedHandle(dir, id.ProfileName(), contextLabel(id), s)
	if err != nil {
		s.Lock()
		return nil, err
	}
	return h, nil
}

// Reset deletes the identity's sealed blob ("start fresh" or the node side of a
// crypto-shred). It needs no key and refuses while a session holds the profile.
func Reset(nodeConfigDir string, id Identity) error {
	if err := id.validate(); err != nil {
		return err
	}
	return cobrowseprofile.ResetIn(Dir(nodeConfigDir), id.ProfileName())
}

// StaticShares is a ShareProvider backed by an in-memory map keyed by
// "orgID/identityID". It is the test provider that the A0 wiring replaces.
type StaticShares map[string][]byte

// AceTeamShare implements ShareProvider. It returns a copy.
func (s StaticShares) AceTeamShare(_ context.Context, orgID, identityID string) ([]byte, error) {
	v, ok := s[orgID+"/"+identityID]
	if !ok {
		return nil, errors.New("no share")
	}
	return append([]byte(nil), v...), nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
