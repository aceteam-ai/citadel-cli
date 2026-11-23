package harnessseat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

const registryFileName = "harness.yaml"

var (
	ErrRegistryMissing  = errors.New("seat_registry_missing")
	ErrRegistryConflict = errors.New("seat_registry_conflict")
)

// RegistrySnapshot is an opaque compare-and-swap token belonging to one store.
// Its private salt, local paths and resolver are never JSON projections.
type RegistrySnapshot struct {
	owner    *SaltStore
	document RegistryDocument
	revision string
	salt     [32]byte
}

func (r RegistrySnapshot) Seats() []Seat    { return r.document.Seats() }
func (r RegistrySnapshot) Revision() string { return r.revision }

func registryRevision(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// LoadRegistry never creates a registry or its identity salt.
func (s *SaltStore) LoadRegistry(ctx context.Context, resolve func(string) (string, error)) (RegistrySnapshot, error) {
	var result RegistrySnapshot
	err := s.withPrivateLock(ctx, func() error {
		raw, err := s.readRegistry()
		if err != nil {
			return err
		}
		result, err = s.snapshot(raw, resolve)
		return err
	})
	return result, err
}

func (s *SaltStore) snapshot(raw []byte, resolve func(string) (string, error)) (RegistrySnapshot, error) {
	doc, err := DecodeRegistry(raw, resolve)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	salt, err := s.readSalt()
	if err != nil {
		return RegistrySnapshot{}, err
	}
	return RegistrySnapshot{owner: s, document: doc, revision: registryRevision(raw), salt: salt}, nil
}

// InitializeRegistry is an explicit offline initialization operation. Existing
// seats without a salt are refused, never silently assigned a new identity.
func (s *SaltStore) InitializeRegistry(ctx context.Context, resolve func(string) (string, error)) (RegistrySnapshot, error) {
	var result RegistrySnapshot
	err := s.withPrivateLock(ctx, func() error {
		raw, err := s.readRegistry()
		missing := errors.Is(err, ErrRegistryMissing)
		if missing {
			raw = []byte("seats: []\n")
		} else if err != nil {
			return err
		}
		doc, err := DecodeRegistry(raw, resolve)
		if err != nil {
			return err
		}
		_, err = s.readSalt()
		if errors.Is(err, ErrSaltMissing) && len(doc.Seats()) == 0 {
			_, err = s.createSalt()
		}
		if err != nil {
			return err
		}
		if missing {
			if err := s.writeRegistry(raw, false); err != nil {
				return err
			}
		}
		result, err = s.snapshot(raw, resolve)
		return err
	})
	return result, err
}

// UpdateRegistry serializes cooperating writers and refuses a stale snapshot.
// It only changes metadata: no vendor directory is created, read or removed.
func (s *SaltStore) UpdateRegistry(ctx context.Context, previous RegistrySnapshot, seats []Seat) (RegistrySnapshot, error) {
	if previous.owner != s {
		return RegistrySnapshot{}, ErrRegistryConflict
	}
	var result RegistrySnapshot
	err := s.withPrivateLock(ctx, func() error {
		current, err := s.expectedRegistry(previous)
		if err != nil {
			return err
		}
		next, err := current.document.WithSeats(seats).Encode()
		if err != nil {
			return err
		}
		// Validate the exact bytes before publishing them, not just the caller's
		// typed input. Owner homes are resolved again by the trusted resolver.
		result, err = s.snapshot(next, previous.document.resolve)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return s.writeRegistry(next, true)
	})
	if err != nil {
		return RegistrySnapshot{}, err
	}
	return result, nil
}

// expectedRegistry must run under withPrivateLock. Keep the same fencing for
// direct updates and for local-edit preflight before any directory creation.
func (s *SaltStore) expectedRegistry(previous RegistrySnapshot) (RegistrySnapshot, error) {
	if previous.owner != s {
		return RegistrySnapshot{}, ErrRegistryConflict
	}
	raw, err := s.readRegistry()
	if err != nil {
		return RegistrySnapshot{}, err
	}
	if registryRevision(raw) != previous.revision {
		return RegistrySnapshot{}, ErrRegistryConflict
	}
	current, err := s.snapshot(raw, previous.document.resolve)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	if current.salt != previous.salt {
		return RegistrySnapshot{}, ErrRegistryConflict
	}
	return current, nil
}

func (s *SaltStore) readRegistry() ([]byte, error) {
	f, err := s.root.OpenFile(registryFileName, os.O_RDONLY|privateNoFollowFlags(), 0)
	if os.IsNotExist(err) {
		return nil, ErrRegistryMissing
	}
	if err != nil {
		return nil, ErrPrivateStore
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !privateStoreInfo(info, false) || info.Size() < 1 || info.Size() > MaxRegistryBytes {
		return nil, ErrPrivateStore
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxRegistryBytes+1))
	if err != nil || len(raw) > MaxRegistryBytes {
		return nil, ErrPrivateStore
	}
	return raw, nil
}

// A synced same-directory temporary inode is published atomically. The kernel
// sidecar lock is held by every caller. The local owner/root remain trusted;
// this does not fence an uncooperative owner editing their own files.
func (s *SaltStore) writeRegistry(raw []byte, replace bool) error {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return ErrPrivateStore
	}
	tmpName := ".harness-registry-tmp-" + hex.EncodeToString(suffix[:])
	f, err := s.root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|privateNoFollowFlags(), 0o600)
	if err != nil {
		return ErrPrivateStore
	}
	tmpExists := true
	defer func() {
		if tmpExists {
			_ = s.root.Remove(tmpName)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return ErrPrivateStore
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return ErrPrivateStore
	}
	if err := f.Close(); err != nil {
		return ErrPrivateStore
	}
	if replace {
		err = s.root.Rename(tmpName, registryFileName)
		if err != nil {
			return ErrPrivateStore
		}
		tmpExists = false
	} else {
		if err := s.root.Link(tmpName, registryFileName); err != nil {
			if os.IsExist(err) {
				return ErrRegistryConflict
			}
			return ErrPrivateStore
		}
		if err := s.root.Remove(tmpName); err != nil {
			return ErrPrivateStore
		}
		tmpExists = false
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return ErrPrivateStore
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return ErrPrivateStore
	}
	return nil
}
