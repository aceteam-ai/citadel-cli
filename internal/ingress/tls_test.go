package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/libdns/cloudflare"
	"github.com/libdns/rfc2136"
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

func writeTestPair(t *testing.T, certPath, keyPath, domain string) {
	t.Helper()
	cert, err := generateSelfSigned(domain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStaticFileCertProvider_MultiSetReloadAndUnknownSNI(t *testing.T) {
	dir := t.TempDir()
	appsCert, appsKey := filepath.Join(dir, "apps.crt"), filepath.Join(dir, "apps.key")
	hostCert, hostKey := filepath.Join(dir, "demo.example.net.crt"), filepath.Join(dir, "demo.example.net.key")
	writeTestPair(t, appsCert, appsKey, "apps.example.com")
	writeTestPair(t, hostCert, hostKey, "demo.example.net")
	p := &StaticFileCertProvider{CertFile: appsCert, KeyFile: appsKey, CertDir: dir,
		HostCertBase: func(host string) (string, bool) {
			return "demo.example.net", host == "demo.example.net" || host == "supabase.demo.example.net"
		},
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	get := func(name string) (*tls.Certificate, error) {
		return p.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: name})
	}
	first, err := get("supabase.demo.example.net")
	if err != nil {
		t.Fatal(err)
	}
	firstLeaf, _ := x509.ParseCertificate(first.Certificate[0])
	writeTestPair(t, hostCert, hostKey, "demo.example.net")
	second, err := get("supabase.demo.example.net")
	if err != nil {
		t.Fatal(err)
	}
	secondLeaf, _ := x509.ParseCertificate(second.Certificate[0])
	if firstLeaf.SerialNumber.Cmp(secondLeaf.SerialNumber) == 0 {
		t.Fatal("mounted certificate did not reload")
	}
	if _, err := get("unknown.example.net"); err == nil {
		t.Fatal("unknown SNI accepted")
	}
	if _, err := get("slug.apps.example.com"); err != nil {
		t.Fatalf("apps wildcard lost: %v", err)
	}
}

func TestNewACMECertProvider_ValidatesConfig(t *testing.T) {
	rfc := RFC2136Config{Server: "ns1.example.net:53", KeyName: "k.", KeyAlg: "hmac-sha256.", Key: "c2VjcmV0"}
	cf := CloudflareConfig{APIToken: "cf-token"}
	cases := []struct {
		name string
		cfg  ACMEConfig
	}{
		{"no apps domain", ACMEConfig{DNSZone: "z", StorageDir: "/tmp", RFC2136: rfc}},
		{"no dns provider", ACMEConfig{AppsDomain: "apps.example.com", DNSZone: "z", StorageDir: "/tmp"}},
		{"rfc2136 without dns zone", ACMEConfig{AppsDomain: "apps.example.com", StorageDir: "/tmp", RFC2136: rfc}},
		{"both providers", ACMEConfig{AppsDomain: "apps.example.com", DNSZone: "z", StorageDir: "/tmp", RFC2136: rfc, Cloudflare: cf}},
		{"no storage dir", ACMEConfig{AppsDomain: "apps.example.com", DNSZone: "z", RFC2136: rfc}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewACMECertProvider(tc.cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
	// Complete configs validate (no network contact until Start).
	if _, err := NewACMECertProvider(ACMEConfig{
		AppsDomain: "apps.example.com", DNSZone: "acme.example.net", StorageDir: t.TempDir(), RFC2136: rfc,
	}); err != nil {
		t.Fatalf("complete rfc2136 config should validate: %v", err)
	}
	if _, err := NewACMECertProvider(ACMEConfig{
		AppsDomain: "apps.example.com", StorageDir: t.TempDir(), Cloudflare: cf,
	}); err != nil {
		t.Fatalf("cloudflare config without a dns zone should validate: %v", err)
	}
}

func TestDNSProviderForSelectsConfiguredBackend(t *testing.T) {
	p, err := dnsProviderFor(ACMEConfig{Cloudflare: CloudflareConfig{APIToken: "t", ZoneToken: "z"}})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := p.(*cloudflare.Provider)
	if !ok || c.APIToken != "t" || c.ZoneToken != "z" {
		t.Fatalf("cloudflare token selected %T %+v", p, p)
	}
	p, err = dnsProviderFor(ACMEConfig{DNSZone: "z", RFC2136: RFC2136Config{Server: "ns:53", KeyName: "k."}})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := p.(*rfc2136.Provider); !ok || r.Server != "ns:53" || r.KeyName != "k." {
		t.Fatalf("rfc2136 server selected %T %+v", p, p)
	}
}
