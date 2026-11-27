package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

func TestHarnessPermissionCommandIsExplicitAndLocal(t *testing.T) {
	c, _, err := permissionCmd.Find([]string{"harness"})
	if err != nil || c == permissionCmd || c.Name() != "harness" {
		t.Fatal("missing local harness permission command")
	}
}

func TestHarnessPermissionDefaultsOff(t *testing.T) {
	for _, contents := range []string{"", "shell: true\nconsole: true\nfiles: true\n", "harness: true\nfiles: [broken"} {
		dir := t.TempDir()
		if contents != "" {
			if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
		}
		p := config.LoadPermissions(dir)
		if p.Harness {
			t.Fatal("existing permissions enabled harness")
		}
		if err := config.SavePermissions(dir, p); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "permissions.yaml"))
		if strings.Contains(string(data), "harness: true") {
			t.Fatal("existing permissions enabled harness")
		}
	}
}

func TestHarnessPermissionCLIChangesOnlyTheLocalSetting(t *testing.T) {
	dir := t.TempDir()
	p := config.DefaultPermissions()
	p.Console, p.Files, p.HostNetwork, p.CobrowsePublicOnly = true, true, true, true
	p.SetupPosture, p.PasscodeHash = config.SetupPostureCobrowseOnly, "synthetic-hash"
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		var output bytes.Buffer
		c := newPermissionHarnessCommand(func() string { return dir })
		c.SetOut(&output)
		arg := "disable"
		if enabled {
			arg = "enable"
		}
		c.SetArgs([]string{arg})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		got, err := config.LoadPermissionsStrict(dir)
		want := *p
		want.Harness = enabled
		if err != nil || !reflect.DeepEqual(got, &want) {
			t.Fatalf("changed unrelated settings: %+v %v", got, err)
		}
		if !strings.Contains(output.String(), "Execution is not wired") || !strings.Contains(output.String(), enabledLabel(enabled)) {
			t.Fatalf("missing saved-intent limitation: %s", &output)
		}
		info, err := os.Stat(filepath.Join(dir, "permissions.yaml"))
		if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
			t.Fatalf("permissions not saved privately: %v %v", info, err)
		}
	}
}

func TestHarnessPermissionCLIRefusesInvalidArgumentsBeforeIO(t *testing.T) {
	for _, args := range [][]string{nil, {"enable", "extra"}, {"anything"}} {
		resolved := false
		c := newPermissionHarnessCommand(func() string { resolved = true; return t.TempDir() })
		c.SilenceErrors, c.SilenceUsage = true, true
		c.SetArgs(args)
		if err := c.Execute(); err == nil || resolved {
			t.Fatalf("bad arguments reached filesystem: %v %v", args, err)
		}
	}
}

func TestHarnessPermissionRefusesUnreadableOrUnsavablePolicy(t *testing.T) {
	for _, directory := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "permissions.yaml")
		body := []byte("harness: true\nfiles: [synthetic-malformed-value")
		if directory {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		p, err := setHarnessPermission(dir, true)
		if err == nil || p != nil || strings.Contains(err.Error(), "synthetic-malformed-value") {
			t.Fatalf("unreadable policy was accepted or leaked: %v %v", p, err)
		}
		if !directory {
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, body) {
				t.Fatal("damaged policy rewritten")
			}
		}
	}
	// A file cannot serve as the policy directory. Refuse it without returning
	// a partly updated permission structure.
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := setHarnessPermission(path, true); err == nil || p != nil {
		t.Fatalf("invalid destination accepted: %+v %v", p, err)
	}
}

func TestHarnessPermissionStaysLocalAcrossWholePolicySaves(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		dir := t.TempDir()
		if _, err := setHarnessPermission(dir, enabled); err != nil {
			t.Fatal(err)
		}
		stale := config.DefaultPermissions()
		stale.Harness = !enabled
		stale.Desktop = true
		if err := savePermissionsChecked(dir, stale); err != nil {
			t.Fatal(err)
		}
		got, err := config.LoadPermissionsStrict(dir)
		if err != nil || got.Harness != enabled || !got.Desktop {
			t.Fatalf("whole-policy save changed local harness setting: %+v %v", got, err)
		}
		if err := toggleNodePermission(dir, "Harness", !enabled); err == nil {
			t.Fatal("existing generic toggle gained harness enable")
		}
		if err := toggleNodePermission(dir, "Files", true); err != nil {
			t.Fatal(err)
		}
		got, err = config.LoadPermissionsStrict(dir)
		if err != nil || got.Harness != enabled || !got.Files {
			t.Fatalf("unrelated toggle changed harness: %+v %v", got, err)
		}
	}
}

func TestHarnessPermissionIsClearedByRestrictivePosture(t *testing.T) {
	dir := t.TempDir()
	if _, err := setHarnessPermission(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupPosture(dir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadPermissionsStrict(dir)
	if err != nil || got.Harness || got.Shell || got.Files || got.Console || got.Desktop {
		t.Fatalf("restrictive posture kept harness enabled: %+v %v", got, err)
	}
}

func TestHarnessPermissionRoundTripsIndependently(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.yaml")
	if err := os.WriteFile(path, []byte("harness: true\nconsole: false\ndesktop: false\nfiles: false\nshell: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := config.LoadPermissionsStrict(dir)
	if err != nil || p.Console || p.Desktop || p.Files || p.Shell {
		t.Fatal("harness must not imply another permission")
	}
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("harness: true")) {
		t.Fatal("harness setting did not persist")
	}
}
