package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestPreparePrivateCATrustRecoversAuthOriginWithExplicitCA(t *testing.T) {
	if os.Getenv("CITADEL_CA_EXPLICIT_RECOVERY_CHILD") == "1" {
		t.Setenv("CITADEL_AUTH_HOST", "")
		nodeConfigDirFn = func() string { return os.Getenv("CITADEL_CA_TEST_DIR") }
		deviceConfigForCATrustFn = func() *DeviceConfig { return nil }
		caCertPath = os.Getenv("CITADEL_CA_TEST_CERT")
		authServiceURL = "https://aceteam.ai"
		nexusURL = "https://nexus.tenant.test"

		cmd := &cobra.Command{}
		cmd.Flags().String("nexus", "", "")
		if err := cmd.Flags().Set("nexus", nexusURL); err != nil {
			t.Fatal(err)
		}
		if err := preparePrivateCATrust(cmd); err != nil {
			t.Fatalf("preparePrivateCATrust: %v", err)
		}
		if authServiceURL != "https://web.tenant.test" {
			t.Fatalf("authServiceURL = %q", authServiceURL)
		}
		return
	}

	dir := t.TempDir()
	oldCAPEM := makePrivateCATestCA(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	newCAPEM := makePrivateCATestCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	caPath := filepath.Join(dir, "tenant-ca.pem")
	if err := os.WriteFile(caPath, newCAPEM, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(oldCAPEM)
	record := map[string]any{
		"version":      1,
		"auth_origin":  "https://web.tenant.test",
		"nexus_origin": "https://nexus.tenant.test",
		"sha256":       hex.EncodeToString(digest[:]),
		"pem":          string(oldCAPEM),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	identityDir := filepath.Join(dir, "identity")
	if err := os.MkdirAll(identityDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identityDir, "control-ca.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestPreparePrivateCATrustRecoversAuthOriginWithExplicitCA$")
	child.Env = append(os.Environ(),
		"CITADEL_CA_EXPLICIT_RECOVERY_CHILD=1",
		"CITADEL_CA_TEST_DIR="+dir,
		"CITADEL_CA_TEST_CERT="+caPath,
	)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
}

func makePrivateCATestCA(t *testing.T, notBefore, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tenant root"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
