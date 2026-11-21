package nodevault

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// compartmentKeyBytes is the size of a node-key-file compartment key.
const compartmentKeyBytes = 32

// ErrCompartmentKeyUnsafe reports a key file this process refuses to trust.
var ErrCompartmentKeyUnsafe = errors.New("nodevault: compartment key file is not private")

// KeyCompartment seals and opens secrets under a node-local key file. It is
// deliberately NOT a *Session: the at-rest consumers (UnlockAtRest and its
// passphrase-strength floor) take a Session, and a compartment keyed by a file
// the node can read unattended must never be passable where that floor is
// meant to apply. It exposes Seal/Unseal only, never the key or a subkey.
type KeyCompartment struct{ s *Session }

// Seal encrypts plaintext bound to context (see Session.Seal).
func (k *KeyCompartment) Seal(context string, plaintext []byte) ([]byte, error) {
	return k.s.Seal(context, plaintext)
}

// Unseal reverses Seal.
func (k *KeyCompartment) Unseal(context string, ciphertext []byte) ([]byte, error) {
	return k.s.Unseal(context, ciphertext)
}

// OpenKeyCompartment opens a compartment keyed by a node-local key file instead
// of the human PIN. It exists for secrets a node must open unattended (a
// declared session's credentials at wake): the master PIN stays human-only, and
// a copied volume or sealed blob is useless without this file.
//
// The key file is created 0600 (in a 0700 directory) when create is true and
// the file is absent. An existing file is opened without following a final
// symlink and judged on the descriptor: it must be a regular, owner-only file
// of exactly the expected size, owned by this process's user. A loose,
// truncated or foreign-owned key is refused, never repaired.
func OpenKeyCompartment(keyPath string, create bool) (*KeyCompartment, error) {
	if !filepath.IsAbs(keyPath) {
		return nil, fmt.Errorf("nodevault: compartment key path must be absolute")
	}
	key, err := readCompartmentKey(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, err
		}
		key, err = createCompartmentKey(keyPath)
	}
	if err != nil {
		return nil, err
	}
	defer zero(key)
	return &KeyCompartment{s: newSession(key)}, nil
}

func readCompartmentKey(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		if isSymlinkOpenError(err) {
			return nil, ErrCompartmentKeyUnsafe
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != compartmentKeyBytes || !ownedByCurrentUser(info) {
		return nil, ErrCompartmentKeyUnsafe
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, ErrCompartmentKeyUnsafe
	}
	key := make([]byte, compartmentKeyBytes)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, ErrCompartmentKeyUnsafe
	}
	return key, nil
}

func createCompartmentKey(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("nodevault: create compartment dir: %w", err)
	}
	key := make([]byte, compartmentKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("nodevault: compartment key: %w", err)
	}
	// O_EXCL: never replace a key that appeared since the read above.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		zero(key)
		return nil, fmt.Errorf("nodevault: create compartment key: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		zero(key)
		return nil, fmt.Errorf("nodevault: write compartment key: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		zero(key)
		return nil, err
	}
	return key, nil
}
