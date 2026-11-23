package cmd

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// writePerms writes body as permissions.yaml in a fresh temp dir.
func writePerms(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unreadablePerms makes permissions.yaml a directory, so reading it fails even
// for root (chmod 000 would not).
func unreadablePerms(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "permissions.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSSHDeployAllowedIn(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"missing file keeps the SSH default", t.TempDir(), true},
		{"ssh on", writePerms(t, "ssh: true\n"), true},
		{"ssh off", writePerms(t, "ssh: false\n"), false},
		{"damaged file refuses", writePerms(t, "ssh: [true\n"), false},
		{"unreadable file refuses", unreadablePerms(t), false},
	}
	for _, tc := range cases {
		if got := sshDeployAllowedIn(tc.dir); got != tc.want {
			t.Errorf("%s: sshDeployAllowedIn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFilesEnabledIn(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"missing file keeps the Files default (off)", t.TempDir(), false},
		{"files on", writePerms(t, "files: true\n"), true},
		{"files off", writePerms(t, "files: false\n"), false},
		{"damaged file refuses", writePerms(t, "files: [true\n"), false},
		{"unreadable file refuses", unreadablePerms(t), false},
	}
	for _, tc := range cases {
		if got := filesEnabledIn(tc.dir); got != tc.want {
			t.Errorf("%s: filesEnabledIn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The /rag control routes run the Files gate after the coordinator gate.
func TestRAGControlRoutesHonorFilesPermission(t *testing.T) {
	gated := func(dir string) int {
		sink := &fakeSink{}
		registerControlOnlyAPI(sink, fakeAPI{}, permissionGate("files", func() bool { return filesEnabledIn(dir) }))
		return serve(sink.control[0], allow)
	}
	if code := gated(writePerms(t, "files: false\n")); code != http.StatusForbidden {
		t.Fatalf("Files off must refuse, got %d", code)
	}
	if code := gated(unreadablePerms(t)); code != http.StatusForbidden {
		t.Fatalf("unreadable permissions must refuse, got %d", code)
	}
	if code := gated(writePerms(t, "files: true\n")); code != http.StatusOK {
		t.Fatalf("Files on must admit, got %d", code)
	}
}
