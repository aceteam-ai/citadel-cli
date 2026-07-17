// Leaf renewal client: refresh the fabric identity certificate BEFORE it
// expires, so a device never has to fall back to the interactive enrollment
// ceremony (aceteam #5959 follow-up; platform endpoint POST /api/fabric/ca/renew).
//
// The security model is application-layer proof-of-possession: the platform
// coordinator cannot terminate client-cert TLS, so instead of an mTLS
// handshake the request carries a CSR self-signed by the SAME key as the
// current leaf. Only the holder of the leaf's private key can produce that
// CSR, and the server verifies chain + revocation + key equality before
// issuing a successor leaf with the same identity and lifetime class. The
// private key never leaves the machine and is NOT rotated — the identity
// store keeps one stable key, which is also what makes the CSR a possession
// proof.
package devicemode

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

type renewRequest struct {
	LeafPem string `json:"leaf_pem"`
	CSRPem  string `json:"csr_pem"`
}

type renewResponse struct {
	OK       bool   `json:"ok"`
	LeafPem  string `json:"leaf_pem"`
	ChainPem string `json:"chain_pem"`
	NodeUID  string `json:"node_uid"`
	NotAfter string `json:"not_after"`
}

// RenewLeaf exchanges the current (still valid) leaf + a CSR for a successor
// leaf and persists it to the identity store. Returns the new expiry.
//
// The returned leaf is verified locally before it replaces the old one: it
// must parse and its public key must match our private key — a response that
// fails either check is rejected so we never overwrite a working identity
// with an unusable cert.
func RenewLeaf(
	ctx context.Context,
	client *http.Client,
	apiBaseURL string,
	store *nodeidentity.Store,
) (time.Time, error) {
	if !store.HasKey() || !store.HasLeaf() {
		return time.Time{}, fmt.Errorf(
			"no fabric identity found in %s — run 'citadel device enroll' first", store.Dir())
	}
	key, err := store.GetOrCreateKey() // key exists (checked above): loads, never creates
	if err != nil {
		return time.Time{}, fmt.Errorf("load identity key: %w", err)
	}
	leafPEM, err := os.ReadFile(store.LeafPath())
	if err != nil {
		return time.Time{}, fmt.Errorf("read current leaf: %w", err)
	}
	csrPEM, err := store.GenerateCSR(key)
	if err != nil {
		return time.Time{}, fmt.Errorf("generate renewal CSR: %w", err)
	}

	payload, err := json.Marshal(renewRequest{LeafPem: string(leafPEM), CSRPem: string(csrPEM)})
	if err != nil {
		return time.Time{}, fmt.Errorf("marshal renewal request: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		strings.TrimSuffix(apiBaseURL, "/")+"/api/fabric/ca/renew",
		bytes.NewReader(payload),
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("build renewal request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("renewal request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return time.Time{}, fmt.Errorf("read renewal response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, renewError(resp.StatusCode, body)
	}

	var result renewResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return time.Time{}, fmt.Errorf("parse renewal response: %w", err)
	}
	newLeaf, err := parseLeafForKey(result.LeafPem, key)
	if err != nil {
		return time.Time{}, err
	}
	if err := store.StoreLeaf(result.LeafPem, result.ChainPem); err != nil {
		return time.Time{}, fmt.Errorf("store renewed leaf: %w", err)
	}
	return newLeaf.NotAfter, nil
}

// parseLeafForKey validates that a returned leaf PEM parses and is bound to
// OUR private key. A mismatched cert would be worse than none: it would
// silently replace a working identity with one we cannot use.
func parseLeafForKey(leafPEM string, key *ecdsa.PrivateKey) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(leafPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("renewal response leaf is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse renewed leaf: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("renewed leaf is not bound to this device's key; refusing to store it")
	}
	return cert, nil
}

// renewError maps the renewal endpoint's documented status codes to
// operator-actionable errors.
func renewError(status int, body []byte) error {
	detail := strings.TrimSpace(string(body))
	switch status {
	case http.StatusForbidden:
		return fmt.Errorf(
			"renewal refused (HTTP 403 — certificate expired, revoked, or possession not proven): %s\n"+
				"If the certificate has expired, re-enroll with 'citadel device enroll'", detail)
	case http.StatusUnauthorized:
		return fmt.Errorf(
			"renewal refused (HTTP 401 — certificate not recognized by the platform): %s", detail)
	case http.StatusTooManyRequests:
		return fmt.Errorf("renewal rate-limited (HTTP 429); will retry later: %s", detail)
	case http.StatusServiceUnavailable:
		return fmt.Errorf(
			"renewal unavailable (HTTP 503 — certificate authority not active); will retry: %s", detail)
	default:
		return fmt.Errorf("renewal returned HTTP %d: %s", status, detail)
	}
}
