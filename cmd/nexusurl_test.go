// cmd/nexusurl_test.go
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// withNexusWriteSeams overrides the machine-convergent write seam and the
// root-only chown to no-ops so a test never writes/reads through the real
// network.GetNodeConfigDir() (which on this dev box resolves to a LIVE node's
// config -- CLAUDE.md's ConfigDir()/GetNodeConfigDir() test rule). HOME is
// redirected too so seedAceteamAPIKeyFromLegacyFile's read of
// platform.ConfigDir() is hermetic.
func withNexusWriteSeams(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	origDir := nodeConfigDirFn
	origFix := fixStatePermissionsFn
	nodeConfigDirFn = func() string { return dir }
	fixStatePermissionsFn = func() {}
	t.Cleanup(func() {
		nodeConfigDirFn = origDir
		fixStatePermissionsFn = origFix
	})
}

// TestSaveNexusURLToConfig_PreservesAndPersists proves the read-modify-write
// keeps the existing device token AND persists nexus_url, and that the value
// reads back through the same getDeviceConfigFromFile path production uses.
func TestSaveNexusURLToConfig_PreservesAndPersists(t *testing.T) {
	tmp := t.TempDir()
	withNexusWriteSeams(t, tmp)

	// Pre-seed a device token so readDeviceConfigFromDirs recognizes the file.
	seed := "device_api_token: tok-123\norg_id: \"42\"\n"
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte(seed), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := saveNexusURLToConfig("https://nexus.box.internal"); err != nil {
		t.Fatalf("saveNexusURLToConfig: %v", err)
	}

	dc := readDeviceConfigFromDirs([]string{tmp})
	if dc == nil {
		t.Fatal("readDeviceConfigFromDirs returned nil")
	}
	if dc.NexusURL != "https://nexus.box.internal" {
		t.Fatalf("NexusURL = %q, want persisted value", dc.NexusURL)
	}
	if dc.DeviceAPIToken != "tok-123" {
		t.Fatalf("DeviceAPIToken = %q, want preserved token (read-modify-write must not drop it)", dc.DeviceAPIToken)
	}
}

// TestSaveNexusURLToConfig_CreatesFile covers the authkey-init shape where no
// device config was written first: saveNexusURLToConfig must MkdirAll and
// create config.yaml from scratch.
func TestSaveNexusURLToConfig_CreatesFile(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "does-not-exist-yet")
	withNexusWriteSeams(t, sub)

	if err := saveNexusURLToConfig("https://nexus.box.internal"); err != nil {
		t.Fatalf("saveNexusURLToConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(sub, "config.yaml"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var parsed struct {
		NexusURL string `yaml:"nexus_url"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.NexusURL != "https://nexus.box.internal" {
		t.Fatalf("nexus_url = %q, want the written URL", parsed.NexusURL)
	}
}

// TestNexusFlagMismatchError pins the refusal decision, including the BLOCKER
// case: after `citadel logout` HasState()==false, so a re-enroll to a different
// nexus (exactly what the error message tells the user to do) must NOT refuse.
func TestNexusFlagMismatchError(t *testing.T) {
	cases := []struct {
		name               string
		hasState, explicit bool
		flagURL, persisted string
		wantErr            bool
	}{
		{"flag not explicit", true, false, "https://a", "https://b", false},
		{"post-logout re-enroll (no state)", false, true, "https://a", "https://b", false},
		{"no persisted url", true, true, "https://a", "", false},
		{"match", true, true, "https://nexus.box", "https://nexus.box", false},
		{"match ignoring trailing slash", true, true, "https://nexus.box/", "https://nexus.box", false},
		{"differ refuses", true, true, "https://nexus.aceteam.ai", "https://nexus.box.internal", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nexusFlagMismatchError(tc.hasState, tc.explicit, tc.flagURL, tc.persisted)
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), tc.persisted) {
				t.Fatalf("error should name the persisted URL for re-enroll guidance: %v", err)
			}
		})
	}
}

// TestNexusPersistentFlagChangedDetection guards the single cobra assumption
// refuseNexusFlagMismatch rests on: a persistent flag declared on the PARENT is
// visible via cmd.Flags().Changed on the CHILD's *cobra.Command, and reflects
// whether the user actually passed it. If cobra ever stopped merging inherited
// persistent flags into Flags() before Run, the refusal would silently never
// fire (or always fire) -- this catches that.
func TestNexusPersistentFlagChangedDetection(t *testing.T) {
	run := func(args ...string) bool {
		var captured bool
		parent := &cobra.Command{Use: "parent"}
		var nexusVal string
		parent.PersistentFlags().StringVar(&nexusVal, "nexus", "https://default", "")
		child := &cobra.Command{Use: "child", Run: func(cmd *cobra.Command, _ []string) {
			captured = cmd.Flags().Changed("nexus")
		}}
		parent.AddCommand(child)
		parent.SetArgs(args)
		if err := parent.Execute(); err != nil {
			t.Fatalf("execute %v: %v", args, err)
		}
		return captured
	}
	if run("child") {
		t.Fatal("Changed must be false when --nexus is not passed")
	}
	if !run("child", "--nexus", "https://box") {
		t.Fatal("Changed must be true when --nexus is passed to a subcommand of the flag owner")
	}
}

// TestClearNodeDeviceFieldsPreservingNexusURL pins the --relogin interaction
// (citadel-cli#1110): device-auth fields are cleared but the control-plane
// binding (nexus_url) survives, so an interrupted relogin can't strand a
// self-hosted-nexus node on the default control URL.
func TestClearNodeDeviceFieldsPreservingNexusURL(t *testing.T) {
	t.Run("preserves nexus_url, drops device fields", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("device_api_token: tok\norg_id: \"42\"\nnexus_url: https://box\n"), 0600); err != nil {
			t.Fatal(err)
		}
		clearNodeDeviceFieldsPreservingNexusURL(p)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("file should still exist: %v", err)
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if m["nexus_url"] != "https://box" {
			t.Fatalf("nexus_url not preserved: %v", m)
		}
		if _, ok := m["device_api_token"]; ok {
			t.Fatal("device_api_token should be cleared")
		}
		if _, ok := m["org_id"]; ok {
			t.Fatal("org_id should be cleared")
		}
	})
	t.Run("removes file when there is no nexus_url to preserve", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte("device_api_token: tok\n"), 0600); err != nil {
			t.Fatal(err)
		}
		clearNodeDeviceFieldsPreservingNexusURL(p)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("file should be removed when no nexus_url to preserve (err=%v)", err)
		}
	})
}

func TestNormalizeControlURL(t *testing.T) {
	if got := normalizeControlURL("  https://x/ "); got != "https://x" {
		t.Fatalf("trailing slash + space not normalized: %q", got)
	}
	if normalizeControlURL("https://x") != normalizeControlURL("https://x/") {
		t.Fatal("trailing slash should compare equal")
	}
	if normalizeControlURL("https://a") == normalizeControlURL("https://b") {
		t.Fatal("genuinely different hosts must not normalize equal")
	}
}
