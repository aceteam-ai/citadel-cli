package network

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func networkPathFixture(t *testing.T) (node, machine string) {
	t.Helper()
	node, machine = t.TempDir(), t.TempDir()
	restore, err := OverrideStateDirectoriesForTesting(node, machine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	})
	return node, machine
}

func TestStateFixtureBypassesDiscoveryBeforeInputsAreRead(t *testing.T) {
	node, machine := networkPathFixture(t)
	previous := globalConfigDirForState
	globalConfigDirForState = func() string { panic("machine pointer discovery reached") }
	t.Cleanup(func() { globalConfigDirForState = previous })
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ProgramData", "SUDO_USER"} {
		t.Setenv(key, "fixture-selector-must-not-be-used")
	}
	if got := GetStateDir(); got != filepath.Join(node, "network") {
		t.Fatalf("state directory = %q", got)
	}
	if got := GetNodeConfigDir(); got != node {
		t.Fatalf("node directory = %q", got)
	}
	if got := getNodeConfigDirFromGlobalConfig(); got != node {
		t.Fatalf("config discovery = %q", got)
	}
	if got := getGlobalConfigDirForState(); got != machine {
		t.Fatalf("machine config directory = %q", got)
	}
	if got := getGlobalConfigDir(); got != machine {
		t.Fatalf("reconnect global config directory = %q", got)
	}
	if got := getManifestLocations(); !reflect.DeepEqual(got, []string{filepath.Join(node, "citadel.yaml")}) {
		t.Fatalf("reconnect manifest lookup escaped fixture: %v", got)
	}
	// A fixture must not invoke real service-user lookup or ownership repair.
	FixStatePermissions()
}

func TestStateFixturePointerAndClearStayInOwnedDirectories(t *testing.T) {
	node, machine := networkPathFixture(t)
	foreign := t.TempDir() // A tripwire, never an actual host-node path.
	foreignState := filepath.Join(foreign, "network")
	if err := os.MkdirAll(foreignState, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(foreignState, "machine-key")
	const content = "foreign fixture must survive"
	if err := os.WriteFile(sentinel, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if got := machineStatePointerPath(); got != filepath.Join(machine, machineStatePointerFile) {
		t.Fatalf("pointer escaped fixture: %q", got)
	}
	if err := WriteMachineStatePointer(foreign); err != nil {
		t.Fatal(err)
	}
	if got := readMachineStatePointer(); got != foreign {
		t.Fatal("positive control did not write the fixture pointer")
	}
	if err := os.WriteFile(filepath.Join(machine, "config.yaml"), []byte("node_config_dir: "+foreign+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "citadel.yaml"), []byte("node:\n  name: foreign-hostname\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "citadel.yaml"), []byte("node:\n  name: owned-fixture-hostname\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := getHostnameForReconnect(); got != "owned-fixture-hostname" {
		t.Fatalf("reconnect followed a foreign manifest: %q", got)
	}
	expectedState := filepath.Join(node, "network")
	if got := GetStateDir(); got != expectedState {
		t.Fatalf("state creation would escape fixture: %q", got)
	}
	state, err := EnsureStateDir()
	if err != nil || state != filepath.Join(node, "network") {
		t.Fatalf("ensure fixture state = %q, %v", state, err)
	}
	if err := os.WriteFile(filepath.Join(state, "owned-key"), []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	if !HasState() {
		t.Fatal("positive control did not create fixture state")
	}
	if got := GetStateDir(); got != expectedState {
		t.Fatalf("state clearing would escape fixture: %q", got)
	}
	if err := ClearState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("clear did not remove owned state: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != content {
		t.Fatalf("foreign state changed: %v", err)
	}
}

func TestStateFixtureInvalidInstallPreservesCurrentScope(t *testing.T) {
	node, machine := networkPathFixture(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dirs := range [][2]string{{"", machine}, {"relative", machine}, {file, machine}, {node, ""}, {node, filepath.Join(t.TempDir(), "missing")}} {
		if restore, err := OverrideStateDirectoriesForTesting(dirs[0], dirs[1]); err == nil || restore != nil {
			t.Fatal("invalid fixture installation succeeded")
		}
		if GetNodeConfigDir() != node {
			t.Fatal("failed installation changed the active scope")
		}
	}
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(node, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if restore, err := OverrideStateDirectoriesForTesting(link, machine); err == nil || restore != nil {
			t.Fatal("symlink fixture was accepted")
		}
		if GetNodeConfigDir() != node {
			t.Fatal("symlink rejection changed the active scope")
		}
	})
}

func TestStateFixtureRestoreIsOrderedAndIdempotent(t *testing.T) {
	initial := stateDirectoriesForTests.Load()
	a, b, machine := t.TempDir(), t.TempDir(), t.TempDir()
	restoreA, err := OverrideStateDirectoriesForTesting(a, machine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreA(); err != nil {
			t.Error(err)
		}
	})
	restoreB, err := OverrideStateDirectoriesForTesting(b, machine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restoreB(); err != nil {
			t.Error(err)
		}
	})
	if err := restoreA(); err == nil || GetNodeConfigDir() != b {
		t.Fatal("out-of-order restore removed another active scope")
	}
	if err := restoreB(); err != nil || GetNodeConfigDir() != a {
		t.Fatal("inner restore did not recover the previous fixture")
	}
	if err := restoreA(); err != nil {
		t.Fatal(err)
	}
	if err := restoreA(); err != nil || stateDirectoriesForTests.Load() != initial {
		t.Fatal("restore was not idempotent or lost the original provider")
	}
}

func TestStateFixtureConcurrentPathReaders(t *testing.T) {
	a, machine := networkPathFixture(t)
	b := t.TempDir()
	bad := make(chan string, 1)
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 1000; j++ {
				got := GetNodeConfigDir()
				if got != a && got != b {
					select {
					case bad <- got:
					default:
					}
				}
			}
		}()
	}
	defer readers.Wait() // Also join before fixture cleanup after t.Fatal.
	for i := 0; i < 100; i++ {
		restore, err := OverrideStateDirectoriesForTesting(b, machine)
		if err != nil {
			t.Fatal(err)
		}
		if err := restore(); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait() // Join before the outer fixture and its directories are released.
	select {
	case got := <-bad:
		t.Fatalf("reader escaped active fixture paths: %q", got)
	default:
	}
}
