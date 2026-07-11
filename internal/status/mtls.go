// internal/status/mtls.go
// Coordinator-identity gate for the status server's MUTATING control endpoints
// (SSH-key injection first and foremost -- issue #5028).
//
// Background (the hole this closes): the status server's mutating endpoints were
// gated by requireVPNOrAuth, which trusts ANY source IP in the Headscale CGNAT
// range 100.64.0.0/10. Because the live mesh ACL is flat, any node on the mesh
// -- including, once multi-tenant, another org's node -- could POST an SSH key to
// /ssh/authorized-keys and take over the host. Mesh origin is not an identity.
//
// The fix: require the caller to present a fabric-CA-signed client certificate
// (a coordinator identity) over mTLS, verified against the node's trusted fabric
// CA bundle, before any mutating control action runs. This reuses the existing
// fabric mTLS PKI (the reenroll service + nginx client-cert termination, and the
// backend's fabric CA that signs node leafs with SAN URIs aceteam:node:<uid> /
// aceteam:org:<org>). It introduces no new bearer token.
//
// FAIL CLOSED is the load-bearing property: a missing CA bundle, an empty
// coordinator allowlist, a caller with no client cert, a cert that does not chain
// to the fabric CA, or a valid fabric cert whose SAN is not an allowlisted
// coordinator identity -- all are refused with no side effects (the SSH key is
// never written).
//
// Why not "any valid fabric cert" (the weaker interim the CA profile makes easy):
// the threat is OTHER orgs' nodes, and every enrolled node holds a valid fabric
// leaf. "Chains to the CA" would still admit a peer org's node and would NOT close
// #5028. So this gate requires an explicitly-configured coordinator identity and
// refuses to construct a verifier without one. See the flagged gap in the PR: the
// fabric CA profile does not yet mint a distinct coordinator/role SAN, so the
// coordinator identity must be configured out-of-band (env) until it does.
package status

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// FabricCAVerifier holds the trusted fabric CA pool used to verify a caller's
// client certificate, plus the set of coordinator SAN URIs permitted to invoke
// mutating control endpoints.
type FabricCAVerifier struct {
	pool            *x509.CertPool
	coordinatorSANs map[string]bool
}

// NewFabricCAVerifier loads the PUBLIC fabric CA bundle (intermediate + root PEM)
// from caBundlePath and builds a verifier that accepts a client cert only when it
// (a) chains to that CA -- enforced at the TLS handshake via ClientCAs -- AND
// (b) carries one of coordinatorSANs as a SAN URI.
//
// Fail-closed by construction: an empty/unreadable/certless bundle, or an empty
// coordinator allowlist, returns an error. The caller MUST then decline to serve
// the mutating control endpoints at all (they stay refused on every listener),
// rather than fall back to a weaker gate.
func NewFabricCAVerifier(caBundlePath string, coordinatorSANs []string) (*FabricCAVerifier, error) {
	if strings.TrimSpace(caBundlePath) == "" {
		return nil, fmt.Errorf("fabric CA bundle path is empty")
	}
	pemBytes, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("read fabric CA bundle %s: %w", caBundlePath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("fabric CA bundle %s contained no valid PEM certificates", caBundlePath)
	}

	sans := make(map[string]bool)
	for _, s := range coordinatorSANs {
		if s = strings.TrimSpace(s); s != "" {
			sans[s] = true
		}
	}
	if len(sans) == 0 {
		return nil, fmt.Errorf(
			"no coordinator identities configured; refusing to gate mutating control " +
				"endpoints on 'any valid fabric cert' (that would admit other orgs' nodes)")
	}

	return &FabricCAVerifier{pool: pool, coordinatorSANs: sans}, nil
}

// ServerTLSConfig returns a tls.Config for the mTLS control listener. It presents
// serverCert to callers and REQUIRES a client cert that chains to the fabric CA
// (RequireAndVerifyClientCert), so any caller without a CA-signed leaf is rejected
// at the handshake before a handler runs.
func (v *FabricCAVerifier) ServerTLSConfig(serverCert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    v.pool,
		MinVersion:   tls.VersionTLS12,
	}
}

// isCoordinator reports whether leaf carries an allowlisted coordinator SAN URI.
// Identity is read ONLY from the CA-signed SAN URIs, never the Subject (which the
// CSR submitter controls).
func (v *FabricCAVerifier) isCoordinator(leaf *x509.Certificate) bool {
	for _, u := range leaf.URIs {
		if u != nil && v.coordinatorSANs[u.String()] {
			return true
		}
	}
	return false
}

// requireCoordinator wraps next so it runs ONLY when the request arrived over a
// verified mTLS handshake carrying a fabric-CA-signed client cert whose SAN URI
// identifies an allowlisted coordinator.
//
// Fail-closed: no TLS, no peer certificate, or a non-coordinator identity -> 403,
// and next never runs (no side effects). Chain-to-CA is already guaranteed by the
// listener's RequireAndVerifyClientCert; this adds the identity (authorization)
// check on top of that authentication.
func (v *FabricCAVerifier) requireCoordinator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeJSONError(w, "coordinator mTLS client certificate required", http.StatusForbidden)
			return
		}
		if !v.isCoordinator(r.TLS.PeerCertificates[0]) {
			writeJSONError(w, "client certificate is not an authorized coordinator identity", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
