package cmd

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/clilog"
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
	// Seed only the explicitly owned path. Calling the production pointer
	// writer here would itself be unsafe if its directory seam regressed.
	if err := os.WriteFile(filepath.Join(home, "machine-config", "state-dir"), []byte(foreign+"\n"), 0644); err != nil {
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

func TestCommandHomeFixturePrecedesDefaultReadsAndWrites(t *testing.T) {
	home := t.TempDir()
	useCmdFixtureHome(t, home)
	foreign := t.TempDir()
	foreignLog := filepath.Join(foreign, ".citadel-cli", "logs", "latest.log")
	if err := os.MkdirAll(filepath.Dir(foreignLog), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignLog, []byte("foreign synthetic log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Change only selectors, not the explicitly installed providers.
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(key, foreign)
	}
	t.Setenv("SUDO_USER", "fixture-account-that-must-not-be-looked-up")
	t.Setenv("CITADEL_WORKSPACE", "")
	t.Setenv("CITADEL_NODE_DIR", "")
	previousWorkspace := workWorkspaceDir
	workWorkspaceDir = ""
	t.Cleanup(func() { workWorkspaceDir = previousWorkspace })
	// Assert each selected default BEFORE calling helpers that read/create.
	if err := checkCmdPathFixtures(home, filepath.Join(home, "network-node-fixture"), filepath.Join(home, ".citadel-cli"), filepath.Join(home, "podman-marker")); err != nil {
		t.Fatal(err)
	}
	if userHomeDir() != home || workspaceDirPath() != filepath.Join(home, "citadel-node", "workspace") {
		t.Fatal("memory/workspace default ignored owned home")
	}
	if got, err := cacheBaseDirectory(); err != nil || got != filepath.Join(home, "citadel-cache") {
		t.Fatal("cache writer did not select owned directory before writing")
	}
	if dir, source, err := resolveNodeConfigDirReadOnly(); err != nil || source != nodeDirDefault || dir != filepath.Join(home, "citadel-node") {
		t.Fatalf("manifest default ignored owned home: %q %v", dir, err)
	}
	if clilog.Dir() != filepath.Join(home, ".citadel-cli", "logs") {
		t.Fatal("log writer did not select the owned directory before writing")
	}
	if err := prepareCacheDirectories(); err != nil {
		t.Fatal(err)
	}
	if got := resolveWorkspaceDir(); got != filepath.Join(home, "citadel-node", "workspace") {
		t.Fatal("workspace positive control did not create the owned directory")
	}
	clilog.Write("", "owned synthetic log")
	if err := clilog.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, "citadel-cache", "huggingface"), filepath.Join(home, "citadel-node", "workspace")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("positive control did not write owned fixture: %v", err)
		}
	}
	logs, err := filepath.Glob(filepath.Join(home, ".citadel-cli", "logs", "citadel-*.log"))
	if err != nil || len(logs) == 0 {
		t.Fatal("positive control did not create an owned log")
	}
	found := false
	for _, path := range logs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		found = found || bytes.Contains(data, []byte("owned synthetic log"))
	}
	if !found {
		t.Fatal("positive control did not write the owned log entry")
	}
	if _, err := os.Stat(filepath.Join(foreign, "citadel-cache")); !os.IsNotExist(err) {
		t.Fatalf("cache write escaped to foreign selector: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, "citadel-node")); !os.IsNotExist(err) {
		t.Fatalf("workspace write escaped to foreign selector: %v", err)
	}
	if got, err := os.ReadFile(foreignLog); err != nil || string(got) != "foreign synthetic log\n" {
		t.Fatal("foreign synthetic log changed")
	}
}

func TestCommandHomeCallersCannotBypassFixtureResolver(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, "../internal/clilog/clilog.go")
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		aliases := map[string]bool{}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os"` {
				name := "os"
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == "." {
					t.Fatalf("%s: dot-import of os bypasses the home fixture audit", path)
				}
				aliases[name] = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "UserHomeDir" {
				if id, ok := sel.X.(*ast.Ident); ok && aliases[id.Name] {
					t.Errorf("%s: use platform.HomeDir instead of os.UserHomeDir", path)
				}
			}
			return true
		})
	}
}

func TestCommandPathFixtureFailedSetupRestoresPreviousProviders(t *testing.T) {
	beforeNode, beforeConfig := network.GetNodeConfigDir(), platform.ConfigDir()
	beforeHome, _ := platform.HomeDir("")
	beforeMarker := podmanUserMarkerPath()
	node, machine := t.TempDir(), t.TempDir()
	home, configDir, markerDir := t.TempDir(), t.TempDir(), t.TempDir()
	missing := filepath.Join(t.TempDir(), "absent")
	for _, paths := range [][3]string{{home, missing, markerDir}, {missing, configDir, markerDir}, {home, configDir, missing}} {
		cleanup, err := installCmdPathFixtures(paths[0], node, machine, paths[1], paths[2])
		if err == nil || cleanup != nil {
			t.Fatal("invalid path fixture unexpectedly installed")
		}
		gotHome, _ := platform.HomeDir("")
		if network.GetNodeConfigDir() != beforeNode || platform.ConfigDir() != beforeConfig || gotHome != beforeHome || podmanUserMarkerPath() != beforeMarker {
			t.Fatal("partial setup lost a previous path provider")
		}
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
