package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestSelfSignedCertProvider_ServesCert(t *testing.T) {
	p := &SelfSignedCertProvider{AppsDomain: "apps.example.com"}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cfg := p.TLSConfig()
	cert, err := cfg.GetCertificate(nil)
	if err != nil || cert == nil {
		t.Fatalf("GetCertificate: cert=%v err=%v", cert, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	// Wildcard + apex both covered.
	if err := leaf.VerifyHostname("foo.apps.example.com"); err != nil {
		t.Errorf("wildcard not covered: %v", err)
	}
	if err := leaf.VerifyHostname("apps.example.com"); err != nil {
		t.Errorf("apex not covered: %v", err)
	}
}

func TestStaticFileCertProvider_LoadsMountedWildcard(t *testing.T) {
	// Generate a self-signed cert/key and write them as PEM files, then load.
	tlsCert, err := generateSelfSigned("apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "wildcard.crt")
	keyPath := filepath.Join(dir, "wildcard.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsCert.Certificate[0]})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(tlsCert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	p := &StaticFileCertProvider{CertFile: certPath, KeyFile: keyPath}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c, err := p.TLSConfig().GetCertificate(nil); err != nil || c == nil {
		t.Fatalf("GetCertificate: %v", err)
	}
}

func TestNewACMECertProvider_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  ACMEConfig
	}{
		{"no apps domain", ACMEConfig{DNSZone: "z", StorageDir: "/tmp"}},
		{"no dns zone", ACMEConfig{AppsDomain: "apps.example.com", StorageDir: "/tmp"}},
		{"no storage dir", ACMEConfig{AppsDomain: "apps.example.com", DNSZone: "z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewACMECertProvider(tc.cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
	// A complete config validates (no network contact until Start).
	if _, err := NewACMECertProvider(ACMEConfig{
		AppsDomain: "apps.example.com", DNSZone: "acme.example.net", StorageDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("complete config should validate: %v", err)
	}
}
