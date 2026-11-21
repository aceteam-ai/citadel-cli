package jobs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestAMTPinnedTLSFailsClosed(t *testing.T) {
	if _, err := newAMTPinnedTLSConfig("10.0.0.2", [sha256.Size]byte{}); err == nil {
		t.Fatal("missing binding pin accepted")
	}
	pin := sha256.Sum256([]byte("synthetic certificate"))
	for _, host := range []string{"amt.example.test", "127.0.0.1", "100.64.0.1", "8.8.8.8"} {
		if _, err := newAMTPinnedTLSConfig(host, pin); err == nil {
			t.Fatal("non-LAN binding accepted")
		}
	}
	cfg, err := newAMTPinnedTLSConfig("10.0.0.2", pin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || cfg.ServerName != "10.0.0.2" || cfg.VerifyConnection == nil {
		t.Fatal("pinned TLS policy incomplete")
	}
	for _, state := range []tls.ConnectionState{{}, {PeerCertificates: []*x509.Certificate{nil}}, {PeerCertificates: []*x509.Certificate{{Raw: []byte("different synthetic certificate")}}}} {
		if err := cfg.VerifyConnection(state); err == nil {
			t.Fatal("absent or mismatched peer certificate accepted")
		}
	}
}

func TestAMTPinnedTLSHandshakeOverMemoryPipe(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := func(serial int64) tls.Certificate {
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "synthetic AMT firmware"}, NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key}
	}
	bound, rotated := certificate(1), certificate(2) // same key, different exact leaf
	pin := sha256.Sum256(bound.Certificate[0])
	for _, tc := range []struct {
		name   string
		cert   tls.Certificate
		wantOK bool
	}{{"bound self-signed leaf", bound, true}, {"different leaf with same key", rotated, false}} {
		t.Run(tc.name, func(t *testing.T) {
			clientRaw, serverRaw := net.Pipe()
			defer clientRaw.Close()
			defer serverRaw.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cfg, err := newAMTPinnedTLSConfig("10.0.0.2", pin)
			if err != nil {
				t.Fatal(err)
			}
			client := tls.Client(clientRaw, cfg)
			server := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{tc.cert}, MinVersion: tls.VersionTLS12, SessionTicketsDisabled: true})
			done := make(chan error, 1)
			go func() { done <- server.HandshakeContext(ctx) }()
			clientErr := client.HandshakeContext(ctx)
			clientRaw.Close()
			serverRaw.Close()
			serverErr := <-done
			if (clientErr == nil) != tc.wantOK {
				t.Fatalf("pin acceptance=%t want=%t", clientErr == nil, tc.wantOK)
			}
			if tc.wantOK && serverErr != nil {
				t.Fatal("bound in-memory TLS handshake failed")
			}
		})
	}
}
