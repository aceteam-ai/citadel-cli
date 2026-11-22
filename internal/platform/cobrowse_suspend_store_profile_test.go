package platform

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowseprofile"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

// Synthetic vault only, created under t.TempDir. Never an operator vault,
// saved profile, node directory or configured credential.
func TestSuspendStoreRealProfileCryptoRoundTrip(t *testing.T) {
	s, reg := testSuspendStore(t)
	base := t.TempDir()
	vault := nodevault.Open(base)
	const phrase = "synthetic lantern orbit pebble saffron"
	if err := vault.SetPIN(phrase, nodevault.DefaultPolicy(), true); err != nil {
		t.Fatal(err)
	}
	h, err := cobrowseprofile.OpenHandle(base, "profile-a", phrase, vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	r, p := captureRecord(t, suspendTestManifest(), true, h)
	if err := s.write(r, p); err != nil {
		t.Fatal(err)
	}
	name, _ := suspendManifestName(r)
	stored, err := os.ReadFile(filepath.Join(reg, suspendManifestDirName, name))
	if err != nil || bytes.Contains(stored, []byte("private-sentinel")) {
		t.Fatal("saved manifest is not encrypted at rest")
	}
	if _, err := s.read(r, h); err != nil {
		t.Fatal("unlocked profile could not read its own tabs")
	}
	_ = h.Close()
	if _, err := s.read(r, h); err == nil {
		t.Fatal("closed profile decrypted saved tabs")
	}
	h2, err := cobrowseprofile.OpenHandle(base, "profile-a", phrase, vault)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if got, err := s.read(r, h2); err != nil || got.Tabs[0].URL != suspendTestManifest().Tabs[0].URL {
		t.Fatal("fresh human unlock did not restore the capture")
	}
	other, err := cobrowseprofile.OpenHandle(base, "profile-b", phrase, vault)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := prepareSuspendCapture(suspendTestManifest(), true, "profile-a", other); err == nil {
		t.Fatal("wrong unlocked profile accepted before persistence")
	}
	if _, err := s.read(r, other); err == nil {
		t.Fatal("wrong unlocked profile read saved tabs")
	}
}
