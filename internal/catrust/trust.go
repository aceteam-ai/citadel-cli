// Package catrust manages the explicitly supplied TLS trust anchor used by a
// self-hosted Citadel control plane.
package catrust

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// EnvCACert is the environment alternative to the global --ca-cert flag.
	EnvCACert = "CITADEL_CA_CERT"

	maxBundleBytes = 1 << 20
	recordVersion  = 1
	recordFileName = "control-ca.json"

	managedAuthOrigin  = "https://aceteam.ai"
	managedNexusOrigin = "https://nexus.aceteam.ai"
)

type material struct {
	pem       []byte
	certs     []*x509.Certificate
	fromStore bool
}

type record struct {
	Version     int    `json:"version"`
	AuthOrigin  string `json:"auth_origin"`
	NexusOrigin string `json:"nexus_origin"`
	SHA256      string `json:"sha256"`
	PEM         string `json:"pem"`
}

var (
	activeMu sync.RWMutex
	active   *material
)

// Bootstrap installs either the explicitly supplied CA or a persisted CA
// bound to the requested auth and Nexus origins. It must run before any TLS
// request in the process because the Linux system root pool is cached once.
// The bool reports whether private trust was installed.
func Bootstrap(explicitPath, nodeConfigDir, authURL, nexusURL string) (bool, error) {
	if strings.TrimSpace(explicitPath) != "" {
		if err := refuseManagedOrigins(authURL, nexusURL); err != nil {
			return false, err
		}
	}
	var m *material
	var err error
	if strings.TrimSpace(explicitPath) != "" {
		m, err = loadFile(explicitPath)
	} else {
		m, err = loadPersisted(nodeConfigDir, authURL, nexusURL)
	}
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, nil
	}
	if err := installProcessRoots(m.pem, m.certs); err != nil {
		return false, err
	}
	activeMu.Lock()
	active = m
	activeMu.Unlock()
	return true, nil
}

// ActivePool returns a custom-only pool for consumers, such as Tailscale,
// that already preserve system trust before consulting extra roots.
func ActivePool() *x509.CertPool {
	activeMu.RLock()
	defer activeMu.RUnlock()
	if active == nil {
		return nil
	}
	pool := x509.NewCertPool()
	for _, cert := range active.certs {
		pool.AddCert(cert)
	}
	return pool
}

// PersistActive atomically stores explicitly supplied trust after a successful
// enrollment. A CA loaded from this same store is already durable and is a
// no-op. The binding prevents a private tenant CA from silently applying to a
// different control plane, especially the public production endpoints.
func PersistActive(nodeConfigDir, authURL, nexusURL string) (bool, error) {
	activeMu.RLock()
	if active == nil || active.fromStore {
		activeMu.RUnlock()
		return false, nil
	}
	pemBytes := append([]byte(nil), active.pem...)
	activeMu.RUnlock()
	if err := refuseManagedOrigins(authURL, nexusURL); err != nil {
		return false, err
	}

	authOrigin, err := endpointOrigin(authURL)
	if err != nil {
		return false, fmt.Errorf("invalid auth service URL: %w", err)
	}
	nexusOrigin, err := endpointOrigin(nexusURL)
	if err != nil {
		return false, fmt.Errorf("invalid Nexus URL: %w", err)
	}
	digest := sha256.Sum256(pemBytes)
	r := record{
		Version:     recordVersion,
		AuthOrigin:  authOrigin,
		NexusOrigin: nexusOrigin,
		SHA256:      hex.EncodeToString(digest[:]),
		PEM:         string(pemBytes),
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode private CA record: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Join(nodeConfigDir, "identity")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, fmt.Errorf("create private CA directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".control-ca-*.tmp")
	if err != nil {
		return false, fmt.Errorf("create private CA record: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return false, fmt.Errorf("secure private CA record: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, fmt.Errorf("write private CA record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, fmt.Errorf("sync private CA record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close private CA record: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, recordFileName)); err != nil {
		return false, fmt.Errorf("install private CA record: %w", err)
	}
	return true, nil
}

func loadFile(path string) (*material, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect CA certificate %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("CA certificate %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read CA certificate %s: %w", path, err)
	}
	if len(data) > maxBundleBytes {
		return nil, fmt.Errorf("CA certificate %s exceeds %d bytes", path, maxBundleBytes)
	}
	certs, normalized, err := parseBundle(data, time.Now())
	if err != nil {
		return nil, fmt.Errorf("CA certificate %s: %w", path, err)
	}
	return &material{pem: normalized, certs: certs}, nil
}

func loadPersisted(nodeConfigDir, authURL, nexusURL string) (*material, error) {
	r, err := readPersistedRecord(nodeConfigDir)
	if err != nil || r == nil {
		return nil, err
	}
	authOrigin, err := endpointOrigin(authURL)
	if err != nil {
		return nil, nil
	}
	nexusOrigin, err := endpointOrigin(nexusURL)
	if err != nil {
		return nil, nil
	}
	if r.AuthOrigin != authOrigin || r.NexusOrigin != nexusOrigin {
		return nil, nil
	}
	certs, normalized, err := parseBundle([]byte(r.PEM), time.Now())
	if err != nil {
		return nil, fmt.Errorf("persisted private CA: %w", err)
	}
	return &material{pem: normalized, certs: certs, fromStore: true}, nil
}

// AuthOriginForNexus returns the persisted auth origin when the supplied Nexus
// belongs to the same saved tenant. This lets authkey-only enrollments recover
// the complete endpoint pair on a later process even though they have no
// device credential record carrying APIBaseURL.
func AuthOriginForNexus(nodeConfigDir, nexusURL string) (string, error) {
	r, err := readPersistedRecord(nodeConfigDir)
	if err != nil || r == nil {
		return "", err
	}
	nexusOrigin, err := endpointOrigin(nexusURL)
	if err != nil || r.NexusOrigin != nexusOrigin {
		return "", nil
	}
	return r.AuthOrigin, nil
}

// readPersistedRecord validates the durable metadata and PEM fingerprint but
// deliberately does not validate certificate dates. Endpoint recovery must
// still work when an expired persisted CA is being replaced by an explicit
// valid bundle; loadPersisted performs full certificate validation before use.
func readPersistedRecord(nodeConfigDir string) (*record, error) {
	path := filepath.Join(nodeConfigDir, "identity", recordFileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read persisted private CA: %w", err)
	}
	if len(data) > maxBundleBytes {
		return nil, fmt.Errorf("persisted private CA record exceeds %d bytes", maxBundleBytes)
	}
	var r record
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("decode persisted private CA: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode persisted private CA: trailing data")
	}
	if r.Version != recordVersion {
		return nil, fmt.Errorf("unsupported persisted private CA version %d", r.Version)
	}
	authOrigin, err := endpointOrigin(r.AuthOrigin)
	if err != nil || authOrigin != r.AuthOrigin {
		return nil, fmt.Errorf("persisted private CA has an invalid auth origin")
	}
	nexusOrigin, err := endpointOrigin(r.NexusOrigin)
	if err != nil || nexusOrigin != r.NexusOrigin {
		return nil, fmt.Errorf("persisted private CA has an invalid Nexus origin")
	}
	if authOrigin == managedAuthOrigin || nexusOrigin == managedNexusOrigin {
		return nil, fmt.Errorf("persisted private CA targets a managed AceTeam endpoint")
	}
	digest := sha256.Sum256([]byte(r.PEM))
	if !strings.EqualFold(r.SHA256, hex.EncodeToString(digest[:])) {
		return nil, fmt.Errorf("persisted private CA fingerprint mismatch")
	}
	return &r, nil
}

func parseBundle(data []byte, now time.Time) ([]*x509.Certificate, []byte, error) {
	rest := bytes.TrimSpace(data)
	var certs []*x509.Certificate
	var normalized []byte
	for len(rest) > 0 {
		block, tail := pem.Decode(rest)
		if block == nil {
			return nil, nil, fmt.Errorf("contains non-PEM data")
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, nil, fmt.Errorf("contains a non-certificate PEM block")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("parse certificate: %w", err)
		}
		if !cert.IsCA || !cert.BasicConstraintsValid {
			return nil, nil, fmt.Errorf("contains a certificate that is not a CA")
		}
		if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, nil, fmt.Errorf("contains a CA certificate without certificate-signing usage")
		}
		if now.Before(cert.NotBefore) {
			return nil, nil, fmt.Errorf("contains a CA certificate that is not valid yet")
		}
		if now.After(cert.NotAfter) {
			return nil, nil, fmt.Errorf("contains an expired CA certificate")
		}
		certs = append(certs, cert)
		normalized = append(normalized, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
		rest = bytes.TrimSpace(tail)
	}
	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("does not contain a CA certificate")
	}
	return certs, normalized, nil
}

func endpointOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("must be an HTTPS URL with a host")
	}
	hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if hostname == "" {
		return "", fmt.Errorf("must be an HTTPS URL with a host")
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port := u.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(hostname, port)
	}
	return "https://" + host, nil
}

func refuseManagedOrigins(authURL, nexusURL string) error {
	authOrigin, err := endpointOrigin(authURL)
	if err != nil {
		return fmt.Errorf("invalid auth service URL: %w", err)
	}
	nexusOrigin, err := endpointOrigin(nexusURL)
	if err != nil {
		return fmt.Errorf("invalid Nexus URL: %w", err)
	}
	if authOrigin == managedAuthOrigin || nexusOrigin == managedNexusOrigin {
		return fmt.Errorf("private CA trust is limited to self-hosted tenants and cannot be enabled for managed AceTeam endpoints")
	}
	return nil
}
