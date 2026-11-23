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

func TestApplianceProfileRestrictsAndPreservesSettings(t *testing.T) {
	node, meeting := t.TempDir(), t.TempDir()
	path := filepath.Join(node, "config.yaml")
	if err := os.WriteFile(path, []byte("device_api_token: fixture-not-a-real-credential\nother_setting: kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := config.DefaultPermissions()
	p.Files, p.Shell, p.Console, p.Desktop = true, true, true, true
	if err := config.SavePermissions(node, p); err != nil {
		t.Fatal(err)
	}
	updates := 0
	enable := func() error { updates++; return nil }
	var out bytes.Buffer
	options := applianceOptions{slots: 1, window: "1280,720"}
	if err := applyApplianceProfile(node, meeting, options, enable, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Changed:") || !strings.Contains(out.String(), "Auto-update: on") {
		t.Fatal(out.String())
	}
	p, err := config.LoadPermissionsStrict(node)
	if err != nil {
		t.Fatal(err)
	}
	if p.Files || p.Shell || p.Console || p.Desktop || p.Services || p.SSH || p.Provision || !p.CobrowsePublicOnly || p.SetupPosture != config.SetupPostureCobrowseOnly {
		t.Fatalf("unsafe posture: %+v", p)
	}
	if config.LoadMeeting(meeting).MeetingEnabled {
		t.Fatal("meeting capability must be off")
	}
	values, err := readApplianceConfig(node)
	if err != nil {
		t.Fatal(err)
	}
	if values["device_api_token"] != "fixture-not-a-real-credential" || values["other_setting"] != "kept" || values["browser_appliance"] != true || values["browser_slots"] != 1 || values["browser_window_size"] != "1280,720" {
		t.Fatal("configuration was not preserved")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	// Omitting the optional size on a retry preserves the prior choice.
	if err := applyApplianceProfile(node, meeting, applianceOptions{slots: 1}, enable, &out); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || updates != 2 || strings.Contains(out.String(), "Changed:") {
		t.Fatalf("not idempotent: %s", &out)
	}
	if defaults := config.DefaultPermissions(); !defaults.Services || !defaults.SSH || !defaults.Provision || defaults.CobrowsePublicOnly {
		t.Fatal("global defaults changed")
	}
}

func TestApplianceProfileRefusesDamagedInputsBeforeWriting(t *testing.T) {
	for _, name := range []string{"permissions.yaml", "config.yaml"} {
		t.Run(name, func(t *testing.T) {
			node, meeting := t.TempDir(), t.TempDir()
			path := filepath.Join(node, name)
			bad := []byte("[malformed:")
			if err := os.WriteFile(path, bad, 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			err := applyApplianceProfile(node, meeting, applianceOptions{slots: 1}, func() error { called = true; return nil }, &bytes.Buffer{})
			after, _ := os.ReadFile(path)
			if err == nil || called || !bytes.Equal(bad, after) {
				t.Fatalf("damaged input rewritten: %v", err)
			}
			if _, err := os.Stat(filepath.Join(meeting, "meeting.yaml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("wrote meeting configuration after refusal")
			}
		})
	}
}

func TestApplianceOptionsValidation(t *testing.T) {
	for _, o := range []applianceOptions{{slots: 0}, {slots: 65}, {slots: 1, window: "--bad"}, {slots: 1, window: "1280,0"}, {slots: 1, window: "8193,720"}, {slots: 1, window: "+1280,720"}} {
		if validateApplianceOptions(o) == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
	for _, o := range []applianceOptions{{slots: 1}, {slots: 64, window: "1920,1080"}} {
		if err := validateApplianceOptions(o); err != nil {
			t.Fatal(err)
		}
	}
	cmd := newApplianceCommand()
	apply, _, err := cmd.Find([]string{"apply"})
	if err != nil || apply.Flags().Lookup("browser-slots").DefValue != "1" {
		t.Fatal("command must default to one slot")
	}
}

func TestApplianceUpdateFailureLeavesRestrictivePosture(t *testing.T) {
	node, meeting := t.TempDir(), t.TempDir()
	err := applyApplianceProfile(node, meeting, applianceOptions{slots: 1}, func() error { return errors.New("fake unavailable") }, &bytes.Buffer{})
	if err == nil {
		t.Fatal("must report update preference failure")
	}
	p, err := config.LoadPermissionsStrict(node)
	if err != nil || p.Files || p.Shell || !p.CobrowsePublicOnly {
		t.Fatalf("partial apply must remain restrictive: %v", err)
	}
	if values, err := readApplianceConfig(node); err != nil || values["browser_appliance"] == true {
		t.Fatal("failed apply must not publish completion marker")
	}
}

func TestApplianceBrowserSettingsDoNotRequireEnrollment(t *testing.T) {
	values := map[string]any{"browser_appliance": true, "browser_slots": 2, "browser_window_size": "1280,720"}
	if slots, window := browserRuntimeSettings(nil, values); slots != 2 || window != "1280,720" {
		t.Fatalf("pre-enrollment settings not applied: %d %q", slots, window)
	}
	values["browser_appliance"] = false
	if slots, window := browserRuntimeSettings(&DeviceConfig{BrowserSlots: 3}, values); slots != 3 || window != "" {
		t.Fatalf("ordinary defaults changed: %d %q", slots, window)
	}
	values["browser_appliance"] = true
	values["browser_slots"], values["browser_window_size"] = 999, "1280,720 --bad"
	if slots, window := browserRuntimeSettings(nil, values); slots != 0 || window != "" {
		t.Fatalf("malformed settings accepted: %d %q", slots, window)
	}
}

func TestApplianceConfigRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("other: kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "config.yaml")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := readApplianceConfig(dir); err == nil {
		t.Fatal("symlink configuration must be refused")
	}
}
