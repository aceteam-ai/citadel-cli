package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestCommandPathFixtureSurvivesChangedHomeAndHostPointer(t *testing.T) {
	home := t.TempDir()
	useCmdFixtureHome(t, home)
	node := filepath.Join(home, "network-node-fixture")
	state := filepath.Join(node, "network")
	configDir := filepath.Join(home, ".citadel-cli")
	foreign := t.TempDir() // A synthetic host tripwire; never the real node.
	foreignState := filepath.Join(foreign, "network")
	for _, dir := range []string{state, foreignState, filepath.Join(foreign, ".citadel-cli")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	foreignConfig := []byte("device_api_token: foreign-fixture\nnode_config_dir: " + foreign + "\n")
	foreignKey := []byte("foreign fixture machine key")
	tripwires := map[string][]byte{
		filepath.Join(foreign, "config.yaml"):                 foreignConfig,
		filepath.Join(foreign, ".citadel-cli", "config.yaml"): foreignConfig,
		filepath.Join(foreignState, "machine-key"):            foreignKey,
	}
	for path, data := range tripwires {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := network.WriteMachineStatePointer(foreign); err != nil {
		t.Fatal(err)
	}
	// Unlike useCmdFixtureHome, these mutations intentionally do NOT select a
	// new provider. Neither missing HOME config nor sudo/Windows/manifest
	// selectors may reopen production host discovery in this test process.
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ProgramData", "CITADEL_NODE_DIR"} {
		t.Setenv(key, foreign)
	}
	t.Setenv("SUDO_USER", "fixture-account-must-not-be-resolved")
	if network.GetStateDir() != state || network.GetNodeConfigDir() != node {
		t.Fatal("network paths followed a host selector instead of the fixture")
	}
	if platform.ConfigDir() != configDir || !reflect.DeepEqual(platform.ConfigDirCandidates(), []string{configDir}) {
		t.Fatal("platform discovery exposed a host fallback")
	}
	if got := deviceConfigDirs(); !reflect.DeepEqual(got, []string{node, configDir}) {
		t.Fatalf("credential readers escaped fixtures: %v", got)
	}
	// Exercise the real command writer and destructive network-state helper,
	// rather than checking only the injected path strings.
	ownedConfig := filepath.Join(node, "config.yaml")
	if err := os.WriteFile(ownedConfig, []byte("device_api_token: owned-fixture\nnexus_url: https://fixture.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "owned-key"), []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	clearNodeDeviceConfig()
	if data, err := os.ReadFile(ownedConfig); err != nil || bytes.Contains(data, []byte("device_api_token")) || !bytes.Contains(data, []byte("nexus_url")) {
		t.Fatalf("positive control did not clear only owned credentials: %v", err)
	}
	if err := network.ClearState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("owned state not cleared: %v", err)
	}
	for path, want := range tripwires {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("foreign tripwire changed: %v", err)
		}
	}
}

func TestCommandPathFixtureFailedSetupRestoresPreviousProviders(t *testing.T) {
	beforeNode, beforeConfig := network.GetNodeConfigDir(), platform.ConfigDir()
	node, machine := t.TempDir(), t.TempDir()
	cleanup, err := installCmdPathFixtures(node, machine, filepath.Join(t.TempDir(), "absent"))
	if err == nil || cleanup != nil {
		t.Fatal("invalid platform fixture unexpectedly installed")
	}
	if network.GetNodeConfigDir() != beforeNode || platform.ConfigDir() != beforeConfig {
		t.Fatal("partial setup lost the previous network/platform providers")
	}
}

func TestDiagnosticHandlersUseExplicitFixtureInputs(t *testing.T) {
	indexes := filepath.Join(t.TempDir(), "indexes")
	for _, shellEnabled := range []bool{false, true} {
		handlers := buildDiagnosticJobHandlers(shellEnabled, indexes)
		shell, ok := handlers["SHELL_COMMAND"].(*jobs.ShellCommandHandler)
		if !ok || shell.Disabled == shellEnabled || shell.Enabled == nil || shell.State == nil || shell.Throttle != nodePasscodeThrottle {
			t.Fatal("deferred construction changed the shell permission/passcode gate")
		}
		index, ok := handlers["FILE_INDEX"].(*jobs.FileIndexHandler)
		if !ok || index.IndexesDir != indexes {
			t.Fatal("file index did not use the explicit fixture")
		}
		search, ok := handlers["FILE_SEMANTIC_SEARCH"].(*jobs.FileSemanticSearchHandler)
		if !ok || search.IndexesDir != indexes {
			t.Fatal("semantic search did not use the explicit fixture")
		}
	}
}

func TestDiagnosticRegistryWasInitializedAfterFixtureSelection(t *testing.T) {
	// No handler runs: inspect the real TestMain-built registry to catch a
	// reintroduced init-time host-path capture.
	indexes := jobs.IndexesDirFor(network.GetNodeConfigDir())
	index, ok := jobHandlers["FILE_INDEX"].(*jobs.FileIndexHandler)
	if !ok || index.IndexesDir != indexes {
		t.Fatal("diagnostic index captured a pre-fixture path")
	}
	search, ok := jobHandlers["FILE_SEMANTIC_SEARCH"].(*jobs.FileSemanticSearchHandler)
	if !ok || search.IndexesDir != indexes {
		t.Fatal("diagnostic search captured a pre-fixture path")
	}
}
