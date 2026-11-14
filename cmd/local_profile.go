// cmd/local_profile.go
//
// Saved logins for Citadel local mode (aceteam#13503 S7). A local_cobrowse
// session that names a profile starts locked; the user unlocks it with a
// passphrase in the viewer, and the profile is decrypted into the session's
// private working dir while the browser runs and re-encrypted on stop.
//
// The passphrase store is per OS user and lives in the local state dir
// (localStateDir), next to the owner lock and the audit log, never in the
// machine's node config dir (#1592). It is its own vault, separate from the
// node master PIN: its only secret is a passphrase at or above the at-rest
// threshold (#820), so a short access PIN never wraps the key that seals
// saved logins.
package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// localProfileStore adapts cobrowseprofile.Store to localcobrowse.ProfileStore
// and turns vault errors into one plain line for the viewer. dir is resolved
// on each call, as the owner dir is.
type localProfileStore struct {
	dir func() string
}

func (l localProfileStore) store() *cobrowseprofile.Store { return cobrowseprofile.NewStore(l.dir()) }

func (l localProfileStore) ValidateName(name string) error { return l.store().ValidateName(name) }

func (l localProfileStore) State() (localcobrowse.ProfileVaultState, error) {
	st, err := l.store().State()
	if err != nil {
		return localcobrowse.ProfileVaultState{}, err
	}
	return localcobrowse.ProfileVaultState{Configured: st.Configured, Strong: st.Strong}, nil
}

func (l localProfileStore) SetPassphrase(passphrase string, ackDataLoss bool) error {
	return friendlyProfileError(l.store().SetPassphrase(passphrase, ackDataLoss))
}

func (l localProfileStore) Open(name, passphrase string) (platform.SessionProfile, error) {
	h, err := l.store().Open(name, passphrase)
	if err != nil {
		// Never return a typed nil inside the interface.
		return nil, friendlyProfileError(err)
	}
	return h, nil
}

// profileError keeps the vault error for errors.Is while showing only msg.
type profileError struct {
	msg string
	err error
}

func (e *profileError) Error() string { return e.msg }
func (e *profileError) Unwrap() error { return e.err }

// friendlyProfileError maps a vault or profile error to one plain line with a
// next step. It never includes the passphrase (no vault error carries it).
func friendlyProfileError(err error) error {
	if err == nil {
		return nil
	}
	var lo *nodevault.ErrLockedOut
	var msg string
	switch {
	case errors.As(err, &lo):
		msg = fmt.Sprintf("Too many wrong passphrases. Try again after %s, or run `citadel local passphrase reset-lockout` in a terminal.",
			lo.Until.Local().Format(time.Kitchen))
	case errors.Is(err, nodevault.ErrWrongPIN):
		msg = "That passphrase is not right."
	case errors.Is(err, nodevault.ErrWeakSecret):
		msg = "That passphrase is too short to protect saved logins on disk. Use four or more random words."
	case errors.Is(err, nodevault.ErrNotConfigured):
		msg = "No passphrase is set on this computer yet."
	case errors.Is(err, nodevault.ErrAlreadyConfigured):
		msg = "A passphrase is already set on this computer; enter it to unlock."
	case errors.Is(err, nodevault.ErrAckRequired):
		msg = "Confirm that you understand a forgotten passphrase cannot be recovered."
	case errors.Is(err, cobrowseprofile.ErrProfilesInUse):
		msg = "A saved profile is open in a running session. Stop it first."
	case errors.Is(err, cobrowseprofile.ErrProfileBusy):
		msg = "These saved logins are open in another Citadel session (perhaps another agent app). Stop that session first."
	case errors.Is(err, nodevault.ErrLockoutUnavailable):
		msg = "Citadel cannot record passphrase attempts on this computer, so it will not check one. Run `citadel local passphrase status` in a terminal."
	default:
		msg = "Citadel could not open the saved logins: " + err.Error()
	}
	return &profileError{msg: msg, err: err}
}
