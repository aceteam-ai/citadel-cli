package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePKCS8(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckRefusesMissingKey(t *testing.T) {
	err := run([]string{"check", "--key", filepath.Join(t.TempDir(), "absent.pem")})
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v, want unreadable", err)
	}
	if err := run([]string{"check"}); err == nil {
		t.Fatal("check without --key succeeded")
	}
}

func TestCheckRefusesUntrustedKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"check", "--key", writePKCS8(t, priv)})
	if err == nil || !strings.Contains(err.Error(), "not in the compiled trusted key set") {
		t.Fatalf("err = %v, want untrusted-key refusal", err)
	}
}

func TestCheckRefusesNonEd25519Key(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"check", "--key", writePKCS8(t, ec)})
	if err == nil || !strings.Contains(err.Error(), "not Ed25519") {
		t.Fatalf("err = %v, want non-Ed25519 refusal", err)
	}
}

func TestSignRefusesUntrustedKeyAndWritesNothing(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	in := filepath.Join(dir, "checksums.txt")
	out := filepath.Join(dir, "checksums.txt.sig")
	_ = os.WriteFile(in, []byte(strings.Repeat("a", 64)+"  citadel_v1.0.0_linux_amd64.tar.gz\n"), 0o644)
	if err := run([]string{"sign", "--key", writePKCS8(t, priv), "--tag", "v1.0.0", "--in", in, "--out", out}); err == nil {
		t.Fatal("sign with an untrusted key succeeded")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("signature written: %v", err)
	}
}

func TestVerifyRefusesMissingAndForeignSignatures(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "checksums.txt")
	_ = os.WriteFile(in, []byte(strings.Repeat("a", 64)+"  citadel_v1.0.0_linux_amd64.tar.gz\n"), 0o644)
	if err := run([]string{"verify", "--tag", "v1.0.0", "--in", in, "--sig", filepath.Join(dir, "absent.sig")}); err == nil {
		t.Fatal("verify without a signature succeeded")
	}
	bogus := filepath.Join(dir, "bogus.sig")
	_ = os.WriteFile(bogus, []byte(`{"v":1,"key_id":"0000000000000000","sig":"`+strings.Repeat("A", 86)+`=="}`), 0o644)
	if err := run([]string{"verify", "--tag", "v1.0.0", "--in", in, "--sig", bogus}); err == nil {
		t.Fatal("verify with a foreign signature succeeded")
	}
}
