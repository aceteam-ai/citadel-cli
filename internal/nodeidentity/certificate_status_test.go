package nodeidentity

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
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func diagnosticLeaf(t *testing.T, public *ecdsa.PublicKey, from, to time.Time) string {
	t.Helper()
	issuer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-node"}, NotBefore: from, NotAfter: to, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, issuer)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestInspectCertificateDoesNotGenerateIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent-identity")
	store := New(dir)
	report := store.InspectCertificate(time.Now())
	if report.State != "missing_leaf" || report.LeafPresent == nil || *report.LeafPresent || report.SigningKeyPresent == nil || *report.SigningKeyPresent || report.SPKIMatchesSigner != nil {
		t.Fatal("missing identity reported as a match")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("diagnostic created identity state")
	}
}

func TestInspectCertificateReportsLocalSPKIAndValidity(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"match", "mismatch", "expired", "future", "missing-key", "bad-key", "bad-leaf", "key-only"} {
		t.Run(name, func(t *testing.T) {
			store := New(t.TempDir())
			key, err := store.GetOrCreateKey()
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.KeyPath())
			if err != nil {
				t.Fatal(err)
			}
			public := &key.PublicKey
			if name == "mismatch" {
				other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				public = &other.PublicKey
			}
			from, to := now.Add(-time.Hour), now.Add(time.Hour)
			if name == "expired" {
				to = now
			}
			if name == "future" {
				from = now.Add(time.Second)
			}
			if name != "key-only" {
				if err := store.StoreLeaf(diagnosticLeaf(t, public, from, to), ""); err != nil {
					t.Fatal(err)
				}
			}
			if name == "missing-key" {
				if err := os.Remove(store.KeyPath()); err != nil {
					t.Fatal(err)
				}
			}
			if name == "bad-key" {
				if err := os.WriteFile(store.KeyPath(), []byte("invalid-key-fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "bad-leaf" {
				if err := os.WriteFile(store.LeafPath(), []byte("invalid-leaf-fixture"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			got := store.InspectCertificate(now)
			expected := map[string]string{"match": "local_match", "mismatch": "spki_mismatch", "expired": "expired_leaf", "future": "not_yet_valid", "missing-key": "signing_key_unavailable", "bad-key": "signing_key_unavailable", "bad-leaf": "invalid_leaf", "key-only": "missing_leaf"}[name]
			if got.State != expected || got.LeafPresent == nil || *got.LeafPresent != (name != "key-only") || got.SigningKeyPresent == nil || *got.SigningKeyPresent != (name != "missing-key") {
				t.Fatal("incorrect local certificate state", name, got.State)
			}
			known := name == "match" || name == "mismatch" || name == "expired" || name == "future"
			if known {
				if got.SPKIMatchesSigner == nil || *got.SPKIMatchesSigner != (name != "mismatch") {
					t.Fatal("SPKI comparison missing or incorrect")
				}
			} else if got.SPKIMatchesSigner != nil {
				t.Fatal("unavailable comparison reported a boolean")
			}
			if name != "missing-key" && name != "bad-key" {
				after, err := os.ReadFile(store.KeyPath())
				if err != nil || string(after) != string(before) {
					t.Fatal("diagnostic changed signing key")
				}
			}
			wire, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{store.Dir(), "node.key", "PRIVATE KEY"} {
				if strings.Contains(string(wire), private) || strings.Contains(got.Summary(), private) {
					t.Fatal("diagnostic disclosed identity storage details")
				}
			}
		})
	}
}

func TestInspectCertificateAdoptsExistingLegacyIdentityBeforeComparison(t *testing.T) {
	legacy := New(t.TempDir())
	key, err := legacy.GetOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := legacy.StoreLeaf(diagnosticLeaf(t, &key.PublicKey, now.Add(-time.Hour), now.Add(time.Hour)), ""); err != nil {
		t.Fatal(err)
	}
	store := &Store{dir: filepath.Join(t.TempDir(), "convergent"), legacyDir: legacy.Dir()}
	got := store.InspectCertificate(now)
	if got.State != "local_match" || got.LeafPresent == nil || !*got.LeafPresent || got.SPKIMatchesSigner == nil || !*got.SPKIMatchesSigner {
		t.Fatal("legacy identity was diagnosed before convergence")
	}
	old, _ := os.ReadFile(legacy.KeyPath())
	current, _ := os.ReadFile(store.KeyPath())
	if string(old) != string(current) {
		t.Fatal("diagnostic minted a replacement identity")
	}
}

func TestInspectCertificateUnreadableEntryIsNotReportedMissing(t *testing.T) {
	store := New(t.TempDir())
	if err := os.Mkdir(store.LeafPath(), 0700); err != nil {
		t.Fatal(err)
	}
	report := store.InspectCertificate(time.Now())
	if report.State != "leaf_unavailable" || report.LeafPresent == nil || !*report.LeafPresent || report.SPKIMatchesSigner != nil || strings.Contains(report.Summary(), "missing") {
		t.Fatal("unreadable entry was treated as an absent or compared certificate")
	}
	var absent *Store
	report = absent.InspectCertificate(time.Now())
	if report.State != "unavailable" || report.LeafPresent != nil || report.SigningKeyPresent != nil {
		t.Fatal("unavailable store fabricated presence observations")
	}
}
