package catrust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBundleStrictCAOnly(t *testing.T) {
	caPEM, ca, caKey := makeCA(t, "private root", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	certs, normalized, err := parseBundle(caPEM, time.Now())
	if err != nil {
		t.Fatalf("parseBundle: %v", err)
	}
	if len(certs) != 1 || !certs[0].Equal(ca) || len(normalized) == 0 {
		t.Fatal("valid CA was not preserved")
	}

	leafPEM := makeLeaf(t, ca, caKey, "tenant.test")
	for name, input := range map[string][]byte{
		"empty":       nil,
		"junk":        append(append([]byte(nil), caPEM...), []byte("junk")...),
		"non ca":      leafPEM,
		"wrong block": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret")}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseBundle(input, time.Now()); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}

	expired, _, _ := makeCA(t, "expired", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	if _, _, err := parseBundle(expired, time.Now()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired CA error = %v", err)
	}
}

func TestPersistedTrustIsOriginBoundAndFingerprinted(t *testing.T) {
	caPEM, ca, _ := makeCA(t, "tenant root", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	activeMu.Lock()
	old := active
	active = &material{pem: caPEM, certs: []*x509.Certificate{ca}}
	activeMu.Unlock()
	t.Cleanup(func() {
		activeMu.Lock()
		active = old
		activeMu.Unlock()
	})

	dir := t.TempDir()
	if wrote, err := PersistActive(dir, "https://web.tenant.test/base", "https://nexus.tenant.test"); err != nil || !wrote {
		t.Fatalf("PersistActive: %v", err)
	}
	path := filepath.Join(dir, "identity", recordFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := loadPersisted(dir, "https://web.tenant.test/other", "https://NEXUS.tenant.test/")
	if err != nil || loaded == nil || !loaded.fromStore {
		t.Fatalf("matching load = %#v, %v", loaded, err)
	}
	loaded, err = loadPersisted(dir, "https://aceteam.ai", "https://nexus.aceteam.ai")
	if err != nil || loaded != nil {
		t.Fatalf("cross-tenant load = %#v, %v", loaded, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored record
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	stored.SHA256 = strings.Repeat("0", 64)
	data, err = json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPersisted(dir, "https://web.tenant.test", "https://nexus.tenant.test"); err == nil {
		t.Fatal("tampered record accepted")
	}
}

func TestBootstrapFreshProcessPreservesExistingRootsAndHostnameChecks(t *testing.T) {
	if os.Getenv("CITADEL_CA_TEST_CHILD") == "1" {
		runBootstrapChild(t)
		return
	}
	basePEM, _, _ := makeCA(t, "base root", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	customPEM, customCA, customKey := makeCA(t, "custom root", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leafPEM := makeLeaf(t, customCA, customKey, "web.tenant.test")
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.pem")
	customPath := filepath.Join(dir, "custom.pem")
	leafPath := filepath.Join(dir, "leaf.pem")
	for path, data := range map[string][]byte{basePath: basePEM, customPath: customPEM, leafPath: leafPEM} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapFreshProcessPreservesExistingRootsAndHostnameChecks$")
	cmd.Env = append(os.Environ(),
		"CITADEL_CA_TEST_CHILD=1",
		"SSL_CERT_FILE="+basePath,
		"CITADEL_CA_TEST_CUSTOM="+customPath,
		"CITADEL_CA_TEST_LEAF="+leafPath,
		"CITADEL_CA_TEST_DIR="+dir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func runBootstrapChild(t *testing.T) {
	custom := os.Getenv("CITADEL_CA_TEST_CUSTOM")
	dir := os.Getenv("CITADEL_CA_TEST_DIR")
	ok, err := Bootstrap(custom, dir, "https://web.tenant.test", "https://nexus.tenant.test")
	if err != nil || !ok {
		t.Fatalf("Bootstrap = %v, %v", ok, err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal(err)
	}
	baseData, err := os.ReadFile(os.Getenv("SSL_CERT_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	baseBlock, _ := pem.Decode(baseData)
	baseCert, err := x509.ParseCertificate(baseBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseCert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("existing root was replaced: %v", err)
	}
	leafData, err := os.ReadFile(os.Getenv("CITADEL_CA_TEST_LEAF"))
	if err != nil {
		t.Fatal(err)
	}
	leafBlock, _ := pem.Decode(leafData)
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "web.tenant.test"}); err != nil {
		t.Fatalf("custom root not trusted: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "ingress.local"}); err == nil {
		t.Fatal("wrong SAN was accepted")
	}
}

func makeCA(t *testing.T, name string, notBefore, notAfter time.Time) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}

func makeLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
