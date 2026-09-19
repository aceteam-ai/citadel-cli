package cmd

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
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
// chain_pem is treated as optional: leaf_pem + node_uid are the load-bearing
// pair (the cert to persist, and the serving identity). This is a deliberate
// interpretation of the draft platform contract (documented in the PR).
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
	if !hasLeaf || !hasUID {
		return loginBundleOutcome{}, fmt.Errorf("incomplete identity bundle from server; refusing to enroll")
	}

	if !nodeUIDPattern.MatchString(nodeUID) {
		return loginBundleOutcome{}, fmt.Errorf("identity bundle node id is malformed; refusing to enroll")
	}

	// The returned leaf must be bound to the key that signed the submitted CSR.
	if err := leafBoundToKey(leafPEM, csrPub); err != nil {
		return loginBundleOutcome{}, err
	}

	// Validate the chain (if any) BEFORE the never-overwrite short-circuit so a
	// malformed bundle fails closed regardless of on-disk state.
	if chainPEM != "" {
		if err := validateChainPEM(chainPEM); err != nil {
			return loginBundleOutcome{}, fmt.Errorf("identity bundle chain: %w", err)
		}
	}

	// Never overwrite an existing cert. If it is bound to our key it is
	// known-good — the re-login idempotence case: leave it untouched and still
	// apply the serving identity. If it is NOT bound to our key it is stale or
	// foreign; refuse rather than silently keep serving with a mismatched
	// identity (and do NOT return the NodeUID, which would imply enrollment).
	if store.HasLeaf() {
		existing, rErr := os.ReadFile(store.LeafPath())
		if rErr != nil {
			return loginBundleOutcome{}, fmt.Errorf("read existing identity certificate: %w", rErr)
		}
		if err := leafBoundToKey(string(existing), csrPub); err != nil {
			return loginBundleOutcome{}, fmt.Errorf("existing identity certificate is not bound to this node's key; refusing to proceed")
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
	block, _ := pem.Decode([]byte(leafPEM))
	if block == nil || block.Type != "CERTIFICATE" {
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

// validateChainPEM confirms data is one or more parseable X.509 certificates.
func validateChainPEM(data string) error {
	rest := []byte(data)
	found := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("unexpected PEM block type %q", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("parse certificate: %w", err)
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("no certificate found in chain PEM")
	}
	return nil
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
