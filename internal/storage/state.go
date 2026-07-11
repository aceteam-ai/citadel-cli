// Package storage manages an on-node, S3-compatible object store backed by
// VersityGW (Apache-2.0). It is the node-side half of the object-storage epic
// (aceteam-ai/citadel-cli#466); this M1 slice provides the
// `citadel storage start|status|stop` command group.
//
// The gateway runs as a managed docker container (like the apps catalog in
// internal/apps and the payload instances in internal/jobs): a pinned image,
// `--restart unless-stopped` so it survives a node reboot, and a persistent
// state file under ~/.citadel so start/status/stop share one source of truth.
//
// Two invariants make this store safe to depend on:
//
//   - Create-once credentials. The root access/secret are minted on the FIRST
//     start and persisted; a restart reuses them verbatim. Regenerating them
//     would orphan every stored object (their on-disk owner would no longer
//     match) and invalidate every outstanding presigned URL (the secret signs
//     them). See Credentials / LoadOrCreateState.
//   - A fixed host port (services.StorageHostPort). S3 presigned URLs sign the
//     endpoint host+port, so the publish must be stable across restarts. A
//     registry slot is stable by construction; a dynamically probed port could
//     land elsewhere after reboot and silently break signed URLs.
package storage

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Credentials is the create-once S3 root key pair minted on first start.
type Credentials struct {
	// AccessKey is the S3 root access key id (ROOT_ACCESS_KEY).
	AccessKey string `json:"access_key"`
	// SecretKey is the S3 root secret access key (ROOT_SECRET_KEY). It signs
	// presigned URLs, so it must never change once objects exist.
	SecretKey string `json:"secret_key"`
}

// State is the persisted storage-service state. It currently holds only the
// create-once credentials; the port is a fixed registry slot and the backing
// dirs are derived from the base dir, so neither is persisted here.
type State struct {
	Credentials Credentials `json:"credentials"`

	mu   sync.Mutex
	path string
}

// accessKeyLen / secretKeyLen size the minted credentials. 20/40 alphanumeric
// chars mirror the shape of AWS-style keys, which keeps them compatible with S3
// clients that validate key length.
const (
	accessKeyLen = 20
	secretKeyLen = 40
)

// credCharset is the alphabet for minted credentials: URL/HTTP-header safe so a
// key never needs escaping in a docker -e value or an S3 Authorization header.
const credCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// BaseDir returns ~/.citadel/storage, the root of the storage service's data
// and state. Bounded there for the same reason as the payload state volume in
// internal/jobs: everything the container mounts stays inside a citadel data
// dir.
func BaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".citadel", "storage"), nil
}

// DataDir returns the posix backing directory the S3 buckets/objects live in.
// It is deliberately separate from IAMDir so the bucket/bytes accounting in
// status only ever sees real S3 data, not gateway metadata.
func DataDir() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "data"), nil
}

// IAMDir returns the directory VersityGW stores its internal IAM state in. Kept
// OUT of DataDir so it never inflates the bucket count or bytes-used report.
func IAMDir() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "iam"), nil
}

// statePath returns ~/.citadel/storage/state.json.
func statePath() (string, error) {
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "state.json"), nil
}

// resolveBackingDir bounds a requested backing directory to <home>/.citadel/
// storage, mirroring internal/jobs.resolveStateVolumePath. A leading "~" is
// expanded to homeDir; the result must resolve to the storage base dir or a path
// beneath it, else it is rejected. This is what stops a caller from pointing the
// S3 backing store at, say, ~/.ssh or /etc. Pure (no I/O) apart from the passed
// homeDir, so it is table-testable.
func resolveBackingDir(raw, homeDir string) (string, error) {
	if homeDir == "" {
		return "", fmt.Errorf("cannot resolve backing dir: home directory unknown")
	}
	base := filepath.Join(homeDir, ".citadel", "storage")

	raw = strings.TrimSpace(raw)
	if raw == "" {
		// Default: the standard data dir under the storage base.
		return filepath.Join(base, "data"), nil
	}

	expanded := raw
	switch {
	case raw == "~":
		expanded = homeDir
	case strings.HasPrefix(raw, "~/"):
		expanded = filepath.Join(homeDir, raw[2:])
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("failed to resolve backing dir %q: %w", raw, err)
	}
	abs = filepath.Clean(abs)

	if abs == base || strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return abs, nil
	}
	return "", fmt.Errorf(
		"backing dir %q resolves to %q, which is outside the storage data dir (allowed: %s)",
		raw, abs, base)
}

// LoadOrCreateState loads the persisted state, minting and persisting fresh
// credentials on first use. It is the create-once entry point: a second call
// always returns the SAME credentials, so a restart never regenerates them.
func LoadOrCreateState() (*State, error) {
	s, err := loadState()
	if err != nil {
		return nil, err
	}
	if s.Credentials.AccessKey != "" && s.Credentials.SecretKey != "" {
		return s, nil
	}
	// First start (or a state file predating credentials): mint once and persist
	// before returning so a crash between here and container launch still leaves
	// the same keys on disk for the next attempt.
	creds, err := generateCredentials()
	if err != nil {
		return nil, err
	}
	s.Credentials = creds
	if err := s.Save(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadState reads the default state.json. A missing file yields an empty
// (uninitialised) state, not an error, so the first start works on a fresh node.
func loadState() (*State, error) {
	p, err := statePath()
	if err != nil {
		return nil, err
	}
	return loadStateFromPath(p)
}

// loadStateFromPath reads a state file from an explicit path (injectable for
// tests). A missing file yields an empty state.
func loadStateFromPath(p string) (*State, error) {
	s := &State{path: p}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("failed to read storage state %s: %w", p, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("failed to parse storage state %s: %w", p, err)
	}
	return s, nil
}

// Save writes the state atomically (temp file + rename) with 0600 perms, since
// it holds the S3 secret key.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("failed to create storage state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal storage state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("failed to write storage state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to persist storage state: %w", err)
	}
	return nil
}

// generateCredentials mints a random access/secret key pair using crypto/rand.
func generateCredentials() (Credentials, error) {
	access, err := randomString(accessKeyLen)
	if err != nil {
		return Credentials{}, fmt.Errorf("failed to generate access key: %w", err)
	}
	secret, err := randomString(secretKeyLen)
	if err != nil {
		return Credentials{}, fmt.Errorf("failed to generate secret key: %w", err)
	}
	return Credentials{AccessKey: access, SecretKey: secret}, nil
}

// randomString returns a cryptographically random string of n chars over
// credCharset.
func randomString(n int) (string, error) {
	out := make([]byte, n)
	limit := big.NewInt(int64(len(credCharset)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[i] = credCharset[idx.Int64()]
	}
	return string(out), nil
}
