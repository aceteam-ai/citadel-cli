package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestRemoveDeviceCredentialsInEveryDir: Disconnect cleans the node config
// dir and the legacy invoker config dir the worker falls back to.
func TestRemoveDeviceCredentialsInEveryDir(t *testing.T) {
	node, legacy := t.TempDir(), t.TempDir()
	for _, d := range []string{node, legacy} {
		if err := os.WriteFile(filepath.Join(d, "config.yaml"), []byte("device_api_token: tok-"+filepath.Base(d)+"\nnode_config_dir: "+node+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := removeDeviceCredentialsIn([]string{node, legacy, node, ""})
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	for _, d := range []string{node, legacy} {
		got, _ := os.ReadFile(filepath.Join(d, "config.yaml"))
		if bytes.Contains(got, []byte("device_api_token")) || !bytes.Contains(got, []byte("node_config_dir")) {
			t.Fatalf("%s:\n%s", d, got)
		}
	}
	if dirs := deviceConfigDirs(); len(dirs) != 2 {
		t.Fatalf("Disconnect must clean every dir the worker reads credentials from: %v", dirs)
	}
}

// TestDisconnectSaysThePostureStays: the result says the co-browse-only
// setting stays and how to remove it.
func TestDisconnectSaysThePostureStays(t *testing.T) {
	var calls []string
	d := recordDisconnect(&calls, nil, nil, nil, true, logoutOutcome{})
	d.postureKept = func() bool { return true }
	if res := disconnectThisPC(d); !strings.Contains(res.Message, "citadel permission setup-posture disable") {
		t.Fatalf("no posture note: %q", res.Message)
	}
	d.postureKept = func() bool { return false }
	if res := disconnectThisPC(d); strings.Contains(res.Message, "setup-posture") {
		t.Fatalf("posture note without a posture: %q", res.Message)
	}
}

// TestClearSetupPosture removes only the marker and refuses a damaged file.
func TestClearSetupPosture(t *testing.T) {
	dir := t.TempDir()
	if err := writeSetupPosture(dir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := clearSetupPosture(dir); err != nil {
		t.Fatal(err)
	}
	p, err := config.LoadPermissionsStrict(dir)
	if err != nil || p.SetupPosture != "" || !p.CobrowsePublicOnly || p.Services || p.Files {
		t.Fatalf("clear changed more than the marker: %+v %v", p, err)
	}
	bad := t.TempDir()
	body := []byte("setup_posture: cobrowse_only\nfiles: [\n")
	if err := os.WriteFile(filepath.Join(bad, "permissions.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearSetupPosture(bad); err == nil {
		t.Fatal("cleared over a damaged file")
	}
	if got, _ := os.ReadFile(filepath.Join(bad, "permissions.yaml")); !bytes.Equal(got, body) {
		t.Fatal("a damaged file was rewritten")
	}
}

// TestControlCenterSaveIsStrictAndKeepsThePosture: a Control Center save
// from a stale copy keeps the marker and the public-only guard as they are
// on disk, and never rewrites a damaged file.
func TestControlCenterSaveIsStrictAndKeepsThePosture(t *testing.T) {
	dir := t.TempDir()
	stale := config.DefaultPermissions() // loaded before the setup page connected
	if err := writeSetupPosture(dir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	stale.Desktop = true
	if err := saveControlCenterPermissions(dir, stale); err != nil {
		t.Fatal(err)
	}
	p, err := config.LoadPermissionsStrict(dir)
	if err != nil || p.SetupPosture == "" || !p.CobrowsePublicOnly || !p.Desktop {
		t.Fatalf("save from a stale copy: %+v %v", p, err)
	}
	bad := t.TempDir()
	body := []byte("setup_posture: cobrowse_only\nshell: [\n")
	if err := os.WriteFile(filepath.Join(bad, "permissions.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveControlCenterPermissions(bad, config.DefaultPermissions()); err == nil {
		t.Fatal("saved over a damaged file")
	}
	if got, _ := os.ReadFile(filepath.Join(bad, "permissions.yaml")); !bytes.Equal(got, body) {
		t.Fatal("a damaged file was rewritten")
	}
	// A missing file is fine (a fresh node).
	if err := saveControlCenterPermissions(t.TempDir(), config.DefaultPermissions()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh node: %v", err)
	}
}

// TestSetupPostureCacheSeesChanges: the per-job posture check follows a
// change to the permissions file.
func TestSetupPostureCacheSeesChanges(t *testing.T) {
	dir := t.TempDir()
	var c setupPostureCache
	if c.active(dir) {
		t.Fatal("no file reads as a posture")
	}
	if err := writeSetupPosture(dir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !c.active(dir) {
		t.Fatal("the cache missed the posture write")
	}
	if err := clearSetupPosture(dir); err != nil {
		t.Fatal(err)
	}
	if c.active(dir) {
		t.Fatal("the cache missed the posture clear")
	}
}
