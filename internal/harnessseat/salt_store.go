package harnessseat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

const (
	saltFileName = ".harness-account-salt"
	saltLockName = ".harness-account-salt.lock"
)

var (
	ErrSaltMissing      = errors.New("seat_salt_missing")
	ErrPrivateStore     = errors.New("seat_private_store_unsafe")
	ErrStoreUnsupported = errors.New("seat_private_store_unsupported")
)

// SaltStore pins an existing private directory chosen by a trusted local owner
// resolver. It never resolves a vendor home, opens a vendor credential, repairs
// permissions, or creates parent directories. The owning user and root remain
// trusted; this is not a sandbox against that user modifying their own files.
// Only explicit local metadata commands use this store; no vendor process or
// dispatched job is wired to it.
type SaltStore struct {
	mu   sync.Mutex
	root *os.Root
}

func OpenSaltStore(dir string) (*SaltStore, error) {
	if !privateStoreSupported() {
		return nil, ErrStoreUnsupported
	}
	before, err := os.Lstat(dir)
	if err != nil || !privateStoreInfo(before, true) {
		return nil, ErrPrivateStore
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrPrivateStore
	}
	after, err := root.Stat(".")
	if err != nil || !privateStoreInfo(after, true) || !os.SameFile(before, after) {
		root.Close()
		return nil, ErrPrivateStore
	}
	return &SaltStore{root: root}, nil
}

func (s *SaltStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	if err != nil {
		return ErrPrivateStore
	}
	return nil
}

// LoadSalt refuses a missing salt. Reads never initialize or rotate identity.
func (s *SaltStore) LoadSalt(ctx context.Context) ([32]byte, error) {
	return s.salt(ctx, false)
}

// InitializeSalt is reserved for a future explicit local registry-initializing
// operation. A missing salt is generated once; unsafe existing state is never
// overwritten or repaired. Callers must not expose the salt in a projection.
func (s *SaltStore) InitializeSalt(ctx context.Context) ([32]byte, error) {
	return s.salt(ctx, true)
}

func (s *SaltStore) salt(ctx context.Context, create bool) ([32]byte, error) {
	var value [32]byte
	err := s.withPrivateLock(ctx, func() error {
		var err error
		value, err = s.readSalt()
		if !create || !errors.Is(err, ErrSaltMissing) {
			return err
		}
		// Generic salt initialization cannot establish whether an existing
		// registry already uses this identity. Only InitializeRegistry may
		// initialize alongside a validated empty registry.
		if _, err := s.root.Lstat(registryFileName); !os.IsNotExist(err) {
			return ErrSaltMissing
		}
		value, err = s.createSalt()
		return err
	})
	if err != nil {
		return [32]byte{}, err
	}
	return value, nil
}

func (s *SaltStore) withPrivateLock(ctx context.Context, action func() error) error {
	if ctx == nil {
		return ErrPrivateStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return ErrPrivateStore
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The sidecar is persistent: deleting it would let concurrent callers lock
	// different inodes. Each store opens its own description, so separate stores
	// in this process and separate processes use the same kernel lock.
	f, err := s.root.OpenFile(saltLockName, os.O_CREATE|os.O_RDWR|privateNoFollowFlags(), 0o600)
	if err != nil {
		return ErrPrivateStore
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !privateStoreInfo(info, false) {
		return ErrPrivateStore
	}
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := privateStoreLock(lockCtx, f); err != nil {
		return err
	}
	defer privateStoreUnlock(f)
	if err := ctx.Err(); err != nil {
		return err
	}
	return action()
}

func (s *SaltStore) readSalt() ([32]byte, error) {
	var value [32]byte
	f, err := s.root.OpenFile(saltFileName, os.O_RDONLY|privateNoFollowFlags(), 0)
	if os.IsNotExist(err) {
		return value, ErrSaltMissing
	}
	if err != nil {
		return value, ErrPrivateStore
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !privateStoreInfo(info, false) || info.Size() != int64(len(value)) {
		return value, ErrPrivateStore
	}
	if _, err := io.ReadFull(f, value[:]); err != nil {
		return [32]byte{}, ErrPrivateStore
	}
	return value, nil
}

func (s *SaltStore) createSalt() ([32]byte, error) {
	var value [32]byte
	var suffix [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return value, ErrPrivateStore
	}
	if _, err := rand.Read(suffix[:]); err != nil {
		return [32]byte{}, ErrPrivateStore
	}
	tmpName := ".harness-salt-tmp-" + hex.EncodeToString(suffix[:])
	f, err := s.root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|privateNoFollowFlags(), 0o600)
	if err != nil {
		return [32]byte{}, ErrPrivateStore
	}
	tmpExists := true
	defer func() {
		if tmpExists {
			_ = s.root.Remove(tmpName)
		}
	}()
	if _, err := f.Write(value[:]); err != nil {
		f.Close()
		return [32]byte{}, ErrPrivateStore
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return [32]byte{}, ErrPrivateStore
	}
	if err := f.Close(); err != nil {
		return [32]byte{}, ErrPrivateStore
	}
	// Link publishes a fully written inode without replacing an existing salt.
	// Readers take the same lock and cannot observe the temporary second link.
	if err := s.root.Link(tmpName, saltFileName); err != nil {
		if os.IsExist(err) {
			return s.readSalt()
		}
		return [32]byte{}, ErrPrivateStore
	}
	if err := s.root.Remove(tmpName); err != nil {
		return [32]byte{}, ErrPrivateStore
	}
	tmpExists = false
	dir, err := s.root.Open(".")
	if err != nil {
		return [32]byte{}, ErrPrivateStore
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return [32]byte{}, ErrPrivateStore
	}
	return value, nil
}
