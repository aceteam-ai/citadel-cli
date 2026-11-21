package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
)

func TestJobDefaultsAndInstanceWriterUseExplicitHome(t *testing.T) {
	home, foreign := t.TempDir(), t.TempDir()
	restore, err := platform.OverrideHomeDirectoryForTesting(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("HOME", foreign)
	t.Setenv("USERPROFILE", foreign)
	for _, tc := range []struct {
		got, rel string
	}{
		{bonsaiCacheDir(), filepath.Join("citadel-cache", services.BonsaiCacheDirName)},
		{defaultLlamaCppCacheDir(), filepath.Join("citadel-cache", services.LlamaCppCacheDirName)},
		{canonicalHFCacheDir(), filepath.Join("citadel-cache", services.HFHubCacheDirName)},
		{legacyHFCacheDir(), filepath.Join(".cache", "huggingface")},
	} {
		if tc.got != filepath.Join(home, tc.rel) {
			t.Fatalf("job default escaped fixture: %q", tc.got)
		}
	}
	want := filepath.Join(home, ".citadel", "instances", "state.json")
	if got, err := instanceStorePath(); err != nil || got != want {
		t.Fatal("instance path escaped fixture before the first read/write")
	}
	store, err := newInstanceStore()
	if err != nil {
		t.Fatal(err)
	}
	if store.path != want {
		t.Fatal("instance store captured a foreign path before saving")
	}
	if err := store.save(map[string]InstanceRecord{"synthetic": {ServiceName: "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(want); err != nil || !bytes.Contains(data, []byte("synthetic")) {
		t.Fatalf("positive control did not save owned instance state: %v", err)
	}
	for _, root := range []string{home, foreign} {
		if _, err := os.Stat(filepath.Join(root, "citadel-cache")); !os.IsNotExist(err) {
			t.Fatal("path selection started cache/model work")
		}
	}
	if _, err := os.Stat(filepath.Join(foreign, ".citadel")); !os.IsNotExist(err) {
		t.Fatal("instance writer touched foreign home")
	}
}
