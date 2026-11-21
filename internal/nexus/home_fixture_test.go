package nexus

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestSSHSyncUsesHomeFixtureAndPreservesForeignKeys(t *testing.T) {
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
	want := filepath.Join(home, ".ssh", "authorized_keys")
	if path, err := authorizedKeysPath(); err != nil || path != want {
		t.Fatal("SSH key path escaped fixture before network or file effects")
	}
	foreignPath := filepath.Join(foreign, ".ssh", "authorized_keys")
	for _, path := range []string{want, foreignPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic-existing-entry\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/nodes/synthetic/authorized-keys" {
			t.Error("unexpected fixture request path")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authorized_keys":"synthetic-managed-entry"}`))
	}))
	t.Cleanup(server.Close)
	if err := SyncAuthorizedKeys(SSHSyncConfig{APIToken: "synthetic-not-a-credential", NodeID: "synthetic", BaseURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(want)
	if err != nil || !bytes.Contains(data, []byte(ManagedKeysStart)) || !bytes.Contains(data, []byte("synthetic-managed-entry")) || !bytes.Contains(data, []byte("synthetic-existing-entry")) {
		t.Fatal("positive control did not update owned authorized_keys")
	}
	if data, err := os.ReadFile(foreignPath); err != nil || string(data) != "synthetic-existing-entry\n" {
		t.Fatal("SSH sync changed foreign synthetic authorized_keys")
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(want); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("owned authorized_keys lost private mode: %v", err)
		}
	}
}
