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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

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
// The zero value is not usable; construct one with New or Default.
type Store struct {
	dir string
}

// Default returns a Store rooted at platform.ConfigDir()/identity, matching the
// location and pattern citadel already uses for config.yaml and the device
// token (see cmd/init.go, internal/tlscert).
func Default() *Store {
	return &Store{dir: filepath.Join(platform.ConfigDir(), dirName)}
}

// New returns a Store rooted at an explicit directory. Used in tests to avoid
// touching the real config dir.
func New(dir string) *Store {
	return &Store{dir: dir}
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
	if s.HasKey() {
		return s.loadKey()
	}
	return s.generateKey()
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
