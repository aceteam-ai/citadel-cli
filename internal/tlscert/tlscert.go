// Package tlscert provides self-signed TLS certificate generation and management
// for the Citadel gateway. Certificates are generated on first run and reused
// on subsequent starts.
//
// The self-signed approach is the guaranteed-to-work foundation. When Headscale
// supports Tailscale-compatible cert provisioning (ACME via control plane), the
// gateway can switch to tsnet.ListenTLS for automatic Let's Encrypt certs. See
// internal/network/server.go ListenTLS for that path.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const (
	certFileName = "server.crt"
	keyFileName  = "server.key"
	certValidity = 365 * 24 * time.Hour // 1 year
)

// Config holds options for certificate generation.
type Config struct {
	// Hostname is the node hostname (included as SAN).
	Hostname string

	// IPAddresses are additional IPs to include as SANs.
	IPAddresses []net.IP

	// CertDir overrides the default certificate directory.
	// If empty, uses the platform-appropriate default.
	CertDir string
}

// certDir returns the directory where TLS certs are stored.
func certDir(override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(platform.ConfigDir(), "tls")
}

// CertPath returns the path to the certificate file.
func CertPath(override string) string {
	return filepath.Join(certDir(override), certFileName)
}

// KeyPath returns the path to the private key file.
func KeyPath(override string) string {
	return filepath.Join(certDir(override), keyFileName)
}

// sansMatch checks that the cert's SANs cover all desired hostnames and IPs.
func sansMatch(leaf *x509.Certificate, cfg Config) bool {
	wantDNS := map[string]bool{"localhost": true}
	if cfg.Hostname != "" {
		wantDNS[cfg.Hostname] = true
	}
	haveDNS := map[string]bool{}
	for _, name := range leaf.DNSNames {
		haveDNS[name] = true
	}
	for name := range wantDNS {
		if !haveDNS[name] {
			return false
		}
	}
	haveIP := map[string]bool{}
	for _, ip := range leaf.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, ip := range cfg.IPAddresses {
		if !haveIP[ip.String()] {
			return false
		}
	}
	return true
}

// EnsureCert loads an existing cert+key pair, or generates a new self-signed
// pair if none exists. Returns the tls.Certificate ready for use.
func EnsureCert(cfg Config) (tls.Certificate, error) {
	dir := certDir(cfg.CertDir)

	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	// Try loading existing cert
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, parseErr := x509.ParseCertificate(cert.Certificate[0]); parseErr == nil {
			if time.Now().Before(leaf.NotAfter) && sansMatch(leaf, cfg) {
				return cert, nil
			}
		}
	}

	// Generate new self-signed cert
	return generateAndStore(cfg, dir, certPath, keyPath)
}

// generateAndStore creates a new self-signed certificate and writes it to disk.
func generateAndStore(cfg Config, dir, certPath, keyPath string) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return tls.Certificate{}, fmt.Errorf("create tls dir: %w", err)
	}

	// Generate ECDSA P-256 key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"AceTeam Citadel"},
			CommonName:   "citadel-node",
		},
		NotBefore:             now,
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// Add SANs
	template.DNSNames = []string{"localhost"}
	if cfg.Hostname != "" {
		template.DNSNames = append(template.DNSNames, cfg.Hostname)
	}
	template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"), net.IPv6loopback)
	for _, ip := range cfg.IPAddresses {
		template.IPAddresses = append(template.IPAddresses, ip)
	}

	// Self-sign
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}

	// Encode to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Write to disk (key first, restrictive permissions)
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return tls.Certificate{}, fmt.Errorf("write cert: %w", err)
	}

	// Parse back as tls.Certificate
	return tls.X509KeyPair(certPEM, keyPEM)
}
