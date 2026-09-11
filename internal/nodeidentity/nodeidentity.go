// Package nodeidentity manages a fabric node's cryptographic identity: an
// EC P-256 keypair whose private half never leaves the node, a PKCS#10 CSR
// derived from it, and the CA-signed leaf certificate + trust chain the node
// receives at pairing time.
//
// This is PR-0 of P2, Epic #4583 (self-healing fabric node identity). The
// keypair + CSR are the prerequisite for mTLS self-reenrollment: a future
// `citadel reconnect` will present the stored leaf over mTLS to a self-scoped
// re-enrollment endpoint. Until the backend fabric CA is activated, everything
// on the cert path is best-effort and fail-open — a node with no CA/cert pairs
// and runs exactly as it does today.
//
// Storage layout (under platform.ConfigDir()/identity/):
//
//	node.key       EC P-256 private key (PKCS#8 PEM, 0600, never transmitted)
//	node.crt       CA-signed leaf certificate (PEM, public, 0644)
//	ca-chain.pem   fabric CA trust chain: intermediate || root (PEM, public, 0644)
//
// TPM: citadel has no TPM abstraction today (no internal/platform TPM seam),
// so the private key is software-protected via 0600 file permissions. TPM
// sealing is a follow-up once a platform TPM abstraction exists — see #4583.
package nodeidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const (
	// dirName is the identity subdirectory under the citadel config dir.
	dirName = "identity"

	keyFileName     = "node.key"
	leafFileName    = "node.crt"
	caChainFileName = "ca-chain.pem"

	// keyPerms restricts the private key to owner read/write only. The private
	// key must NEVER be transmitted, logged, or made group/world-readable.
	keyPerms = 0o600
	// pubPerms are used for the leaf and CA chain, which are public material.
	pubPerms = 0o644
	dirPerms = 0o700

	pemTypeECPrivateKey = "PRIVATE KEY" // PKCS#8
	pemTypeCSR          = "CERTIFICATE REQUEST"
	pemTypeCertificate  = "CERTIFICATE"
)

// Store is a filesystem-backed node identity store rooted at a directory.
// The zero value is not usable; construct one with New, Default, or Convergent.
//
// legacyDir, when non-empty, is a SECOND directory this store performs a
// one-time read-through from on first key/leaf access: a convergent store
// (see Convergent) adopts a key registered under the legacy invoker-scoped
// location so the AEP receipt signer and the CSR/enrollment flows use ONE key
// (K-A, docs/design-trust-receipt-v2.md §4, aceteam#8253). Empty for New and
// Default, which are byte-for-byte unchanged from before convergence.
type Store struct {
	dir       string
	legacyDir string
}

// newStore is the internal constructor. A legacyDir that is empty or equal to
// dir disables the read-through convergence (New/Default). Exposed to tests
// (in-package) so they can inject a temp legacy dir instead of the real
// platform.ConfigDir().
func newStore(dir, legacyDir string) *Store {
	if legacyDir == dir {
		legacyDir = ""
	}
	return &Store{dir: dir, legacyDir: legacyDir}
}

// Default returns a Store rooted at platform.ConfigDir()/identity.
//
// DEPRECATED for production identity paths: platform.ConfigDir() is
// invoker-scoped (user-local unless root), so a systemd-root `citadel work`
// and an interactive non-root process resolve DIFFERENT directories — the
// exact split that made the AEP receipt signer sign with a key the fabric CA
// never registered (docs/design-trust-receipt-v2.md §4). Every production
// identity path now uses Convergent instead; a NEW nodeidentity.Default()
// call site is how that bug comes back. Kept only for tests and any path that
// genuinely needs the invoker-scoped location.
func Default() *Store {
	return newStore(filepath.Join(platform.ConfigDir(), dirName), "")
}

// New returns a Store rooted at an explicit directory, with NO read-through
// convergence. Used in tests to avoid touching the real config dir.
func New(dir string) *Store {
	return newStore(dir, "")
}

// Convergent returns a Store rooted at <nodeConfigDir>/identity — the
// machine-convergent node config dir (network.GetNodeConfigDir(), the same
// directory #845's device config and #726's heartbeat marker converge on).
// The nodeConfigDir is threaded in from cmd / internal/worker because
// nodeidentity is a leaf package that must not import internal/network.
//
// Every production identity path constructs the store this way — the AEP
// receipt signer (internal/worker.defaultAEPSigner) and every CSR/enrollment
// site (cmd/init.go:ensureNodeIdentity, cmd/device.go) — so all of them
// resolve the IDENTICAL key file (K-A, docs/design-trust-receipt-v2.md §4,
// aceteam#8253). On first key/leaf access the store performs a one-time
// read-through of a key previously registered under the legacy invoker-scoped
// platform.ConfigDir()/identity location; see reconcileFromLegacy for the
// exact (never-displace-a-registered-key, never-destroy-key-material) rule.
func Convergent(nodeConfigDir string) *Store {
	return newStore(filepath.Join(nodeConfigDir, dirName), filepath.Join(platform.ConfigDir(), dirName))
}

// Dir returns the identity store directory.
func (s *Store) Dir() string { return s.dir }

// KeyPath returns the absolute path to the private key file.
func (s *Store) KeyPath() string { return filepath.Join(s.dir, keyFileName) }

// LeafPath returns the absolute path to the leaf certificate file.
func (s *Store) LeafPath() string { return filepath.Join(s.dir, leafFileName) }

// CAChainPath returns the absolute path to the CA trust chain file.
func (s *Store) CAChainPath() string { return filepath.Join(s.dir, caChainFileName) }

// HasKey reports whether a private key is already present on disk.
func (s *Store) HasKey() bool {
	_, err := os.Stat(s.KeyPath())
	return err == nil
}

// HasLeaf reports whether a leaf certificate is already present on disk.
func (s *Store) HasLeaf() bool {
	_, err := os.Stat(s.LeafPath())
	return err == nil
}

// GetOrCreateKey returns the node's EC P-256 private key, generating and
// persisting a fresh one (0600) if none exists yet. This is idempotent: on
// subsequent calls it loads the existing key from disk so the node keeps a
// stable identity across `citadel init` re-runs.
func (s *Store) GetOrCreateKey() (*ecdsa.PrivateKey, error) {
	// Read-through convergence (K-A, docs/design-trust-receipt-v2.md §4): before
	// minting a new key, adopt a key registered under the legacy invoker-scoped
	// location if one exists there. A HARD error if reconciliation fails while a
	// legacy key is present — never fall through to generateKey and mint a
	// SECOND identity beside a CA-registered one, which would orphan the node's
	// issued mTLS leaf. No-op for a non-convergent store (legacyDir == "").
	if err := s.reconcileFromLegacy(); err != nil {
		return nil, err
	}
	if s.HasKey() {
		return s.loadKey()
	}
	return s.generateKey()
}

// reconcileFromLegacy performs the one-time read-through that converges a
// node's cryptographic identity onto this (machine-convergent) store from the
// legacy invoker-scoped location a pre-convergence citadel wrote it to
// (platform.ConfigDir()/identity — see Convergent). It is the mechanism the
// K-A design (docs/design-trust-receipt-v2.md §4, aceteam#8253) requires so the
// AEP receipt signer and the CSR/enrollment flows use ONE key — the key whose
// SPKI the fabric CA already signed — instead of two.
//
// No-op for a non-convergent store (legacyDir == "": New/Default).
//
// Decision (invariants: never displace a REGISTERED key, never destroy key
// material). node.crt (the CA-signed leaf) is the local proof that a key was
// registered: pre-K-A only the legacy Default() store ever received a leaf
// (device pairing wrote it), while the convergent path only ever received a
// bare AEP signer key (#917, no leaf), so a convergent leaf can only mean a
// post-K-A registration here.
//   - convergent has BOTH key and leaf → registered here already; FINAL. Never
//     touched, even if a differing legacy key exists.
//   - legacy key exists, convergent has no leaf, and the convergent key is
//     absent OR differs from the legacy key → ADOPT the legacy key (and its
//     leaf + CA chain, if any). A pre-existing (leafless, therefore
//     unregistered) convergent key being displaced is first copied aside to
//     node.key.displaced-<unixnano> (never removed), then the legacy key is
//     installed via atomic temp-write + rename, so node.key is never
//     momentarily absent.
//   - otherwise (no legacy key, or convergent key already equals legacy) →
//     nothing to adopt.
//
// Idempotent (a second call sees a convergent leaf, or convergent == legacy)
// and crash-safe (every write is an atomic temp-file + rename). Safe across
// processes: two concurrent adopters copy IDENTICAL legacy bytes, so a rename
// that overwrites the other's install is benign.
func (s *Store) reconcileFromLegacy() error {
	if s.legacyDir == "" {
		return nil
	}
	// A convergent key WITH a leaf is a post-K-A registration here: final.
	if s.HasKey() && s.HasLeaf() {
		return nil
	}

	legacyKey, err := readFileIfExists(filepath.Join(s.legacyDir, keyFileName))
	if err != nil {
		return fmt.Errorf("read legacy identity key: %w", err)
	}
	if legacyKey == nil {
		return nil // nothing registered at the legacy location to adopt
	}

	convKey, err := readFileIfExists(s.KeyPath())
	if err != nil {
		return fmt.Errorf("read convergent identity key: %w", err)
	}

	if err := os.MkdirAll(s.dir, dirPerms); err != nil {
		return fmt.Errorf("create identity dir: %w", err)
	}

	// Bring the key into agreement unless it already matches. A differing
	// convergent key (leafless, so never CA-registered — see above) is copied
	// aside, never destroyed, before the registered legacy key overwrites it.
	if convKey == nil || !bytes.Equal(convKey, legacyKey) {
		if convKey != nil {
			aside := fmt.Sprintf("%s.displaced-%d", s.KeyPath(), time.Now().UnixNano())
			if err := writeFileAtomic(aside, convKey, keyPerms); err != nil {
				return fmt.Errorf("preserve displaced identity key: %w", err)
			}
		}
		// Mandatory: the byte-identical preservation of the registered key
		// (same bytes → same SPKI → the already-issued leaf stays valid) is the
		// entire point of the read-through.
		if err := writeFileAtomic(s.KeyPath(), legacyKey, keyPerms); err != nil {
			return fmt.Errorf("adopt legacy identity key: %w", err)
		}
	}

	// Adopt the leaf + CA chain too when present at the legacy location and
	// absent here — public material, best-effort — so an already-paired node's
	// `citadel device status` display and its mTLS leaf keep working.
	for _, f := range []struct{ src, dst string }{
		{filepath.Join(s.legacyDir, leafFileName), s.LeafPath()},
		{filepath.Join(s.legacyDir, caChainFileName), s.CAChainPath()},
	} {
		if fileExists(f.dst) {
			continue
		}
		if data, rErr := readFileIfExists(f.src); rErr == nil && data != nil {
			_ = writeFileAtomic(f.dst, data, pubPerms)
		}
	}
	return nil
}

// readFileIfExists returns the file's bytes, or (nil, nil) if it does not
// exist. A real read error (permissions, I/O) is returned.
func readFileIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// fileExists reports whether path exists (as any file type).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeFileAtomic writes data to path via a temp file in the SAME directory
// (created 0600, so private-key bytes never transit a looser-perm file) chmod'd
// to perm, then an atomic rename. Crash-safe: a crash leaves either the old
// file or the fully-written new one, never a partial key.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// generateKey creates a new EC P-256 keypair and writes the private key to
// disk with 0600 permissions. The private key bytes never leave this function
// except to be written to the owner-only file.
func (s *Store) generateKey() (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(s.dir, dirPerms); err != nil {
		return nil, fmt.Errorf("create identity dir: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate EC P-256 key: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeECPrivateKey, Bytes: der})

	// Write 0600 explicitly; also chmod in case the file already existed with
	// looser perms (umask can widen the WriteFile mode).
	if err := os.WriteFile(s.KeyPath(), keyPEM, keyPerms); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}
	if err := os.Chmod(s.KeyPath(), keyPerms); err != nil {
		return nil, fmt.Errorf("chmod private key: %w", err)
	}
	return key, nil
}

// loadKey reads and parses the persisted private key.
func (s *Store) loadKey() (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(s.KeyPath())
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("private key file is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not an EC key (got %T)", parsed)
	}
	return key, nil
}

// GenerateCSR produces a PEM-encoded PKCS#10 certificate signing request
// carrying the node's public key. The Subject is intentionally minimal: the
// backend fabric CA uses ONLY the CSR's public key and assigns identity
// (node_uid / org_id) server-side, so the Subject/SAN are ignored (P1 of
// #4583). The private key is used only to sign the CSR and is never included
// in the output.
func (s *Store) GenerateCSR(key *ecdsa.PrivateKey) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("nil private key")
	}
	tmpl := &x509.CertificateRequest{
		// Minimal subject; backend ignores it. A CN of "citadel-node" is purely
		// cosmetic for anyone inspecting the CSR.
		Subject:            pkix.Name{CommonName: "citadel-node"},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCSR, Bytes: der}), nil
}

// StoreLeaf persists the CA-signed leaf certificate and the CA trust chain
// returned by the pairing flow. Both are public material, written with normal
// (0644) perms. Either argument may be empty (back-compat: an older backend or
// an un-activated CA returns no cert) in which case the corresponding file is
// left untouched and no error is returned.
//
// leafPEM and chainPEM are validated as parseable PEM certificate(s) before
// being written; malformed input is rejected rather than silently stored.
func (s *Store) StoreLeaf(leafPEM, chainPEM string) error {
	if leafPEM == "" && chainPEM == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, dirPerms); err != nil {
		return fmt.Errorf("create identity dir: %w", err)
	}
	if leafPEM != "" {
		if err := validateCertPEM(leafPEM); err != nil {
			return fmt.Errorf("leaf cert: %w", err)
		}
		if err := os.WriteFile(s.LeafPath(), []byte(leafPEM), pubPerms); err != nil {
			return fmt.Errorf("write leaf cert: %w", err)
		}
	}
	if chainPEM != "" {
		if err := validateCertPEM(chainPEM); err != nil {
			return fmt.Errorf("ca chain: %w", err)
		}
		if err := os.WriteFile(s.CAChainPath(), []byte(chainPEM), pubPerms); err != nil {
			return fmt.Errorf("write ca chain: %w", err)
		}
	}
	return nil
}

// StoreCAChain persists just the CA trust chain (e.g. fetched from
// GET /api/fabric/ca/chain). Public material, 0644. Empty input is a no-op.
func (s *Store) StoreCAChain(chainPEM string) error {
	if chainPEM == "" {
		return nil
	}
	if err := validateCertPEM(chainPEM); err != nil {
		return fmt.Errorf("ca chain: %w", err)
	}
	if err := os.MkdirAll(s.dir, dirPerms); err != nil {
		return fmt.Errorf("create identity dir: %w", err)
	}
	if err := os.WriteFile(s.CAChainPath(), []byte(chainPEM), pubPerms); err != nil {
		return fmt.Errorf("write ca chain: %w", err)
	}
	return nil
}

// LoadLeaf reads and parses the stored leaf certificate.
func (s *Store) LoadLeaf() (*x509.Certificate, error) {
	// Best-effort read-through so an already-paired node whose leaf still lives
	// at the legacy location is displayed/loaded correctly. Unlike
	// GetOrCreateKey this does NOT hard-fail on a reconcile error — reading a
	// leaf is a display/dormant path, never a key-minting decision.
	if s.legacyDir != "" {
		_ = s.reconcileFromLegacy()
	}
	data, err := os.ReadFile(s.LeafPath())
	if err != nil {
		return nil, fmt.Errorf("read leaf cert: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("leaf cert file is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse leaf cert: %w", err)
	}
	return cert, nil
}

// Sign signs the SHA-256 digest of payload with the node's ECDSA P-256
// private key (creating one via GetOrCreateKey if none exists yet) and
// returns an ASN.1 DER-encoded signature.
//
// Added for citadel-cli's signed AEP receipt (aceteam #8253, the deferred
// signing half of internal/trust's grounding guardrail) — a second consumer
// of this package's key alongside the mTLS CSR/leaf flow it was originally
// built for (#4583). See docs/design-node-identity-receipts.md §1c/§1d for
// why this key (unattended-capable, no PIN gate) was chosen over
// internal/nodevault's Session/DeriveSubkey (symmetric, PIN-gated, cannot
// sign unattended under a headless `citadel work`).
func (s *Store) Sign(payload []byte) ([]byte, error) {
	key, err := s.GetOrCreateKey()
	if err != nil {
		return nil, fmt.Errorf("get or create signing key: %w", err)
	}
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign payload: %w", err)
	}
	return sig, nil
}

// PublicKeyFingerprint returns a stable identifier for the node's public key
// — sha256 of its DER-encoded SubjectPublicKeyInfo, hex-encoded and prefixed
// "sha256:" — that a verifier can look up a registered public key by without
// needing the private key or a full certificate. Deterministic: the same
// on-disk key always yields the same fingerprint, and (like Sign) creates a
// key via GetOrCreateKey if none exists yet.
func (s *Store) PublicKeyFingerprint() (string, error) {
	key, err := s.GetOrCreateKey()
	if err != nil {
		return "", fmt.Errorf("get or create signing key: %w", err)
	}
	return FingerprintPublicKey(&key.PublicKey)
}

// FingerprintPublicKey computes the stable "sha256:<hex>" fingerprint of an
// arbitrary ECDSA public key — sha256 over its DER-encoded SubjectPublicKeyInfo,
// the IDENTICAL derivation Store.PublicKeyFingerprint applies to the node's own
// key. Exposed as a free function so a verifier that holds only a public key
// (e.g. `citadel aep verify` given a --pubkey or --cert) computes the SAME
// fingerprint the signer wrote into the receipt, and so both sides can never
// drift because there is only one derivation.
func FingerprintPublicKey(pub *ecdsa.PublicKey) (string, error) {
	if pub == nil {
		return "", fmt.Errorf("nil public key")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// PublicKey returns the node identity key's public half by LOADING the persisted
// key — it never generates one. Unlike Sign/PublicKeyFingerprint (which are on
// the signing path, where minting a key on first use is the point), a verifier
// must not create identity as a side effect: a verify command that silently
// wrote a fresh node.key would leave a leafless key reconcileFromLegacy later
// has to displace, and would mask the "no key present" failure a verifier needs
// to report honestly. Mirrors LoadLeaf's read-only, best-effort-reconcile shape:
// it adopts a legacy key if one exists (convergent stores only), then requires a
// key to already be on disk, returning a distinct error naming KeyPath otherwise.
func (s *Store) PublicKey() (*ecdsa.PublicKey, error) {
	// Best-effort read-through so a convergent store adopts an already-registered
	// legacy key instead of reporting it absent. Never hard-fails here — reading a
	// public key is a display/verify path, not a key-minting decision (same
	// posture as LoadLeaf).
	if s.legacyDir != "" {
		_ = s.reconcileFromLegacy()
	}
	if !s.HasKey() {
		return nil, fmt.Errorf("no node identity key at %s", s.KeyPath())
	}
	key, err := s.loadKey()
	if err != nil {
		return nil, err
	}
	return &key.PublicKey, nil
}

// validateCertPEM confirms that data contains at least one parseable X.509
// certificate. It walks every PEM block so a chain (multiple certs) is fully
// validated.
func validateCertPEM(data string) error {
	rest := []byte(data)
	found := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != pemTypeCertificate {
			return fmt.Errorf("unexpected PEM block type %q", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("parse certificate: %w", err)
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("no certificate found in PEM data")
	}
	return nil
}
