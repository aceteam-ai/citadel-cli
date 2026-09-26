package cmd

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"gopkg.in/yaml.v3"
)

// CSR-enrolled interactive device-grant login (citadel-cli#1062, companion to
// the draft platform aceteam#9576). This file holds the PURE, hermetically
// testable core: CSR generation from the machine-convergent identity store, the
// fail-closed bundle validation/persist decision, and the deterministic serving
// identity name. cmd/login.go wires these into the interactive device flow; no
// function here calls network.GetNodeConfigDir() directly (the store is
// constructed by the caller / the loginIdentityStore seam), so a test drives
// them against a t.TempDir()-rooted store without touching the real node.

// loginIdentityStore constructs the identity store for CSR-enrolled login. It
// is the SAME nodeidentity.Convergent(network.GetNodeConfigDir()) store the AEP
// receipt signer (internal/worker.defaultAEPSigner) and the CSR/enroll paths
// (cmd/init.go:ensureNodeIdentity, cmd/device.go) construct, so the login CSR
// key, the receipt-signing key, and the enrollment key are ONE key (K-A,
// docs/design-trust-receipt-v2.md §4). Reuses the nodeConfigDirFn seam (defined
// in cmd/init.go) so the wiring is swappable/assertable without I/O.
var loginIdentityStore = func() *nodeidentity.Store {
	return nodeidentity.Convergent(nodeConfigDirFn())
}

// nodeUIDPattern bounds a server-assigned node_uid to a conservative,
// serving-name-safe charset before it is used to derive the deterministic
// Headscale givenName "node-"+uid. Rejects whitespace/control chars (log &
// header-injection safety) and anything the verifier's exact-match rule could
// not round-trip.
var nodeUIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// loginBundleOutcome is the result of processing a CSR-enrollment bundle.
type loginBundleOutcome struct {
	// NodeUID is the validated server-assigned fabric node id, empty when no
	// bundle was returned. When non-empty it drives the deterministic serving
	// identity ("node-"+NodeUID) for this login.
	NodeUID string
	// Persisted is true only when a NEW leaf certificate was written on this
	// call (false for the no-bundle inert path and for the never-overwrite
	// re-login idempotence path).
	Persisted bool
}

// generateLoginCSR loads-or-creates the node's convergent identity key and
// derives ONE PEM CSR from it. It returns the CSR plus the key's PUBLIC half so
// the caller can bind the returned leaf to the EXACT key that signed the CSR
// (no second on-disk read that could race reconcileFromLegacy mid-flow). The
// private key never leaves the store.
func generateLoginCSR(store *nodeidentity.Store) (csrPEM string, pub *ecdsa.PublicKey, err error) {
	key, err := store.GetOrCreateKey()
	if err != nil {
		return "", nil, fmt.Errorf("node identity key: %w", err)
	}
	der, err := store.GenerateCSR(key)
	if err != nil {
		return "", nil, fmt.Errorf("generate CSR: %w", err)
	}
	return string(der), &key.PublicKey, nil
}

// persistLoginIdentityBundle validates and (when appropriate) persists the
// leaf/chain/node_uid a CSR-enrolled device-grant login received, FAILING
// CLOSED on anything ambiguous. csrPub is the public key of the CSR this login
// actually submitted; a nil csrPub means NO CSR was sent, in which case any
// returned bundle is rejected outright (a leaf we never requested is not one to
// trust). Rules:
//
//   - No bundle at all (no leaf, no node_uid): inert no-op — the universal case
//     today (legacy backend / un-activated CA). Returns a zero outcome, nil err.
//   - Bundle present but csrPub == nil: rejected (bundle without a submitted CSR).
//   - Partial bundle (leaf xor node_uid): rejected.
//   - node_uid malformed: rejected.
//   - Leaf not bound to csrPub: rejected (the CA signed a different key).
//   - Malformed chain (when present): rejected.
//   - A known-good leaf already on disk: NOT overwritten (re-login idempotence);
//     still returns the validated NodeUID so the serving identity is applied.
//   - Otherwise: StoreLeaf(leaf, chain), Persisted=true.
//
// The issuer returns all three fields together; a missing chain is incomplete.
func persistLoginIdentityBundle(store *nodeidentity.Store, csrPub *ecdsa.PublicKey, leafPEM, chainPEM, nodeUID string) (loginBundleOutcome, error) {
	leafPEM = strings.TrimSpace(leafPEM)
	chainPEM = strings.TrimSpace(chainPEM)
	nodeUID = strings.TrimSpace(nodeUID)

	hasLeaf := leafPEM != ""
	hasUID := nodeUID != ""

	// No bundle: the inert path (backend not merged, or CA not activated).
	if !hasLeaf && !hasUID && chainPEM == "" {
		return loginBundleOutcome{}, nil
	}

	// A bundle we never asked for: refuse to trust a leaf when this login sent
	// no CSR at all.
	if csrPub == nil {
		return loginBundleOutcome{}, fmt.Errorf("server returned an identity bundle but no CSR was submitted; refusing to enroll")
	}

	// Fail closed on anything but a complete bundle: leaf AND node_uid are both
	// load-bearing (leaf → the cert we persist; node_uid → the serving
	// identity). This also catches a chain-only response (no leaf, no uid).
	if !hasLeaf || !hasUID || chainPEM == "" {
		return loginBundleOutcome{}, fmt.Errorf("incomplete identity bundle from server; refusing to enroll")
	}

	if !nodeUIDPattern.MatchString(nodeUID) {
		return loginBundleOutcome{}, fmt.Errorf("identity bundle node id is malformed; refusing to enroll")
	}

	// The returned leaf must be bound to the key that signed the submitted CSR.
	if err := leafBoundToKey(leafPEM, csrPub); err != nil {
		return loginBundleOutcome{}, err
	}
	if err := leafBoundToUID(leafPEM, nodeUID); err != nil {
		return loginBundleOutcome{}, err
	}
	if err := leafValidNow(leafPEM); err != nil {
		return loginBundleOutcome{}, err
	}

	// Validate the complete issuer chain before the never-overwrite shortcut.
	if err := validateChainPEM(leafPEM, chainPEM); err != nil {
		return loginBundleOutcome{}, fmt.Errorf("identity bundle chain: %w", err)
	}

	// An existing cert is normally never overwritten. If it is bound to our key
	// AND uid it is this node's own certificate; if it is NOT, it is stale or
	// foreign and we refuse rather than silently keep serving with a mismatched
	// identity (and do NOT return the NodeUID, which would imply enrollment).
	if store.HasLeaf() {
		existing, rErr := os.ReadFile(store.LeafPath())
		if rErr != nil {
			return loginBundleOutcome{}, fmt.Errorf("read existing identity certificate: %w", rErr)
		}
		if err := leafBoundToKey(string(existing), csrPub); err != nil {
			return loginBundleOutcome{}, fmt.Errorf("existing identity certificate is not bound to this node's key; refusing to proceed")
		}
		if err := leafBoundToUID(string(existing), nodeUID); err != nil {
			return loginBundleOutcome{}, fmt.Errorf("existing identity certificate has a different node id; refusing to proceed")
		}
		// Same key + same uid means this is our own cert. If it is outside its
		// validity window (an EXPIRED leaf once the backend's leaf TTL elapses),
		// the freshly issued, already-validated bundle above is a RENEWAL: store
		// it. Without this a node whose leaf expired could never renew through
		// login and would stay unverified until node.crt was deleted by hand.
		if leafValidNow(string(existing)) != nil {
			if err := store.StoreLeaf(leafPEM, chainPEM); err != nil {
				return loginBundleOutcome{}, fmt.Errorf("store renewed identity certificate: %w", err)
			}
			return loginBundleOutcome{NodeUID: nodeUID, Persisted: true}, nil
		}
		// The existing leaf is still valid: idempotent re-login. Leave it
		// untouched, but still require its on-disk chain to be complete so the
		// serving identity resolves cleanly on the next reconnect.
		chain, cErr := os.ReadFile(store.CAChainPath())
		if cErr != nil || validateChainPEM(string(existing), string(chain)) != nil {
			return loginBundleOutcome{}, fmt.Errorf("existing identity certificate chain is incomplete; refusing to proceed")
		}
		return loginBundleOutcome{NodeUID: nodeUID}, nil
	}

	if err := store.StoreLeaf(leafPEM, chainPEM); err != nil {
		return loginBundleOutcome{}, fmt.Errorf("store identity certificate: %w", err)
	}
	return loginBundleOutcome{NodeUID: nodeUID, Persisted: true}, nil
}

// leafBoundToKey confirms leafPEM parses as a single X.509 certificate whose
// public key equals pub. Mirrors internal/devicemode/renew.go:parseLeafForKey —
// a mismatched cert is worse than none, so it is rejected rather than stored.
func leafBoundToKey(leafPEM string, pub *ecdsa.PublicKey) error {
	block, rest := pem.Decode([]byte(leafPEM))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return fmt.Errorf("identity bundle leaf is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse identity bundle leaf: %w", err)
	}
	leafPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub == nil || !leafPub.Equal(pub) {
		return fmt.Errorf("identity bundle leaf is not bound to this node's key; refusing to store it")
	}
	return nil
}

func leafBoundToUID(leafPEM, nodeUID string) error {
	block, _ := pem.Decode([]byte(leafPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.Subject.CommonName != nodeUID {
		return fmt.Errorf("identity bundle leaf node id does not match response")
	}
	for _, uri := range cert.URIs {
		if uri.String() == "aceteam:node:"+nodeUID {
			return nil
		}
	}
	return fmt.Errorf("identity bundle leaf node id does not match response")
}

func leafValidNow(leafPEM string) error {
	block, _ := pem.Decode([]byte(leafPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
		return fmt.Errorf("identity bundle leaf is outside its validity window")
	}
	return nil
}

// validateChainPEM confirms data is one or more parseable X.509 certificates.
func validateChainPEM(leafPEM, data string) error {
	rest := []byte(data)
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		before := rest
		block, rest = pem.Decode(rest)
		if block == nil {
			if len(strings.TrimSpace(string(rest))) != 0 {
				return fmt.Errorf("unexpected data in chain PEM")
			}
			break
		}
		if !strings.HasPrefix(strings.TrimSpace(string(before)), "-----BEGIN CERTIFICATE-----") {
			return fmt.Errorf("unexpected data in chain PEM")
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("unexpected PEM block type %q", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) != 3 {
		return fmt.Errorf("expected leaf, intermediate, and root certificates")
	}
	leafBlock, _ := pem.Decode([]byte(leafPEM))
	if leafBlock == nil || string(certs[0].Raw) != string(leafBlock.Bytes) {
		return fmt.Errorf("chain leaf does not match bundle leaf")
	}
	if !certs[1].IsCA || !certs[2].IsCA || certs[0].CheckSignatureFrom(certs[1]) != nil || certs[1].CheckSignatureFrom(certs[2]) != nil || certs[2].CheckSignatureFrom(certs[2]) != nil {
		return fmt.Errorf("invalid certificate chain")
	}
	return nil
}

// saveLoginNodeUID records login enrollment independently of device-mode's
// device.json. The shared config is machine-convergent across login and work.
func saveLoginNodeUID(nodeUID string) error {
	if nodeUID == "" {
		return fmt.Errorf("invalid login node id")
	}
	return persistLoginNodeUID(nodeUID)
}

// clearLoginNodeUID prevents a later authkey or unenrolled device login from
// reconnecting under the serving name of a previous CSR-enrolled login.
func clearLoginNodeUID() error {
	return persistLoginNodeUID("")
}

func persistLoginNodeUID(nodeUID string) error {
	if nodeUID != "" && !nodeUIDPattern.MatchString(nodeUID) {
		return fmt.Errorf("invalid login node id")
	}
	path := filepath.Join(nodeConfigDirFn(), "config.yaml")
	config := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("read login node config: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if nodeUID == "" {
		return nil
	}
	if config == nil {
		config = map[string]interface{}{}
	}
	if nodeUID == "" {
		if _, present := config["login_node_uid"]; !present {
			return nil
		}
		delete(config, "login_node_uid")
	} else {
		config["login_node_uid"] = nodeUID
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fixStatePermissionsFn()
	return nil
}

// loadLoginNodeUID only trusts the persisted UID while the local leaf remains
// bound to the convergent key. A missing or replaced cert cannot claim it.
func loadLoginNodeUID() string {
	data, err := os.ReadFile(filepath.Join(nodeConfigDirFn(), "config.yaml"))
	if err != nil {
		return ""
	}
	var config struct {
		LoginNodeUID string `yaml:"login_node_uid"`
	}
	if yaml.Unmarshal(data, &config) != nil || !nodeUIDPattern.MatchString(config.LoginNodeUID) {
		return ""
	}
	store := loginIdentityStore()
	pub, err := store.PublicKey()
	if err != nil {
		return ""
	}
	leaf, err := os.ReadFile(store.LeafPath())
	if err != nil || leafBoundToKey(string(leaf), pub) != nil || leafBoundToUID(string(leaf), config.LoginNodeUID) != nil || leafValidNow(string(leaf)) != nil {
		return ""
	}
	chain, err := os.ReadFile(store.CAChainPath())
	if err != nil || validateChainPEM(string(leaf), string(chain)) != nil {
		return ""
	}
	return config.LoginNodeUID
}

// servingIdentityHostname is the deterministic Headscale givenName a login node
// registers under. A CSR-enrolled node (validated nodeUID) MUST serve under
// "node-"+nodeUID so the verifier's headscale_node_name(uid) == "node-"+uid rule
// matches; the user's display hostname is kept separately by the caller. With no
// nodeUID (every login today) it falls back to displayHostname — byte-identical
// to pre-#1062 behavior.
func servingIdentityHostname(nodeUID, displayHostname string) string {
	if nodeUID != "" {
		return "node-" + nodeUID
	}
	return displayHostname
}
