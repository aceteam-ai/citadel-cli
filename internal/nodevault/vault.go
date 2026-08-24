// Package nodevault implements the node master-PIN primitive: an access
// authorization gate that doubles as the key-derivation root for
// zero-knowledge at-rest encryption (aceteam-ai/citadel-cli#796).
//
// Design (see docs/node-master-pin.md, as amended by the issue #796
// comment thread):
//
//   - One master secret (default a 6-digit PIN, configurable up to a full
//     passphrase — see Policy) both gates online access AND roots the
//     at-rest encryption. The single-secret model is made truthful by an
//     entropy-gated badge (see Status / entropy.go), not by pretending a
//     6-digit PIN is strong.
//
//   - Envelope encryption. The master secret plus a DEDICATED node-held
//     pepper (a random file generated once, distinct from node.key which
//     rotates under #4583) feed Argon2id to derive a KEK. The KEK AEAD-wraps
//     a random 256-bit DEK. Consumers never touch the DEK directly: they
//     receive HKDF context-scoped subkeys through a Session (see session.go).
//     Rotating the master secret re-wraps the DEK; it never re-encrypts data.
//
//   - Verify-by-unwrap. There is no separate password hash. A wrong PIN
//     simply fails to AEAD-open the wrapped DEK; the AES-GCM tag is the
//     constant-time verifier. This is why the migration deletes the legacy
//     bcrypt PasscodeHash (a bcrypt hash cannot yield a key, and leaving it
//     behind re-introduces the cheap offline brute-force target the KDF
//     exists to remove).
//
// The vault is pure: it takes its directory as a constructor parameter and
// imports nothing from the rest of this module, so callers (cmd, config)
// depend on it without any import cycle.
package nodevault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/argon2"
	"gopkg.in/yaml.v3"
)

// Sentinel errors. Callers use errors.Is to distinguish an authorization
// failure (wrong PIN / tampered vault) from an operational one (locked out,
// not configured, weak secret rejected by policy).
var (
	// ErrWrongPIN is returned when the supplied secret does not unwrap the
	// DEK. Tampering with the wrapped DEK, salt, or pepper is indistinguishable
	// from a wrong PIN at this layer (all surface as an AEAD-open failure), so
	// it maps to the same error — fail closed, never return partial material.
	ErrWrongPIN = errors.New("nodevault: incorrect PIN or tampered vault")
	// ErrNotConfigured is returned by Verify/Unlock/ChangePIN when no vault
	// has been set up yet.
	ErrNotConfigured = errors.New("nodevault: no master PIN configured")
	// ErrAlreadyConfigured is returned by SetPIN when a vault already exists.
	// Use ChangePIN to rotate.
	ErrAlreadyConfigured = errors.New("nodevault: master PIN already configured")
	// ErrAckRequired is returned by SetPIN/ChangePIN when the caller did not
	// pass the data-loss acknowledgement. Enforced at the API boundary so no
	// consumer can set a master PIN without the user having acknowledged that
	// forgetting it (or losing the pepper) is unrecoverable.
	ErrAckRequired = errors.New("nodevault: data-loss acknowledgement required to set master PIN")
)

// Argon2id v1 defaults (docs/node-master-pin.md §3, Resolved decision #8).
// Stored per-vault in the header so a future default change never breaks an
// existing vault.
const (
	argonTime    = 3         // passes
	argonMemory  = 64 * 1024 // KiB (= 64 MiB)
	argonThreads = 4
	argonKeyLen  = 32 // 256-bit KEK
	saltLen      = 16
	dekLen       = 32 // 256-bit DEK
	gcmNonceLen  = 12
	pepperLen    = 32
	vaultVersion = 1
)

// wrapAAD binds a wrapped-DEK entry to its purpose so a wrapped DEK cannot be
// replayed as some other AES-GCM ciphertext.
var wrapAAD = []byte("nodevault/wrap/pin/v1")

const (
	vaultDirName  = "vault"
	vaultFileName = "vault.yaml"
	pepperName    = "pepper"
)

// argonParams records the KDF cost used for a given vault. Kept in the header.
type argonParams struct {
	Time    uint32 `yaml:"time"`
	Memory  uint32 `yaml:"memory"`
	Threads uint8  `yaml:"threads"`
	KeyLen  uint32 `yaml:"key_len"`
}

func defaultArgonParams() argonParams {
	return argonParams{Time: argonTime, Memory: argonMemory, Threads: argonThreads, KeyLen: argonKeyLen}
}

// wrapEntry is one method of unwrapping the shared DEK. v1 populates exactly
// one entry ("pin"), but the list shape reserves room for a future recovery
// key or hardware token to wrap the SAME DEK without re-encrypting any data
// (docs §7, extensibility).
type wrapEntry struct {
	Method     string `yaml:"method"`
	Nonce      string `yaml:"nonce"`       // base64, GCM nonce for the wrap
	WrappedDEK string `yaml:"wrapped_dek"` // base64, DEK sealed under the KEK
}

// header is the on-disk vault.yaml. No plaintext key material is ever written.
type header struct {
	Version int         `yaml:"version"`
	KDF     argonParams `yaml:"kdf"`
	Salt    string      `yaml:"salt"` // base64
	Wraps   []wrapEntry `yaml:"wraps"`
	// EntropyBits and MeetsE2EThreshold are a DELIBERATE, coarse leak: surfaces
	// need to show a truthful end-to-end-encryption badge (caveated for a
	// 6-digit PIN, unqualified for a strong passphrase) without unlocking the
	// vault. They describe the secret's estimated strength, never the secret.
	EntropyBits       float64 `yaml:"entropy_bits"`
	MeetsE2EThreshold bool    `yaml:"meets_e2e_threshold"`
}

// Vault is the sealed master-PIN store for one node, backed by a directory the
// caller supplies (in production, network.GetNodeConfigDir()).
type Vault struct {
	dir string

	// kdfMu serializes Argon2id so N concurrent gate checks don't each spike
	// 64 MiB simultaneously. Also guards lockout-state read-modify-write within
	// this process.
	kdfMu sync.Mutex

	// paramsOverride, when non-nil, replaces the default Argon2id cost. It is
	// set ONLY by tests (same package) so the suite is not tens of seconds of
	// KDF; production always uses defaultArgonParams(). The params a vault was
	// created with are recorded in its header regardless, so an override never
	// leaks into how an existing vault is opened.
	paramsOverride *argonParams
}

// Open returns a Vault backed by baseDir. It does not touch disk; call
// IsConfigured to check for an existing vault.
func Open(baseDir string) *Vault {
	return &Vault{dir: filepath.Join(baseDir, vaultDirName)}
}

// newParams returns the Argon2id cost to use when CREATING a wrap.
func (v *Vault) newParams() argonParams {
	if v.paramsOverride != nil {
		return *v.paramsOverride
	}
	return defaultArgonParams()
}

func (v *Vault) headerPath() string { return filepath.Join(v.dir, vaultFileName) }
func (v *Vault) pepperPath() string { return filepath.Join(v.dir, pepperName) }

// IsConfigured reports whether a master PIN has been set for this node.
func (v *Vault) IsConfigured() bool {
	_, err := os.Stat(v.headerPath())
	return err == nil
}

// SetPIN initializes the vault: enforces policy on pin, generates the pepper
// (if absent), a random salt and a random DEK, derives the KEK via Argon2id,
// wraps the DEK, and persists the vault atomically. It refuses if a vault
// already exists (use ChangePIN) and requires the data-loss acknowledgement.
func (v *Vault) SetPIN(pin string, policy Policy, ackDataLoss bool) error {
	if !ackDataLoss {
		return ErrAckRequired
	}
	if err := policy.Validate(pin); err != nil {
		return err
	}
	if v.IsConfigured() {
		return ErrAlreadyConfigured
	}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return fmt.Errorf("nodevault: create dir: %w", err)
	}

	pepper, err := v.loadOrCreatePepper()
	if err != nil {
		return err
	}
	defer zero(pepper)

	dek := make([]byte, dekLen)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return fmt.Errorf("nodevault: generate DEK: %w", err)
	}
	defer zero(dek)

	params := v.newParams()
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return fmt.Errorf("nodevault: generate salt: %w", err)
	}

	entry, err := wrapDEK(pin, pepper, salt, params, dek)
	if err != nil {
		return err
	}

	bits := estimateEntropyBits(pin)
	h := header{
		Version:           vaultVersion,
		KDF:               params,
		Salt:              b64(salt),
		Wraps:             []wrapEntry{entry},
		EntropyBits:       bits,
		MeetsE2EThreshold: bits >= policy.E2EThresholdBits,
	}
	return writeHeaderAtomic(v.headerPath(), h)
}

// ChangePIN re-derives a KEK from newPIN (fresh salt + nonce) and re-wraps the
// EXISTING DEK. It never touches already-encrypted data. Requires the current
// PIN, respects lockout, and enforces policy + acknowledgement on the new PIN.
func (v *Vault) ChangePIN(oldPIN, newPIN string, policy Policy, ackDataLoss bool) error {
	if !ackDataLoss {
		return ErrAckRequired
	}
	if err := policy.Validate(newPIN); err != nil {
		return err
	}

	v.kdfMu.Lock()
	defer v.kdfMu.Unlock()

	if err := v.checkLockedLocked(); err != nil {
		return err
	}
	h, dek, err := v.unwrapLocked(oldPIN)
	if err != nil {
		v.recordFailureLocked()
		return err
	}
	defer zero(dek)
	v.recordSuccessLocked()

	pepper, err := v.loadPepper()
	if err != nil {
		return err
	}
	defer zero(pepper)

	params := v.newParams()
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return fmt.Errorf("nodevault: generate salt: %w", err)
	}
	entry, err := wrapDEK(newPIN, pepper, salt, params, dek)
	if err != nil {
		return err
	}

	bits := estimateEntropyBits(newPIN)
	h.KDF = params
	h.Salt = b64(salt)
	h.Wraps = []wrapEntry{entry}
	h.EntropyBits = bits
	h.MeetsE2EThreshold = bits >= policy.E2EThresholdBits
	return writeHeaderAtomic(v.headerPath(), h)
}

// VerifyPIN reports whether pin unlocks the vault. It enforces lockout BEFORE
// running the (expensive) KDF, records the attempt result, and fails closed.
// Returns nil on success; ErrWrongPIN, ErrLockedOut, or ErrNotConfigured
// otherwise.
func (v *Vault) VerifyPIN(pin string) error {
	sess, err := v.Unlock(pin)
	if err != nil {
		return err
	}
	sess.Lock()
	return nil
}

// Unlock derives the KEK from pin, unwraps the DEK, and returns a Session. It
// fails closed on a wrong PIN (no partial unlock) and is subject to lockout.
func (v *Vault) Unlock(pin string) (*Session, error) {
	v.kdfMu.Lock()
	defer v.kdfMu.Unlock()

	if err := v.checkLockedLocked(); err != nil {
		return nil, err
	}
	_, dek, err := v.unwrapLocked(pin)
	if err != nil {
		v.recordFailureLocked()
		return nil, err
	}
	v.recordSuccessLocked()
	return newSession(dek), nil
}

// Status reports the vault's presence and truthful badge signal without
// unlocking it. Unlocked is always false here — a live session is held by the
// caller, not the Vault, so process-wide unlock state is the caller's concern
// (a future heartbeat field, docs §9). Configured/EntropyBits/MeetsThreshold
// come straight from the header.
func (v *Vault) Status() Status {
	h, err := v.readHeader()
	if err != nil {
		return Status{Configured: false}
	}
	return Status{
		Configured:     true,
		EntropyBits:    h.EntropyBits,
		MeetsThreshold: h.MeetsE2EThreshold,
	}
}

// Status is the badge signal (see entropy.go for the threshold rationale).
type Status struct {
	Configured     bool
	Unlocked       bool
	EntropyBits    float64
	MeetsThreshold bool
}

// unwrapLocked derives the KEK and AEAD-opens the wrapped DEK. Caller holds
// kdfMu. Returns the header (for callers that rewrite it) and the DEK.
func (v *Vault) unwrapLocked(pin string) (header, []byte, error) {
	h, err := v.readHeader()
	if err != nil {
		return header{}, nil, err
	}
	pepper, err := v.loadPepper()
	if err != nil {
		return header{}, nil, err
	}
	defer zero(pepper)

	salt, err := unb64(h.Salt)
	if err != nil {
		return header{}, nil, fmt.Errorf("nodevault: decode salt: %w", err)
	}
	entry, ok := pinWrap(h.Wraps)
	if !ok {
		return header{}, nil, ErrNotConfigured
	}
	nonce, err := unb64(entry.Nonce)
	if err != nil {
		return header{}, nil, fmt.Errorf("nodevault: decode nonce: %w", err)
	}
	wrapped, err := unb64(entry.WrappedDEK)
	if err != nil {
		return header{}, nil, fmt.Errorf("nodevault: decode wrapped DEK: %w", err)
	}

	kek := deriveKEK(pin, pepper, salt, h.KDF)
	defer zero(kek)

	dek, err := aeadOpen(kek, nonce, wrapped, wrapAAD)
	if err != nil {
		return header{}, nil, ErrWrongPIN
	}
	return h, dek, nil
}

// wrapDEK derives a KEK and seals the DEK under it. Shared by SetPIN/ChangePIN.
func wrapDEK(pin string, pepper, salt []byte, params argonParams, dek []byte) (wrapEntry, error) {
	kek := deriveKEK(pin, pepper, salt, params)
	defer zero(kek)
	nonce := make([]byte, gcmNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return wrapEntry{}, fmt.Errorf("nodevault: generate wrap nonce: %w", err)
	}
	wrapped, err := aeadSeal(kek, nonce, dek, wrapAAD)
	if err != nil {
		return wrapEntry{}, err
	}
	return wrapEntry{Method: "pin", Nonce: b64(nonce), WrappedDEK: b64(wrapped)}, nil
}

// deriveKEK computes the key-encryption key from the master secret and the
// node-held pepper via Argon2id. The pepper is a fixed 32-byte value, so
// pin||pepper is an unambiguous concatenation (no length-extension ambiguity)
// — the pepper is a secret, node-local input that raises the per-guess cost of
// an offline attack that has the salt and params but not the pepper file.
func deriveKEK(pin string, pepper, salt []byte, p argonParams) []byte {
	input := make([]byte, 0, len(pin)+len(pepper))
	input = append(input, []byte(pin)...)
	input = append(input, pepper...)
	defer zero(input)
	return argon2.IDKey(input, salt, p.Time, p.Memory, p.Threads, p.KeyLen)
}

func aeadSeal(key, nonce, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, aad), nil
}

func aeadOpen(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("nodevault: aes cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func pinWrap(wraps []wrapEntry) (wrapEntry, bool) {
	for _, w := range wraps {
		if w.Method == "pin" {
			return w, true
		}
	}
	return wrapEntry{}, false
}

// loadOrCreatePepper returns the node's pepper, generating and persisting it
// (0600) on first use. Losing this file makes the vault permanently
// unrecoverable — it is intentionally node-local and not backed up by this
// package (that is the zero-knowledge property, see docs §6).
func (v *Vault) loadOrCreatePepper() ([]byte, error) {
	if p, err := v.loadPepper(); err == nil {
		return p, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	pepper := make([]byte, pepperLen)
	if _, err := io.ReadFull(rand.Reader, pepper); err != nil {
		return nil, fmt.Errorf("nodevault: generate pepper: %w", err)
	}
	if err := writeFileAtomic(v.pepperPath(), pepper, 0o600); err != nil {
		zero(pepper)
		return nil, err
	}
	return pepper, nil
}

func (v *Vault) loadPepper() ([]byte, error) {
	p, err := os.ReadFile(v.pepperPath())
	if err != nil {
		return nil, err
	}
	if len(p) != pepperLen {
		return nil, fmt.Errorf("nodevault: pepper has wrong length %d", len(p))
	}
	return p, nil
}

func (v *Vault) readHeader() (header, error) {
	data, err := os.ReadFile(v.headerPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return header{}, ErrNotConfigured
		}
		return header{}, fmt.Errorf("nodevault: read vault: %w", err)
	}
	var h header
	if err := yaml.Unmarshal(data, &h); err != nil {
		return header{}, fmt.Errorf("nodevault: parse vault: %w", err)
	}
	return h, nil
}

func writeHeaderAtomic(path string, h header) error {
	data, err := yaml.Marshal(h)
	if err != nil {
		return fmt.Errorf("nodevault: marshal vault: %w", err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// writeFileAtomic writes to a temp file in the same dir, fsyncs it, and renames
// over the target so a crash never leaves a half-written vault or pepper.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("nodevault: create dir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("nodevault: temp file: %w", err)
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("nodevault: chmod temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("nodevault: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("nodevault: sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("nodevault: close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("nodevault: rename: %w", err)
	}
	cleanup = false
	return nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// zero best-effort wipes key material. Go gives no guarantee the compiler or
// GC won't have copied it, but zeroizing the primary buffer shortens the
// window a secret sits in memory.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
