package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestRunLocalConfigUploadsReservedAndIsolated(t *testing.T) {
	if _, ok := findHarness("uploads"); ok {
		t.Fatal("uploads must not be a harness")
	}
	for _, args := range [][]string{{"uploads"}, {"uploads", "bad"}, {"uploads", "off", "extra"}, {"codex", "off"}} {
		if localConfigArgs(nil, args) == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"uploads", "on"}, {"uploads", "off"}, {"uploads", "status"}, {"codex"}} {
		if err := localConfigArgs(nil, args); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	harness := filepath.Join(root, "harness.json")
	const original = "HARNESS_SENTINEL"
	if err := os.WriteFile(harness, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "local")
	old := localStateDir
	localStateDir = func() string { return dir }
	t.Cleanup(func() { localStateDir = old; localConfigCmd.SetOut(nil) })
	t.Setenv("CITADEL_LOCAL_UPLOADS", "")
	var out bytes.Buffer
	localConfigCmd.SetOut(&out)
	for _, value := range []string{"status", "off", "on"} {
		out.Reset()
		if err := localConfigCmd.RunE(localConfigCmd, []string{"uploads", value}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Uploads are") {
			t.Fatalf("missing status: %s", out.String())
		}
		if !strings.Contains(out.String(), "this shell's environment") {
			t.Fatal("environment scope missing")
		}
		b, err := os.ReadFile(harness)
		if err != nil || string(b) != original {
			t.Fatal("harness changed")
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected files: %v %v", entries, err)
	}
	// File-on must still report environment-off; no restart or harness edit.
	t.Setenv("CITADEL_LOCAL_UPLOADS", "off")
	out.Reset()
	if err := localConfigCmd.RunE(localConfigCmd, []string{"uploads", "on"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "turned off by CITADEL_LOCAL_UPLOADS") {
		t.Fatalf("misreported: %s", out.String())
	}
}

func TestLocalMCPUploadsRuntimeCapturesEnvAndRereadsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	t.Setenv("CITADEL_LOCAL_UPLOADS", "off")
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return dir }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	t.Cleanup(rt.close)
	t.Setenv("CITADEL_LOCAL_UPLOADS", "")
	if !errors.Is(rt.svc.CheckUploads(), localcobrowse.ErrUploadsDisabled) {
		t.Fatal("owner environment not captured")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status created local state")
	}
	rt2 := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return dir }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	t.Cleanup(rt2.close)
	if !rt2.svc.UploadsAllowed() {
		t.Fatal("new owner default")
	}
	if err := localcobrowse.WriteUploadSetting(dir, "off"); err != nil {
		t.Fatal(err)
	}
	if rt2.svc.UploadsAllowed() {
		t.Fatal("file edit requires restart")
	}
	if err := localcobrowse.WriteUploadSetting(dir, "on"); err != nil {
		t.Fatal(err)
	}
	if !rt2.svc.UploadsAllowed() {
		t.Fatal("on edit not read")
	}
	if err := platform.ValidatePrivateFile(filepath.Join(dir, localcobrowse.UploadSettingsDir, localcobrowse.UploadSettingsFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, localcobrowse.UploadSettingsDir, localcobrowse.UploadSettingsFile), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rt2.svc.UploadsAllowed() {
		t.Fatal("corruption not refused")
	}
}

func TestLocalDoctorUploadsExplainsPolicy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	if err := localcobrowse.WriteUploadSetting(dir, "off"); err != nil {
		t.Fatal(err)
	}
	d := localDoctorDeps{
		discover: func() (platform.BrowserInfo, error) { return platform.BrowserInfo{}, nil },
		bindTest: func() error { return nil }, vault: func() (bool, error) { return false, nil },
		owner: func() error { return nil }, audit: func() (localaudit.VerifyResult, error) { return localaudit.VerifyResult{}, nil },
		uploads: localcobrowse.NewUploadSettings(dir, "PRIVATE_UNKNOWN").Status,
	}
	var out bytes.Buffer
	if err := runLocalDoctor(d, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "turned off in settings.json") || !strings.Contains(out.String(), "unrecognized value") || strings.Contains(out.String(), "PRIVATE_UNKNOWN") {
		t.Fatalf("bad doctor status: %s", out.String())
	}
	if !strings.Contains(out.String(), "this shell's environment") {
		t.Fatal("doctor environment scope missing")
	}
	if err := os.WriteFile(filepath.Join(dir, localcobrowse.UploadSettingsDir, localcobrowse.UploadSettingsFile), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runLocalDoctor(d, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unreadable, invalid") {
		t.Fatal("corruption not explained")
	}
}

func TestLocalMCPUploadsAfterOwnerDirectoryExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	// Windows MkdirAll inherits an unprotected ACL, exactly as the existing
	// owner, audit parents and viewer parents create the local container.
	owner, err := localcobrowse.AcquireOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Release)
	if err := localcobrowse.WriteUploadSetting(dir, "off"); err != nil {
		t.Fatal(err)
	}
	st := localcobrowse.NewUploadSettings(dir, "").Status()
	if st.Enabled {
		t.Fatal("existing owner directory prevented opt-out")
	}
	if err := platform.ValidatePrivateDir(filepath.Join(dir, localcobrowse.UploadSettingsDir)); err != nil {
		t.Fatal(err)
	}
}
