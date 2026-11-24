//go:build linux || darwin

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/agentsprobe"
	"github.com/aceteam-ai/citadel-cli/internal/harnessseat"
	"github.com/spf13/cobra"
)

func seatCLIForTest(t *testing.T, owner seatCLIOwner, args ...string) (string, error) {
	t.Helper()
	command := newHarnessSeatRootWithTerminal(func() (seatCLIOwner, error) { return owner, nil }, func(*cobra.Command) bool { return true })
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.ExecuteContext(context.Background())
	return output.String(), err
}

func seatCLIState(t *testing.T) (string, seatCLIOwner) {
	t.Helper()
	home := t.TempDir()
	registry := filepath.Join(home, "registry")
	if err := os.Mkdir(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	return registry, seatCLIOwner{Name: "fixture-user", Home: home}
}

func TestHarnessSeatCLIRejectsSymlinkedOrWritableRegistryAncestors(t *testing.T) {
	for _, kind := range []string{"outside-home", "symlink", "writable-parent"} {
		t.Run(kind, func(t *testing.T) {
			registry, owner := seatCLIState(t)
			switch kind {
			case "outside-home":
				registry = t.TempDir()
			case "symlink":
				alias := filepath.Join(owner.Home, "alias")
				if err := os.Symlink(owner.Home, alias); err != nil {
					t.Fatal(err)
				}
				registry = filepath.Join(alias, "registry")
			case "writable-parent":
				if err := os.Chmod(owner.Home, 0o777); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); !errors.Is(err, harnessseat.ErrPrivateStore) {
				t.Fatal("unsafe registry path accepted")
			}
			if _, err := os.Stat(filepath.Join(registry, "harness.yaml")); !os.IsNotExist(err) {
				t.Fatal("refused path wrote registry")
			}
		})
	}
}

func TestHarnessSeatCLIInitAndListAreExplicitAndDoNotAdoptVendorHomes(t *testing.T) {
	registry, owner := seatCLIState(t)
	vendor := filepath.Join(owner.Home, ".vendor-fixture")
	if err := os.Mkdir(vendor, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(vendor, "untouched.fixture")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := seatCLIForTest(t, owner, "seat", "list", "--registry-dir", registry); !errors.Is(err, harnessseat.ErrRegistryMissing) {
		t.Fatal("list initialized missing registry")
	}
	if _, err := os.Stat(filepath.Join(registry, "harness.yaml")); !os.IsNotExist(err) {
		t.Fatal("list created registry")
	}
	if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); err != nil {
		t.Fatal(err)
	}
	out, err := seatCLIForTest(t, owner, "seat", "list", "--registry-dir", registry, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		StatusSource string           `json:"status_source"`
		Seats        []seatCLIListRow `json:"seats"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.StatusSource != "registry_declaration" || len(result.Seats) != 0 {
		t.Fatal("list invented probed/default seats")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatal("CLI changed vendor state")
	}
}

func TestHarnessSeatCLIListsMetadataAndDisablesRemovesWithoutOpeningVendorDirectory(t *testing.T) {
	registry, owner := seatCLIState(t)
	resolve := func(name string) (string, error) {
		if name != owner.Name {
			return "", errSeatCLIOwner
		}
		return owner.Home, nil
	}
	store, err := harnessseat.OpenSaltStore(registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	before, err := store.InitializeRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	// This deliberately absent vendor path proves list/disable/remove do not
	// require it, create it, or inspect its credential files.
	seat := harnessseat.Seat{Identity: harnessseat.Identity{ID: "fixture-seat", Harness: harnessseat.Codex, Owner: owner.Name, ConfigDir: filepath.Join(owner.Home, "absent-vendor")}, Label: "Personal", Entitlement: "consumer", Roots: []string{filepath.Join(owner.Home, "checkout")}, DataCeiling: "public", AccountDigest: strings.Repeat("a", 64), Auth: harnessseat.AuthSignedIn, Enabled: true}
	if _, err := store.UpdateRegistry(context.Background(), before, []harnessseat.Seat{seat}); err != nil {
		t.Fatal(err)
	}
	out, err := seatCLIForTest(t, owner, "seat", "list", "--registry-dir", registry, "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{owner.Home, owner.Name, seat.AccountDigest, "config_dir", "account_digest"} {
		if strings.Contains(out, secret) {
			t.Fatal("local list leaked private paths/account binding")
		}
	}
	if !strings.Contains(out, "fixture-seat") || !strings.Contains(out, "registry_declaration") {
		t.Fatal("missing local identity or status provenance")
	}
	if _, err := seatCLIForTest(t, owner, "seat", "disable", "fixture-seat", "--registry-dir", registry); err != nil {
		t.Fatal(err)
	}
	current, err := store.LoadRegistry(context.Background(), resolve)
	if err != nil || current.Seats()[0].Enabled {
		t.Fatal("CLI did not disable selected declaration")
	}
	if _, err := seatCLIForTest(t, owner, "seat", "remove", "fixture-seat", "--registry-dir", registry); err != nil {
		t.Fatal(err)
	}
	current, err = store.LoadRegistry(context.Background(), resolve)
	if err != nil || len(current.Seats()) != 0 {
		t.Fatal("CLI removal did not persist")
	}
	if _, err := os.Stat(seat.ConfigDir); !os.IsNotExist(err) {
		t.Fatal("CLI created vendor directory")
	}
}

func TestHarnessSeatCLIRefusesUnknownOwnerAndRequiresExplicitDirectory(t *testing.T) {
	registry, owner := seatCLIState(t)
	for _, args := range [][]string{{"seat", "init"}, {"seat", "init", "extra", "--registry-dir", registry}, {"seat", "remove", "--registry-dir", registry}, {"seat", "probe", "--registry-dir", registry}} {
		if _, err := seatCLIForTest(t, owner, args...); err == nil {
			t.Fatal("invalid or unimplemented command accepted")
		}
	}
	command := newHarnessSeatRoot(func() (seatCLIOwner, error) { return seatCLIOwner{}, errSeatCLIOwner })
	command.SetArgs([]string{"seat", "init", "--registry-dir", registry})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); !errors.Is(err, errSeatCLIOwner) {
		t.Fatal("unknown owner accepted")
	}
	entries, err := os.ReadDir(registry)
	if err != nil || len(entries) != 0 {
		t.Fatal("refused command touched registry")
	}
}

func TestHarnessSeatCLIRejectsAnotherOwnersDeclarationsAndUnsafeStore(t *testing.T) {
	registry, owner := seatCLIState(t)
	if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); err != nil {
		t.Fatal(err)
	}
	store, err := harnessseat.OpenSaltStore(registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	resolve := func(string) (string, error) { return owner.Home, nil }
	before, err := store.LoadRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	seat := harnessseat.Seat{Identity: harnessseat.Identity{ID: "other-seat", Harness: harnessseat.Codex, Owner: "other-user", ConfigDir: filepath.Join(owner.Home, "other-seat")}, Label: "Other", Entitlement: "consumer", DataCeiling: "public", Auth: harnessseat.AuthUnknown}
	if _, err := store.UpdateRegistry(context.Background(), before, []harnessseat.Seat{seat}); err != nil {
		t.Fatal(err)
	}
	if _, err := seatCLIForTest(t, owner, "seat", "list", "--registry-dir", registry); err == nil {
		t.Fatal("another owner's declaration accepted")
	}
	if err := os.Chmod(registry, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); !errors.Is(err, harnessseat.ErrPrivateStore) {
		t.Fatal("public registry repaired or accepted")
	}
}

func TestHarnessSeatCLILeavesSkipUnimplementedAndRetainsHPCPositiveList(t *testing.T) {
	for _, leaf := range []string{"init", "list", "add", "enable", "disable", "remove"} {
		path := "citadel harness seat " + leaf
		root := &cobra.Command{Use: "citadel"}
		harness := &cobra.Command{Use: "harness"}
		seat := &cobra.Command{Use: "seat"}
		command := &cobra.Command{Use: leaf}
		root.AddCommand(harness)
		harness.AddCommand(seat)
		seat.AddCommand(command)
		if !isHarnessSeatCLI(command) || hpcProfileAllowsCommand(path) {
			t.Fatal("startup scope or HPC gate changed")
		}
	}
	for _, path := range []string{"citadel harness", "citadel harness seat", "citadel harness seat probe", "citadel harness seat adopt", "citadel other seat list"} {
		root := &cobra.Command{Use: "citadel"}
		parent := root
		for _, name := range strings.Fields(path)[1:] {
			command := &cobra.Command{Use: name}
			parent.AddCommand(command)
			parent = command
		}
		if isHarnessSeatCLI(parent) {
			t.Fatal("unknown path bypassed startup")
		}
	}
}

func TestHarnessSeatCLIRootStartupDoesNotReadDeviceCredentialsOrRunUpdater(t *testing.T) {
	t.Setenv(envProfile, "")
	oldProfile := profileFlag
	profileFlag = ""
	t.Cleanup(func() { profileFlag = oldProfile })
	oldConfig, oldUpdate := deviceConfigForCATrustFn, checkForUpdateOnStartupFn
	t.Cleanup(func() { deviceConfigForCATrustFn = oldConfig; checkForUpdateOnStartupFn = oldUpdate })
	deviceConfigForCATrustFn = func() *DeviceConfig { t.Fatal("seat CLI read device credentials"); return nil }
	checkForUpdateOnStartupFn = func(bool) { t.Fatal("seat CLI started updater") }
	root := &cobra.Command{Use: "citadel"}
	harness := newHarnessSeatRoot(func() (seatCLIOwner, error) {
		t.Fatal("startup resolved owner before RunE")
		return seatCLIOwner{}, nil
	})
	root.AddCommand(harness)
	for _, leaf := range []string{"init", "list", "add", "enable", "disable", "remove"} {
		command, _, err := root.Find([]string{"harness", "seat", leaf})
		if err != nil {
			t.Fatal(err)
		}
		rootCmd.PersistentPreRun(command, nil)
	}
}

func TestHarnessSeatCLIProductionOwnerPathRefusesElevationAndIgnoresEnvironment(t *testing.T) {
	lookup := func(string) (*user.User, error) { t.Fatal("elevated caller reached passwd lookup"); return nil, nil }
	resolve := func(agentsprobe.ResolveInputs) (agentsprobe.Target, error) {
		t.Fatal("elevated caller reached owner resolution")
		return agentsprobe.Target{}, nil
	}
	for _, ids := range [][2]int{{0, 0}, {1001, 0}, {0, 1001}, {-1, -1}} {
		if _, err := resolveSeatCLIProcessOwner(ids[0], ids[1], lookup, resolve); !errors.Is(err, errSeatCLIOwner) {
			t.Fatal("elevated/unresolved owner accepted")
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", "other-user")
	home := t.TempDir()
	lookup = func(uid string) (*user.User, error) {
		if uid != "1001" {
			t.Fatal("lookup did not use actual process uid")
		}
		return &user.User{Username: "fixture-user"}, nil
	}
	resolve = func(in agentsprobe.ResolveInputs) (agentsprobe.Target, error) {
		if in.ConfiguredUser != "fixture-user" || in.ProcessUID != 1001 || in.SudoUser != "" || in.StateDir != "" || in.NodeConfigDir != "" {
			t.Fatal("owner resolution used an environment or node fallback")
		}
		return agentsprobe.Target{Username: "fixture-user", UID: 1001, HomeDir: home}, nil
	}
	owner, err := resolveSeatCLIProcessOwner(1001, 1001, lookup, resolve)
	if err != nil || owner.Name != "fixture-user" || owner.Home != home {
		t.Fatal("trusted owner result lost")
	}
	resolve = func(agentsprobe.ResolveInputs) (agentsprobe.Target, error) {
		return agentsprobe.Target{Username: "other-user", UID: 1002, HomeDir: home}, nil
	}
	if _, err := resolveSeatCLIProcessOwner(1001, 1001, lookup, resolve); !errors.Is(err, errSeatCLIOwner) {
		t.Fatal("resolved another uid")
	}
}

type seatCLIRefusingWriter struct{}

func (seatCLIRefusingWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture-output-refusal")
}

func TestHarnessSeatCLIPropagatesJSONAndTableWriteFailure(t *testing.T) {
	registry, owner := seatCLIState(t)
	if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{false, true} {
		command := newHarnessSeatRoot(func() (seatCLIOwner, error) { return owner, nil })
		command.SetOut(seatCLIRefusingWriter{})
		command.SetErr(&bytes.Buffer{})
		args := []string{"seat", "list", "--registry-dir", registry}
		if jsonOutput {
			args = append(args, "--json")
		}
		command.SetArgs(args)
		if err := command.Execute(); !errors.Is(err, errSeatCLIOutput) {
			t.Fatal("output failure was silently accepted")
		}
	}
}

func TestHarnessSeatCLIRefusesBareGroupBeforeSharedStartup(t *testing.T) {
	root := &cobra.Command{Use: "citadel", PersistentPreRun: func(*cobra.Command, []string) { t.Fatal("bare metadata group entered shared startup") }}
	root.AddCommand(newHarnessSeatRoot(func() (seatCLIOwner, error) { t.Fatal("bare group resolved owner"); return seatCLIOwner{}, nil }))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"harness", "seat"})
	if err := root.Execute(); !errors.Is(err, errSeatCLICommand) {
		t.Fatal("bare group did not require a subcommand")
	}
}

func TestHarnessSeatCLIAddThenEnableIsLocalDeclarationOnly(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "copilot", "muse"} {
		t.Run(harness, func(t *testing.T) {
			registry, owner := seatCLIState(t)
			config := filepath.Join(owner.Home, "seats", "quoted ' fixture")
			t.Setenv("GH_TOKEN", "fixture-must-not-appear")
			t.Setenv("OPENAI_API_KEY", "fixture-must-not-appear")
			if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); err != nil {
				t.Fatal(err)
			}
			out, err := seatCLIForTest(t, owner, "seat", "add", "fixture-seat", "--harness", harness, "--label", "Fixture", "--config-dir", config, "--registry-dir", registry)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "env -i ") || !strings.Contains(out, "not executed") || strings.Contains(out, "fixture-must-not-appear") || strings.Contains(out, "GH_TOKEN=") || strings.Contains(out, "OPENAI_API_KEY=") {
				t.Fatal("login instruction inherited credentials or implied execution")
			}
			if !strings.Contains(out, "'\"'\"'") {
				t.Fatal("instruction did not quote apostrophe in path")
			}
			entries, err := os.ReadDir(config)
			if err != nil || len(entries) != 0 {
				t.Fatal("add populated vendor state")
			}
			info, err := os.Stat(config)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatal("configuration directory is not private")
			}
			listed, err := seatCLIForTest(t, owner, "seat", "list", "--json", "--registry-dir", registry)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Seats []seatCLIListRow `json:"seats"`
			}
			if json.Unmarshal([]byte(listed), &result) != nil || len(result.Seats) != 1 {
				t.Fatal("added declaration missing")
			}
			seat := result.Seats[0]
			if seat.Enabled || seat.Auth != harnessseat.AuthUnknown || seat.PlanType != "" || len(seat.Models) != 0 || seat.DataCeiling != "public" {
				t.Fatal("add invented observed metadata or authority")
			}
			if _, err := seatCLIForTest(t, owner, "seat", "enable", "fixture-seat", "--registry-dir", registry); err != nil {
				t.Fatal(err)
			}
			listed, err = seatCLIForTest(t, owner, "seat", "list", "--json", "--registry-dir", registry)
			if err != nil || json.Unmarshal([]byte(listed), &result) != nil || !result.Seats[0].Enabled || result.Seats[0].Auth != harnessseat.AuthUnknown {
				t.Fatal("enable did not remain a local flag")
			}
			if entries, err := os.ReadDir(config); err != nil || len(entries) != 0 {
				t.Fatal("enable populated vendor state")
			}
			if err := os.Chmod(config, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := seatCLIForTest(t, owner, "seat", "enable", "fixture-seat", "--registry-dir", registry); err == nil {
				t.Fatal("unsafe directory enabled")
			}
		})
	}
}

func TestHarnessSeatCLIAddRefusalsDoNotCreateVendorDirectory(t *testing.T) {
	for _, extra := range [][]string{{"--plan-type", "invented"}, {"--os-user", "other-user"}, {"--account-digest", strings.Repeat("a", 64)}, {"--auth", "signed_in"}, {"--models", "invented"}, {"--harness", "unknown"}, {"--label", ""}, {"--root", "relative"}, {"--harness", "muse", "--data-ceiling", "private"}, {"--entitlement", "invalid"}} {
		registry, owner := seatCLIState(t)
		if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(owner.Home, "seats", "refused")
		args := []string{"seat", "add", "fixture-seat", "--harness", "codex", "--label", "Fixture", "--config-dir", config, "--registry-dir", registry}
		if _, err := seatCLIForTest(t, owner, append(args, extra...)...); err == nil {
			t.Fatal("invalid declaration accepted")
		}
		if _, err := os.Stat(config); !os.IsNotExist(err) {
			t.Fatal("refused add created vendor directory")
		}
	}
}

func TestHarnessSeatCLIAddAndEnableRequireTerminalBeforeState(t *testing.T) {
	for _, args := range [][]string{{"seat", "add", "fixture-seat"}, {"seat", "enable", "fixture-seat"}} {
		command := newHarnessSeatRoot(func() (seatCLIOwner, error) {
			t.Fatal("nonterminal mutation reached owner resolution")
			return seatCLIOwner{}, nil
		})
		command.SetIn(strings.NewReader("not a human terminal"))
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		if err := command.Execute(); !errors.Is(err, errSeatCLITerminal) {
			t.Fatal("nonterminal mutation accepted")
		}
	}
}

func TestHarnessSeatCLIAddRefusesExistingDirectoryAndDuplicateDeclaration(t *testing.T) {
	registry, owner := seatCLIState(t)
	if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(owner.Home, "existing")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(config, "untouched.fixture")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"seat", "add", "fixture-seat", "--harness", "codex", "--label", "Fixture", "--registry-dir", registry, "--config-dir", config}
	if _, err := seatCLIForTest(t, owner, args...); !errors.Is(err, harnessseat.ErrSeatDirectoryExists) {
		t.Fatal("existing vendor state adopted")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatal("existing state changed")
	}
	fresh := filepath.Join(owner.Home, "fresh")
	args[len(args)-1] = fresh
	if _, err := seatCLIForTest(t, owner, args...); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(owner.Home, "second")
	args[len(args)-1] = second
	if _, err := seatCLIForTest(t, owner, args...); err == nil {
		t.Fatal("duplicate seat id accepted")
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("duplicate declaration created a directory")
	}
}

func TestHarnessSeatCLIHumanLoginInstructionsRespectDeclaredMethod(t *testing.T) {
	_, owner := seatCLIState(t)
	for _, tc := range []struct {
		harness harnessseat.Harness
		suffix  string
	}{{harnessseat.Codex, "codex login --with-api-key"}, {harnessseat.Copilot, "copilot login --with-token"}, {harnessseat.Claude, "; no value is accepted or read by Citadel."}} {
		i := harnessseat.Identity{ID: "fixture-seat", Harness: tc.harness, Owner: owner.Name, HomeDir: owner.Home, ConfigDir: filepath.Join(owner.Home, "private")}
		got, err := seatHumanLoginInstruction(i, "api_key", "/usr/bin:/bin")
		if err != nil || !strings.HasSuffix(got, tc.suffix) {
			t.Fatal("wrong vendor human login method")
		}
		if tc.harness == harnessseat.Claude && (!strings.Contains(got, "apiKeyHelper") || strings.Contains(got, "claude auth login")) {
			t.Fatal("API-key helper replaced by subscription login")
		}
	}
}

func TestHarnessSeatCLIAddOutputFailurePreservesPublishedDeclaration(t *testing.T) {
	registry, owner := seatCLIState(t)
	if _, err := seatCLIForTest(t, owner, "seat", "init", "--registry-dir", registry); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(owner.Home, "fresh")
	command := newHarnessSeatRootWithTerminal(func() (seatCLIOwner, error) { return owner, nil }, func(*cobra.Command) bool { return true })
	command.SetOut(seatCLIRefusingWriter{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"seat", "add", "fixture-seat", "--harness", "codex", "--label", "Fixture", "--config-dir", config, "--registry-dir", registry})
	if err := command.Execute(); !errors.Is(err, errSeatCLIOutput) {
		t.Fatal("add output error swallowed")
	}
	out, err := seatCLIForTest(t, owner, "seat", "list", "--json", "--registry-dir", registry)
	if err != nil || !strings.Contains(out, "fixture-seat") || !strings.Contains(out, `"enabled":false`) {
		t.Fatal("output failure erased saved disabled declaration")
	}
	if entries, err := os.ReadDir(config); err != nil || len(entries) != 0 {
		t.Fatal("output failure changed fresh vendor directory")
	}
}
