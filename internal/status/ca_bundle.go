// internal/status/ca_bundle.go
// Provisioning of the fabric CA trust bundle that the mTLS control listener
// (issue #5028) verifies coordinator client certs against.
//
// The node needs the PUBLIC fabric CA chain (intermediate || root) on disk so
// NewFabricCAVerifier can build its ClientCAs pool. This file fetches that chain
// once at startup from the coordinator (GET /api/fabric/ca/chain) and caches it
// to ConfigDir()/fabric-ca-bundle.pem.
//
// CRITICAL zero-break property (advisor / #5028): the fetch must NOT be a hard
// dependency for an already-provisioned node. #461 turns the plaintext
// /ssh/authorized-keys path into a 403 stub, so if a transient backend blip made
// startup unable to bring up the :8443 listener, SSH-deploy would be dead until
// the backend recovered AND the node restarted. To avoid that, a fetch failure
// falls back to any previously-cached bundle on disk; only a first-ever cold
// start with no cache leaves the node without a trust root (and it fails closed:
// the control listener stays down and SSH-key injection is refused, never
// silently reverting to VPN-origin trust).
//
// The bundle is read ONCE at startup; a CA rotation requires a node restart to
// re-fetch. That is an acceptable operational constraint for a root of trust.
package status

import (
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultCoordinatorSAN is the SAN URI a node allowlists by default so the
// platform coordinator/relay can invoke the mutating control endpoints (#5028)
// without per-node env configuration. Used when CITADEL_COORDINATOR_SANS is unset.
//
// CROSS-REPO CONTRACT: this literal MUST equal the backend fabric CA's
// coordinator SAN (python-backend utils/fabric_ca/profile.py COORDINATOR_URI =
// "aceteam:coordinator"). The relay mints itself a client leaf carrying this
// exact SAN; if the two drift, the node rejects the coordinator and SSH-deploy
// fails closed. It follows the same aceteam:<role> shape as aceteam:node:<uid> /
// aceteam:org:<org> and round-trips through url.Parse/URL.String unchanged.
const DefaultCoordinatorSAN = "aceteam:coordinator"

// fabricCABundleFilename is the on-disk cache name under ConfigDir().
const fabricCABundleFilename = "fabric-ca-bundle.pem"

// caChainPath is the coordinator API path that serves the public CA chain.
const caChainPath = "/api/fabric/ca/chain"

// EnsureFabricCABundle returns a filesystem path to the public fabric CA chain
// (intermediate || root PEM) for the mTLS control listener's trust root.
//
// It fetches the chain from baseURL+/api/fabric/ca/chain and caches it to
// configDir/fabric-ca-bundle.pem. On any fetch failure (network, non-200, or a
// response that is not valid PEM certificates) it falls back to a previously
// cached bundle if one exists, so a transient backend outage does not disable
// SSH-deploy on an already-provisioned node. It returns an error only when the
// fetch fails AND no cached bundle is available (first-ever cold start offline),
// in which case the caller leaves the control listener disabled (fail closed).
//
// A successfully fetched bundle is only written when it validates, so a bad
// response never clobbers a good cache.
func EnsureFabricCABundle(baseURL, configDir string) (string, error) {
	bundlePath := filepath.Join(configDir, fabricCABundleFilename)

	pemBytes, err := fetchCAChain(baseURL)
	if err != nil {
		if _, statErr := os.Stat(bundlePath); statErr == nil {
			// Reuse the cached bundle from a prior successful fetch.
			return bundlePath, nil
		}
		return "", fmt.Errorf("fetch fabric CA chain and no cached bundle: %w", err)
	}

	if err := writeBundleAtomic(bundlePath, pemBytes); err != nil {
		// Write failed but we still have valid material in hand; if a cache
		// exists, use it rather than failing.
		if _, statErr := os.Stat(bundlePath); statErr == nil {
			return bundlePath, nil
		}
		return "", fmt.Errorf("cache fabric CA bundle: %w", err)
	}
	return bundlePath, nil
}

// fetchCAChain GETs the public CA chain and validates it parses as >=1 cert.
func fetchCAChain(baseURL string) ([]byte, error) {
	url := strings.TrimRight(baseURL, "/") + caChainPath
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, fmt.Errorf("read CA chain body: %w", err)
	}

	// Validate the response is genuinely a PEM chain before caching it, so a
	// gateway error page or empty body can never poison the trust root.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, fmt.Errorf("CA chain response is not valid PEM certificates")
	}
	return body, nil
}

// writeBundleAtomic writes the bundle via a temp file + rename so a concurrent
// reader never observes a partial file. The bundle is PUBLIC (a CA chain), so
// 0644 is appropriate.
func writeBundleAtomic(bundlePath string, pemBytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(bundlePath), 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmpPath := bundlePath + ".tmp"
	if err := os.WriteFile(tmpPath, pemBytes, 0644); err != nil {
		return fmt.Errorf("write temp bundle: %w", err)
	}
	if err := os.Rename(tmpPath, bundlePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename bundle into place: %w", err)
	}
	return nil
}
