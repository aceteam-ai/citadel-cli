//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"reflect"
	"strings"
	"testing"
)

type rootManagerFixture struct {
	system, user map[int]string
	unavailable  map[int]bool
	calls        []string
}

func (f *rootManagerFixture) capture(t *testing.T) func(context.Context, ...string) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, args ...string) ([]byte, error) {
		uid, ok := ctx.Value(workerManagerUIDKey{}).(int)
		if !ok {
			t.Fatal("manager UID not pinned")
		}
		user := len(args) > 0 && args[0] == "--user"
		key := uid
		if !user {
			key = -1
		}
		f.calls = append(f.calls, fmt.Sprintf("%d %s", key, strings.Join(args, " ")))
		op := args[0]
		if user {
			op = args[1]
		}
		switch op {
		case "list-units":
			if f.unavailable[key] {
				return nil, errors.New("fixture manager unavailable")
			}
			if user && f.user[uid] != "" || !user && f.system[uid] != "" {
				return []byte("worker.service loaded active running Worker\n"), nil
			}
			return nil, nil
		case "cat":
			if user {
				return []byte(f.user[uid]), nil
			}
			return []byte(f.system[uid]), nil
		case "show":
			return []byte("no\n"), nil
		}
		t.Fatal("unexpected discovery operation", args)
		return nil, nil
	}
}

const rootWorkerFixture = "[Service]\nExecStart=/fixture/citadel work\n"

func TestRootDiscoveryMissingOwnUserManagerKeepsSystemWorker(t *testing.T) {
	f := &rootManagerFixture{system: map[int]string{0: rootWorkerFixture}, unavailable: map[int]bool{0: true}}
	workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical, 0, "", func(int) (bool, error) { t.Fatal("root-only discovery probed a sudo account"); return false, nil })
	if err != nil || len(workers) != 1 || workers[0].Unit.UserMode || workers[0].User != "root" {
		t.Fatal("missing root user manager hid system worker", workers, err)
	}
}
func TestRootDiscoveryUsesExistingSudoManagerAndPinnedRestart(t *testing.T) {
	useWorkerManagerAccountFixture(t)
	f := &rootManagerFixture{user: map[int]string{9001: rootWorkerFixture}, unavailable: map[int]bool{0: true}}
	workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical, 0, "9001", func(uid int) (bool, error) {
		if uid != 9001 {
			t.Fatal("probed foreign manager", uid)
		}
		return true, nil
	})
	if err != nil || len(workers) != 1 || !workers[0].Unit.UserMode || workers[0].Unit.ManagerUID != "9001" || workers[0].User != "9001" {
		t.Fatal("sudo worker manager not bound", workers, err)
	}
	command, err := managedUnitRestartCommand(context.Background(), workers[0].Unit, 0)
	if err != nil || !reflect.DeepEqual(command.Env, discoveryManagerEnvironment(true, 9001)) {
		t.Fatal("root restart used a different bus", command, err)
	}
	assertSudoManagerCredential(t, command)
	if _, err := managedUnitRestartCommand(context.Background(), workers[0].Unit, 8001); err == nil {
		t.Fatal("nonroot caller inherited sudo manager authority")
	}
}
func TestRootDiscoveryAbsentSudoManagerIsNotIncomplete(t *testing.T) {
	f := &rootManagerFixture{unavailable: map[int]bool{0: true}}
	workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical, 0, "9001", func(int) (bool, error) { return false, nil })
	if err != nil || len(workers) != 0 {
		t.Fatal("root without user managers was incomplete", workers, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "9001 ") {
			t.Fatal("absent manager was queried")
		}
	}
}
func TestRootDiscoveryExistingUninspectableSudoManagerFailsClosed(t *testing.T) {
	for _, probeErr := range []error{nil, errors.New("fixture permission denied")} {
		f := &rootManagerFixture{system: map[int]string{0: rootWorkerFixture}, unavailable: map[int]bool{0: true, 9001: true}}
		workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical, 0, "9001", func(int) (bool, error) { return true, probeErr })
		if err == nil || len(workers) != 0 || !strings.Contains(err.Error(), "incomplete") {
			t.Fatal("sudo manager uncertainty selected a restart target", workers, err)
		}
	}
}
func TestRootDiscoveryDoesNotHideAnotherMatchingSudoWorker(t *testing.T) {
	f := &rootManagerFixture{system: map[int]string{0: rootWorkerFixture}, user: map[int]string{9001: rootWorkerFixture}, unavailable: map[int]bool{0: true}}
	workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical, 0, "9001", func(int) (bool, error) { return true, nil })
	if err != nil || len(workers) != 2 {
		t.Fatal("sudo ambiguity was hidden", workers, err)
	}
}
func TestRootDiscoveryRejectsMalformedSudoUIDBeforeObservation(t *testing.T) {
	for _, uid := range []string{"-1", "../9001", "09", "+9", "nine", "999999999999999999999999"} {
		capture := func(context.Context, ...string) ([]byte, error) {
			t.Fatal("invalid UID queried a manager")
			return nil, nil
		}
		workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", capture, fixtureCanonical, 0, uid, func(int) (bool, error) { t.Fatal("invalid UID probed host path"); return false, nil })
		if err == nil || len(workers) != 0 {
			t.Fatal("invalid sudo UID accepted", uid)
		}
	}
	for _, uid := range []string{"-1", "../9001", "09"} {
		if _, err := managedUnitRestartCommand(context.Background(), ManagedUnit{Name: "worker", UserMode: true, ManagerUID: uid}, 0); err == nil {
			t.Fatal("root restart accepted malformed manager UID")
		}
	}
}
func TestNonRootDiscoveryDoesNotBorrowSudoIdentity(t *testing.T) {
	f := &rootManagerFixture{user: map[int]string{9001: rootWorkerFixture}}
	workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical, 9001, "8001", func(int) (bool, error) { t.Fatal("nonroot discovery probed sudo account"); return false, nil })
	if err != nil || len(workers) != 1 || workers[0].Unit.ManagerUID != "9001" {
		t.Fatal("nonroot identity changed", workers, err)
	}
	f.unavailable = map[int]bool{9001: true}
	if workers, err := discoverActiveWorkersForProcess(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical, 9001, "", nil); err == nil || len(workers) != 0 {
		t.Fatal("missing own manager was reported complete")
	}
}
func TestDiscoveryCaptureUsesPinnedSudoBusWithoutExec(t *testing.T) {
	useWorkerManagerAccountFixture(t)
	old := runWorkerDiscoveryCommand
	defer func() { runWorkerDiscoveryCommand = old }()
	runWorkerDiscoveryCommand = func(command *exec.Cmd) error {
		if !reflect.DeepEqual(command.Env, discoveryManagerEnvironment(true, 9001)) || len(command.Args) < 2 || command.Args[1] != "--user" {
			t.Fatal("capture ignored manager UID", command.Env, command.Args)
		}
		assertSudoManagerCredential(t, command)
		_, err := command.Stdout.Write([]byte("fixture"))
		return err
	}
	for _, operation := range []string{"list-units", "cat", "show"} {
		body, err := captureSystemctl(context.WithValue(context.Background(), workerManagerUIDKey{}, 9001), "--user", operation)
		if err != nil || string(body) != "fixture" {
			t.Fatal("capture fixture failed", string(body), err)
		}
	}
}

func useWorkerManagerAccountFixture(t *testing.T) {
	t.Helper()
	oldUID, oldLookup := workerManagerEffectiveUID, lookupWorkerManagerUser
	t.Cleanup(func() { workerManagerEffectiveUID, lookupWorkerManagerUser = oldUID, oldLookup })
	workerManagerEffectiveUID = func() int { return 0 }
	lookupWorkerManagerUser = func(uid string) (*user.User, error) {
		if uid != "9001" {
			return nil, errors.New("fixture unknown account")
		}
		return &user.User{Uid: "9001", Gid: "9002", HomeDir: "/fixture/manager-home"}, nil
	}
}

func assertSudoManagerCredential(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if command.SysProcAttr == nil || command.SysProcAttr.Credential == nil {
		t.Fatal("sudo manager command did not drop its OS account")
	}
	credential := command.SysProcAttr.Credential
	if credential.Uid != 9001 || credential.Gid != 9002 || len(credential.Groups) != 0 || credential.NoSetGroups || command.Dir != "/fixture/manager-home" {
		t.Fatal("sudo manager command retained the wrong identity or directory")
	}
}

func TestWorkerManagerIdentityPreservesOwnAndSystemScopes(t *testing.T) {
	useWorkerManagerAccountFixture(t)
	lookupWorkerManagerUser = func(string) (*user.User, error) {
		t.Fatal("own/system scope looked up another account")
		return nil, nil
	}
	for _, scope := range []struct {
		user               bool
		manager, effective int
	}{{true, 0, 0}, {true, 9001, 9001}, {false, 0, 0}} {
		command := exec.CommandContext(context.Background(), "systemctl", "list-units")
		if err := applyWorkerManagerIdentity(command, scope.user, scope.manager, scope.effective); err != nil || command.SysProcAttr != nil || command.Dir != "" {
			t.Fatal("own/system manager unexpectedly changed identity", err)
		}
	}
}

func TestWorkerManagerIdentityRefusesUnverifiedAccountBeforeRunner(t *testing.T) {
	useWorkerManagerAccountFixture(t)
	oldRunner := runWorkerDiscoveryCommand
	t.Cleanup(func() { runWorkerDiscoveryCommand = oldRunner })
	runWorkerDiscoveryCommand = func(*exec.Cmd) error { t.Fatal("unverified account reached runner"); return nil }
	for _, account := range []*user.User{nil, {Uid: "8001", Gid: "9002", HomeDir: "/fixture/home"}, {Uid: "9001", Gid: "-1", HomeDir: "/fixture/home"}, {Uid: "9001", Gid: "09002", HomeDir: "/fixture/home"}, {Uid: "9001", Gid: "4294967296", HomeDir: "/fixture/home"}, {Uid: "9001", Gid: "9002", HomeDir: "relative"}} {
		lookupWorkerManagerUser = func(string) (*user.User, error) { return account, nil }
		if _, err := captureSystemctl(context.WithValue(context.Background(), workerManagerUIDKey{}, 9001), "--user", "cat"); err == nil {
			t.Fatal("unverified account accepted")
		}
		if command, err := managedUnitRestartCommand(context.Background(), ManagedUnit{Name: "worker", UserMode: true, ManagerUID: "9001"}, 0); err == nil || command != nil {
			t.Fatal("unverified restart account accepted")
		}
	}
}
func TestWorkerContinuationEndsAtBlankOrFileHeader(t *testing.T) {
	for _, boundary := range []string{"", "# /fixture/worker.service", "# /fixture/worker.service.d/override.conf"} {
		content := "[Service]\nExecStart=/fixture/citadel \\\n" + boundary + "\nwork\n"
		path, _, known := workerUnitIdentity(content)
		if known && path != "" {
			t.Fatal("separate logical lines invented worker authority", boundary, path)
		}
	}
	// Ordinary comments inside a real continuation remain compatible.
	path, _, known := workerUnitIdentity("[Service]\nExecStart=/fixture/citadel \\\n# ordinary comment\nwork\n")
	if !known || path != "/fixture/citadel" {
		t.Fatal("ordinary continuation comment was broken", path, known)
	}
}
