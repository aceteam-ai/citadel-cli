package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestCobrowsePublicOnlyActivation: absent and false keep today's behavior,
// true turns the guard on, and a damaged file counts as on.
func TestCobrowsePublicOnlyActivation(t *testing.T) {
	cases := []struct {
		name, file string
		want       bool
	}{
		{"absent file", "", false},
		{"absent key", "shell: true\n", false},
		{"false", "cobrowse_public_only: false\n", false},
		{"true", "cobrowse_public_only: true\n", true},
		{"unparseable", "cobrowse_public_only: [\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := cobrowsePublicOnlyIn(dir); got != tc.want {
				t.Fatalf("public-only = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSetCobrowsePublicOnlyKeepsOtherPermissions(t *testing.T) {
	dir := t.TempDir()
	p := config.DefaultPermissions()
	p.Shell = true
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if err := setCobrowsePublicOnly(dir, true); err != nil {
		t.Fatal(err)
	}
	got := config.LoadPermissions(dir)
	if !got.CobrowsePublicOnly || !got.Shell {
		t.Fatalf("after enable: %+v", got)
	}
	if err := setCobrowsePublicOnly(dir, false); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadPermissions(dir); got.CobrowsePublicOnly || !got.Shell {
		t.Fatalf("after disable: %+v", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "permissions.yaml"))
	if strings.Contains(string(data), "cobrowse_public_only") {
		t.Fatalf("a disabled setting should not be written: %s", data)
	}
}

func TestSetCobrowsePublicOnlyRefusesDamagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.yaml")
	if err := os.WriteFile(path, []byte("shell: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setCobrowsePublicOnly(dir, false); err == nil {
		t.Fatal("a damaged permissions file must not be rewritten from defaults")
	}
	if data, _ := os.ReadFile(path); string(data) != "shell: [\n" {
		t.Fatalf("file changed: %q", data)
	}
}

// TestLocalPermissionWritersRefuseDamagedFile: every local writer that loads
// and saves the whole policy refuses a damaged file instead of rewriting it
// from defaults (which would drop co-browse public-only and the rest).
func TestLocalPermissionWritersRefuseDamagedFile(t *testing.T) {
	const damaged = "cobrowse_public_only: true\nshell: [\n"
	writers := map[string]func(dir string) error{
		"shell":       func(dir string) error { _, err := setShellPermission(dir, true); return err },
		"passcode":    func(dir string) error { _, err := setNodePasscode(dir, "2468"); return err },
		"clear":       func(dir string) error { _, err := clearNodePasscode(dir); return err },
		"legacyClear": func(dir string) error { return clearLegacyPasscodeAt([]string{dir}) },
		"tui":         func(dir string) error { return savePermissionsChecked(dir, config.DefaultPermissions()) },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "permissions.yaml")
			if err := os.WriteFile(path, []byte(damaged), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := write(dir); err == nil {
				t.Fatal("a damaged permissions file must be refused")
			}
			if data, _ := os.ReadFile(path); string(data) != damaged {
				t.Fatalf("file rewritten: %q", data)
			}
		})
	}
}

// TestControlCenterSaveKeepsPublicOnlyChangedOnDisk: the Built-in Services
// modal loads the policy, the file changes on disk while it is open (here
// public-only and Shell turn on), then a toggle is saved. The toggle writes
// only its own field, and a whole-struct save still keeps public-only.
func TestControlCenterSaveKeepsPublicOnlyChangedOnDisk(t *testing.T) {
	dir := t.TempDir()
	if err := config.SavePermissions(dir, config.DefaultPermissions()); err != nil {
		t.Fatal(err)
	}
	modal := config.LoadPermissions(dir) // what the modal holds

	onDisk := config.LoadPermissions(dir)
	onDisk.CobrowsePublicOnly = true
	onDisk.Shell = true
	if err := config.SavePermissions(dir, onDisk); err != nil {
		t.Fatal(err)
	}

	if err := toggleNodePermission(dir, "Files", true); err != nil {
		t.Fatal(err)
	}
	got := config.LoadPermissions(dir)
	if !got.CobrowsePublicOnly || !got.Shell || !got.Files {
		t.Fatalf("toggle save: %+v", got)
	}

	modal.Console = true
	if err := savePermissionsChecked(dir, modal); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadPermissions(dir); !got.CobrowsePublicOnly || !got.Console {
		t.Fatalf("whole-struct save turned public-only off: %+v", got)
	}
	if err := toggleNodePermission(dir, "Nope", true); err == nil {
		t.Fatal("unknown toggle accepted")
	}
}
