package harnessseat

import (
	"context"
	"errors"
	"strings"
)

var errProbeObservation = errors.New("seat_probe_observation_invalid")

// probeObservation is an internal handoff for future trusted status adapters,
// not an HTTP/CLI input model or a credential container. No adapter or native
// probe is wired yet. Its identity and the supplied snapshot must be captured
// before probing; the adapter must establish provenance, directory privacy and
// process-lifetime custody, and fail closed on an
// invalid result, never fall back to a saved signed-in declaration.
//
// Raw account identity is used only to compute the local salted digest. Fields
// and debug formatting deliberately do not project it or local paths. The type
// is private to keep raw observations out of accidental external API contracts;
// this is not a cryptographic seal against trusted code in this package.
type probeObservation struct {
	identity    Identity
	auth        AuthState
	entitlement string
	planType    string
	models      []string
	accountID   string
}

func (probeObservation) String() string   { return "seat probe observation (redacted)" }
func (probeObservation) GoString() string { return "seat probe observation (redacted)" }

// reconcileProbeObservation publishes only bounded derived metadata. It cannot
// enable a seat, change its binding/roots/ceiling, create directories, execute a
// vendor or authorize a launch. Unknown/signed-out results clear derived login
// evidence; the last entitlement remains only a declaration, never authority.
// A stale result (including one predating local disable or salt replacement)
// refuses rather than rebasing itself onto new state. The mismatch flag is
// advisory and returned only after a successful publication.
func (s *SaltStore) reconcileProbeObservation(ctx context.Context, before RegistrySnapshot, observation probeObservation) (RegistrySnapshot, bool, error) {
	current, err := s.currentRegistry(ctx, before)
	if err != nil {
		return RegistrySnapshot{}, false, err
	}
	seats := current.Seats()
	for index := range seats {
		seat := &seats[index]
		if seat.ID != observation.identity.ID {
			continue
		}
		if seat.Identity != observation.identity {
			return RegistrySnapshot{}, false, errProbeObservation
		}
		mismatch := false
		switch observation.auth {
		case AuthSignedIn:
			if len(observation.models) > 64 || strings.TrimSpace(observation.accountID) != observation.accountID {
				return RegistrySnapshot{}, false, errProbeObservation
			}
			digest, err := AccountDigest(current.salt[:], seat.Harness, observation.accountID)
			if err != nil {
				return RegistrySnapshot{}, false, errProbeObservation
			}
			mismatch = seat.Entitlement != observation.entitlement
			seat.Auth, seat.AccountDigest = AuthSignedIn, digest
			seat.Entitlement, seat.PlanType = observation.entitlement, observation.planType
			seat.Models = append([]string{}, observation.models...)
		case AuthUnknown, AuthSignedOut:
			if observation.accountID != "" || observation.entitlement != "" || observation.planType != "" || len(observation.models) != 0 {
				return RegistrySnapshot{}, false, errProbeObservation
			}
			seat.Auth, seat.AccountDigest, seat.PlanType, seat.Models = observation.auth, "", "", nil
		default:
			return RegistrySnapshot{}, false, errProbeObservation
		}
		if validateSeat(*seat) != nil {
			return RegistrySnapshot{}, false, errProbeObservation
		}
		// The registry encoder revalidates all account bindings atomically
		// with the snapshot/salt check, so a duplicate never partially saves.
		after, err := s.UpdateRegistry(ctx, current, seats)
		if err != nil {
			return RegistrySnapshot{}, false, err
		}
		return after, mismatch, nil
	}
	return RegistrySnapshot{}, false, ErrSeatNotFound
}
