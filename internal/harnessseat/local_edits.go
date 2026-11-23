package harnessseat

import (
	"context"
	"errors"
)

var ErrSeatNotFound = errors.New("seat_not_found")

// LocalSeatDeclaration contains only human-declared metadata. Observed plan,
// model, account and auth fields cannot be supplied through add. These helpers
// are reserved for a future owner-bound local CLI, not a remote enable route or
// launch admission. They have no production caller yet.
type LocalSeatDeclaration struct {
	ID          string   `json:"-"`
	Harness     Harness  `json:"-"`
	Owner       string   `json:"-"`
	ConfigDir   string   `json:"-"`
	Label       string   `json:"-"`
	Entitlement string   `json:"-"`
	Roots       []string `json:"-"`
	DataCeiling string   `json:"-"`
}

func (s *SaltStore) currentRegistry(ctx context.Context, previous RegistrySnapshot) (RegistrySnapshot, error) {
	if previous.owner != s {
		return RegistrySnapshot{}, ErrRegistryConflict
	}
	var current RegistrySnapshot
	err := s.withPrivateLock(ctx, func() error {
		var err error
		current, err = s.expectedRegistry(previous)
		return err
	})
	return current, err
}

// AddLocalSeat starts disabled and unknown, never inferring login or authority
// from a directory. A losing concurrent writer can leave its new private
// directory behind; it is not deleted or automatically adopted on retry.
func (s *SaltStore) AddLocalSeat(ctx context.Context, previous RegistrySnapshot, request LocalSeatDeclaration) (RegistrySnapshot, error) {
	current, err := s.currentRegistry(ctx, previous)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	seat := Seat{Identity: Identity{ID: request.ID, Harness: request.Harness, Owner: request.Owner, ConfigDir: request.ConfigDir}, Label: request.Label, Entitlement: request.Entitlement, Roots: append([]string{}, request.Roots...), DataCeiling: request.DataCeiling, Auth: AuthUnknown}
	seats := append(current.Seats(), seat)
	encoded, err := current.document.WithSeats(seats).Encode()
	if err != nil {
		return RegistrySnapshot{}, err
	}
	doc, err := DecodeRegistry(encoded, current.document.resolve)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	seats = doc.Seats()
	if err := ctx.Err(); err != nil {
		return RegistrySnapshot{}, err
	}
	dir, err := PrepareLocalSeatDirectory(seats[len(seats)-1].Identity, true)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	defer dir.Close()
	if err := dir.Check(); err != nil {
		return RegistrySnapshot{}, err
	}
	return s.UpdateRegistry(ctx, current, seats)
}

// SetLocalSeatEnabled changes a declaration, not login status or permission.
// Local disable remains available even if the vendor directory becomes unsafe.
// Enabling requires fresh directory checks but is still not launch authority.
func (s *SaltStore) SetLocalSeatEnabled(ctx context.Context, previous RegistrySnapshot, id string, enabled bool) (RegistrySnapshot, error) {
	current, err := s.currentRegistry(ctx, previous)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	seats := current.Seats()
	for i := range seats {
		if seats[i].ID != id {
			continue
		}
		if enabled {
			dir, err := PrepareLocalSeatDirectory(seats[i].Identity, false)
			if err != nil {
				return RegistrySnapshot{}, err
			}
			checkErr := dir.Check()
			closeErr := dir.Close()
			if checkErr != nil {
				return RegistrySnapshot{}, checkErr
			}
			if closeErr != nil {
				return RegistrySnapshot{}, closeErr
			}
		}
		seats[i].Enabled = enabled
		return s.UpdateRegistry(ctx, current, seats)
	}
	return RegistrySnapshot{}, ErrSeatNotFound
}

// RemoveLocalSeat removes a declaration only. It never opens or deletes the
// vendor directory, logs out an account, or changes a running process.
func (s *SaltStore) RemoveLocalSeat(ctx context.Context, previous RegistrySnapshot, id string) (RegistrySnapshot, error) {
	current, err := s.currentRegistry(ctx, previous)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	seats := current.Seats()
	for i := range seats {
		if seats[i].ID == id {
			return s.UpdateRegistry(ctx, current, append(seats[:i], seats[i+1:]...))
		}
	}
	return RegistrySnapshot{}, ErrSeatNotFound
}
