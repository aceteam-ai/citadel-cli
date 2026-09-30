package nexus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catrust"
)

func TestPrivateCADeviceAuthFlow(t *testing.T) {
	if os.Getenv("CITADEL_NEXUS_CA_CHILD") == "1" {
		runPrivateCADeviceAuthChild(t)
		return
	}
	caPEM, caCert, caKey := testCA(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}

	good := newDeviceAuthTLSServer(t, caCert, caKey, true)
	defer good.Close()
	runPrivateCAChild(t, dir, caPath, good.URL, "success")

	bad := newDeviceAuthTLSServer(t, caCert, caKey, false)
	defer bad.Close()
	runPrivateCAChild(t, dir, caPath, bad.URL, "wrong-san")
}

func runPrivateCAChild(t *testing.T, dir, caPath, serverURL, mode string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrivateCADeviceAuthFlow$")
	cmd.Env = append(os.Environ(),
		"CITADEL_NEXUS_CA_CHILD=1",
		"CITADEL_NEXUS_CA_DIR="+dir,
		"CITADEL_NEXUS_CA_PATH="+caPath,
		"CITADEL_NEXUS_CA_URL="+serverURL,
		"CITADEL_NEXUS_CA_MODE="+mode,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s child failed: %v\n%s", mode, err, out)
	}
}

func runPrivateCADeviceAuthChild(t *testing.T) {
	serverURL := os.Getenv("CITADEL_NEXUS_CA_URL")
	if ok, err := catrust.Bootstrap(
		os.Getenv("CITADEL_NEXUS_CA_PATH"),
		os.Getenv("CITADEL_NEXUS_CA_DIR"),
		serverURL,
		serverURL,
	); err != nil || !ok {
		t.Fatalf("Bootstrap = %v, %v", ok, err)
	}
	err := CheckAPIReachable(serverURL)
	if os.Getenv("CITADEL_NEXUS_CA_MODE") == "wrong-san" {
		if err == nil {
			t.Fatal("wrong SAN passed reachability check")
		}
		return
	}
	if err != nil {
		t.Fatalf("CheckAPIReachable: %v", err)
	}
	client := NewDeviceAuthClient(serverURL)
	start, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	if start.DeviceCode != "device-code" {
		t.Fatalf("device code = %q", start.DeviceCode)
	}
	token, err := client.CheckToken(start.DeviceCode)
	if err != nil {
		t.Fatalf("CheckToken: %v", err)
	}
	if token.Authkey != "auth-key" {
		t.Fatalf("auth key = %q", token.Authkey)
	}
}

func newDeviceAuthTLSServer(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, correctSAN bool) *httptest.Server {
	t.Helper()
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "device auth test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if correctSAN {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.DNSNames = []string{"ingress.local"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{der, ca.Raw},
		PrivateKey:  serverKey,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/api/fabric/device-auth/start":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case r.Method == http.MethodPost && r.URL.Path == "/api/fabric/device-auth/start":
			_ = json.NewEncoder(w).Encode(DeviceCodeResponse{
				DeviceCode:      "device-code",
				UserCode:        "ABCD-1234",
				VerificationURI: "https://example.invalid/device",
				ExpiresIn:       300,
				Interval:        1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/fabric/device-auth/token":
			_ = json.NewEncoder(w).Encode(TokenResponse{Authkey: "auth-key"})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	return srv
}

func testCA(t *testing.T) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "device auth root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
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
