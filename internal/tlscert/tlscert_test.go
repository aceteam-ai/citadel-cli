package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureCert_GeneratesNew(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Hostname: "test-node",
		CertDir:  dir,
	}

	cert, err := EnsureCert(cfg)
	if err != nil {
		t.Fatalf("EnsureCert() error = %v", err)
	}

	if len(cert.Certificate) == 0 {
		t.Fatal("EnsureCert() returned empty certificate chain")
	}

	// Verify files were written
	if _, err := os.Stat(filepath.Join(dir, certFileName)); err != nil {
		t.Errorf("cert file not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Errorf("key file not created: %v", err)
	}

	// Verify key file permissions
	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("key file permissions = %o, want 0600", perm)
	}
}

func TestEnsureCert_ReusesExisting(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Hostname: "test-node",
		CertDir:  dir,
	}

	// Generate first
	cert1, err := EnsureCert(cfg)
	if err != nil {
		t.Fatalf("first EnsureCert() error = %v", err)
	}

	// Load again — should reuse
	cert2, err := EnsureCert(cfg)
	if err != nil {
		t.Fatalf("second EnsureCert() error = %v", err)
	}

	// Same serial number means same cert was reused
	leaf1, _ := x509.ParseCertificate(cert1.Certificate[0])
	leaf2, _ := x509.ParseCertificate(cert2.Certificate[0])
	if leaf1.SerialNumber.Cmp(leaf2.SerialNumber) != 0 {
		t.Error("EnsureCert() generated a new cert instead of reusing existing")
	}
}

func TestEnsureCert_SANs(t *testing.T) {
	dir := t.TempDir()
	vpnIP := net.ParseIP("100.64.0.42")
	cfg := Config{
		Hostname:    "my-gpu-node",
		IPAddresses: []net.IP{vpnIP},
		CertDir:     dir,
	}

	cert, err := EnsureCert(cfg)
	if err != nil {
		t.Fatalf("EnsureCert() error = %v", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	// Check DNS SANs
	wantDNS := map[string]bool{"localhost": false, "my-gpu-node": false}
	for _, name := range leaf.DNSNames {
		if _, ok := wantDNS[name]; ok {
			wantDNS[name] = true
		}
	}
	for name, found := range wantDNS {
		if !found {
			t.Errorf("DNS SAN %q not found in certificate", name)
		}
	}

	// Check IP SANs
	foundLoopback := false
	foundVPN := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			foundLoopback = true
		}
		if ip.Equal(vpnIP) {
			foundVPN = true
		}
	}
	if !foundLoopback {
		t.Error("127.0.0.1 not found in IP SANs")
	}
	if !foundVPN {
		t.Errorf("VPN IP %s not found in IP SANs", vpnIP)
	}

	// Verify validity period
	if leaf.NotAfter.Before(time.Now().Add(364 * 24 * time.Hour)) {
		t.Error("certificate expires too soon")
	}
}

func TestEnsureCert_TLSUsable(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Hostname: "test-node",
		CertDir:  dir,
	}

	cert, err := EnsureCert(cfg)
	if err != nil {
		t.Fatalf("EnsureCert() error = %v", err)
	}

	// Verify it can be used in a TLS config
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	if len(tlsCfg.Certificates) != 1 {
		t.Error("TLS config should have exactly 1 certificate")
	}
}

func TestCertPath(t *testing.T) {
	got := CertPath("/custom/dir")
	want := filepath.Join("/custom/dir", "server.crt")
	if got != want {
		t.Errorf("CertPath() = %q, want %q", got, want)
	}
}

func TestKeyPath(t *testing.T) {
	got := KeyPath("/custom/dir")
	want := filepath.Join("/custom/dir", "server.key")
	if got != want {
		t.Errorf("KeyPath() = %q, want %q", got, want)
	}
}

func TestCertDir_Default(t *testing.T) {
	dir := certDir("")
	if dir == "" {
		t.Error("certDir() returned empty string for default")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("certDir() returned relative path: %s", dir)
	}
}
