// mTLS self-heal client: present the fabric leaf to the nexus reenroll
// service and receive a fresh org authkey (#5959, reusing #4583's machinery).
//
// The security model is proof-of-possession: the leaf PEM is public, so the
// only thing that authenticates this call is the TLS client-auth handshake —
// the caller must sign with the private key matching the leaf. nginx on nexus
// verifies the chain at the handshake and forwards the verified cert to the
// keyless verifier, which gates on grace + CRL and mints the authkey.
package devicemode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

// ReenrollResult is the reenroll service's success payload.
type ReenrollResult struct {
	OK       bool   `json:"ok"`
	Authkey  string `json:"authkey"`
	NexusURL string `json:"nexus_url"`
	NodeUID  string `json:"node_uid"`
}

// LoadClientCertificate assembles the TLS client certificate from the
// identity store's leaf + private key. It fails with a pointed error when
// either half is missing (device not enrolled yet).
func LoadClientCertificate(store *nodeidentity.Store) (tls.Certificate, error) {
	if !store.HasKey() || !store.HasLeaf() {
		return tls.Certificate{}, fmt.Errorf(
			"no fabric identity found in %s — run 'citadel device enroll' first", store.Dir())
	}
	cert, err := tls.LoadX509KeyPair(store.LeafPath(), store.KeyPath())
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load fabric identity: %w", err)
	}
	return cert, nil
}

// Reenroll POSTs to the mTLS reenroll endpoint with the device's leaf as the
// TLS client certificate and returns the fresh org authkey. The endpoint's
// server certificate is verified against the system roots (nexus serves a
// public TLS cert).
func Reenroll(ctx context.Context, reenrollURL string, clientCert tls.Certificate) (*ReenrollResult, error) {
	return reenrollWithRoots(ctx, reenrollURL, clientCert, nil)
}

// reenrollWithRoots is the testable core: rootCAs == nil means system roots.
func reenrollWithRoots(
	ctx context.Context,
	reenrollURL string,
	clientCert tls.Certificate,
	rootCAs *x509.CertPool,
) (*ReenrollResult, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{clientCert},
				RootCAs:      rootCAs,
				MinVersion:   tls.VersionTLS12,
			},
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reenrollURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build reenroll request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reenroll request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read reenroll response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, reenrollError(resp.StatusCode, body)
	}

	var result ReenrollResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse reenroll response: %w", err)
	}
	if result.Authkey == "" {
		return nil, fmt.Errorf("reenroll succeeded but returned no authkey")
	}
	return &result, nil
}

// reenrollError maps the reenroll service's documented status codes to
// operator-actionable errors (mirrors nexus reenroll/app.py's error table).
func reenrollError(status int, body []byte) error {
	detail := strings.TrimSpace(string(body))
	switch status {
	case http.StatusForbidden:
		return fmt.Errorf(
			"reenroll refused (HTTP 403 — certificate revoked or past grace): %s\n"+
				"Re-enroll this device with 'citadel device enroll'", detail)
	case http.StatusUnauthorized:
		return fmt.Errorf(
			"reenroll refused (HTTP 401 — certificate not trusted by the fabric CA): %s", detail)
	case http.StatusServiceUnavailable:
		return fmt.Errorf(
			"reenroll unavailable (HTTP 503 — revocation status unavailable, fail-closed); "+
				"will retry: %s", detail)
	default:
		return fmt.Errorf("reenroll returned HTTP %d: %s", status, detail)
	}
}
