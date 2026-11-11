package status

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitControlTLS(t *testing.T, s *Server, present bool) *ControlTLSReport {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		report := s.ControlTLS()
		if (report != nil) == present {
			return report
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("loaded certificate presence did not become %t", present)
	return nil
}

func TestControlTLSReportsCertificateActuallyServedAcrossDiskRotationAndRestart(t *testing.T) {
	ca := newTestCA(t, "offline-control-test")
	verifier, err := NewFabricCAVerifier(writeBundle(t, ca.certPEM), []string{coordinatorSAN})
	if err != nil {
		t.Fatal(err)
	}
	clientCert := ca.issueLeaf(t, "coordinator", coordinatorSAN)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "control.crt"), filepath.Join(dir, "control.key")
	writePair := func(cert tls.Certificate) {
		key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first := ca.issueLeaf(t, "control-first")
	second := ca.issueLeaf(t, "control-second")
	writePair(first)
	start := func() (*Server, string) {
		loaded, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s := NewServer(ServerConfig{CAVerifier: verifier, ControlServerCert: &loaded, ControlPort: -1}, nil)
		if s.ControlTLS() != nil {
			t.Fatal("configured certificate is not yet serving evidence")
		}
		s.AddControlListener(ln)
		s.startControlServer()
		t.Cleanup(func() { _ = s.controlHTTPServer.Close() })
		waitControlTLS(t, s, true)
		return s, ln.Addr().String()
	}
	assertServed := func(s *Server, addr string, expected []byte) {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		served := conn.ConnectionState().PeerCertificates[0].Raw
		report := s.ControlTLS()
		if report == nil {
			t.Fatal("serving listener must report certificate")
		}
		block, _ := pem.Decode([]byte(report.CertPEM))
		if block == nil || !bytes.Equal(block.Bytes, served) || !bytes.Equal(served, expected) {
			t.Fatal("heartbeat evidence disagrees with actual TLS handshake")
		}
		report.CertPEM = "mutated caller snapshot"
		if s.ControlTLS().CertPEM == report.CertPEM {
			t.Fatal("snapshot must not mutate listener evidence")
		}
	}
	s, addr := start()
	assertServed(s, addr, first.Certificate[0])
	writePair(second)
	assertServed(s, addr, first.Certificate[0])
	_ = s.controlHTTPServer.Close()
	waitControlTLS(t, s, false)
	restarted, restartedAddr := start()
	assertServed(restarted, restartedAddr, second.Certificate[0])
}

func TestControlTLSOmittedForDisabledFailedAndInvalidListeners(t *testing.T) {
	s := NewServer(ServerConfig{}, nil)
	s.startControlServer()
	if s.ControlTLS() != nil {
		t.Fatal("disabled listener cannot report loaded evidence")
	}
	ca := newTestCA(t, "test-ca")
	verifier, err := NewFabricCAVerifier(writeBundle(t, ca.certPEM), []string{coordinatorSAN})
	if err != nil {
		t.Fatal(err)
	}
	cert := ca.issueLeaf(t, "test-control")
	s = NewServer(ServerConfig{CAVerifier: verifier, ControlServerCert: &cert, ControlPort: -1}, nil)
	s.startControlServer()
	if s.ControlTLS() != nil {
		t.Fatal("failed listener cannot report configured cert as loaded")
	}
	_, report := freezeControlCertificate(tls.Certificate{Certificate: [][]byte{[]byte("bad DER")}})
	if report != nil {
		t.Fatal("invalid loaded bytes cannot report a certificate")
	}
	frozen, report := freezeControlCertificate(cert)
	if report == nil {
		t.Fatal("valid cert omitted")
	}
	cert.Certificate[0][0] ^= 1
	if bytes.Equal(cert.Certificate[0], frozen.Certificate[0]) {
		t.Fatal("TLS must own an independent DER snapshot")
	}
}

func TestControlTLSRemainsPresentWhileAnotherListenerServes(t *testing.T) {
	ca := newTestCA(t, "offline-multiple-listeners")
	verifier, err := NewFabricCAVerifier(writeBundle(t, ca.certPEM), []string{coordinatorSAN})
	if err != nil {
		t.Fatal(err)
	}
	cert := ca.issueLeaf(t, "control")
	s := NewServer(ServerConfig{CAVerifier: verifier, ControlServerCert: &cert, ControlPort: -1}, nil)
	for range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s.AddControlListener(ln)
	}
	s.startControlServer()
	defer s.controlHTTPServer.Close()
	clientCert := ca.issueLeaf(t, "coordinator", coordinatorSAN)
	for _, ln := range s.extraControlListeners {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", ln.Addr().String(), &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{clientCert},
			MinVersion:         tls.VersionTLS12,
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	_ = s.extraControlListeners[0].Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.controlTLSMu.Lock()
		remaining := s.controlTLSListeners
		s.controlTLSMu.Unlock()
		if remaining == 1 {
			if s.ControlTLS() == nil {
				t.Fatal("stopping one listener must not erase another serving listener's evidence")
			}
			_ = s.controlHTTPServer.Close()
			waitControlTLS(t, s, false)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("first listener did not stop")
}
