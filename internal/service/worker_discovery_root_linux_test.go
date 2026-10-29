//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
	old := runWorkerDiscoveryCommand
	defer func() { runWorkerDiscoveryCommand = old }()
	runWorkerDiscoveryCommand = func(command *exec.Cmd) error {
		if !reflect.DeepEqual(command.Env, discoveryManagerEnvironment(true, 9001)) || len(command.Args) < 2 || command.Args[1] != "--user" {
			t.Fatal("capture ignored manager UID", command.Env, command.Args)
		}
		_, err := command.Stdout.Write([]byte("fixture"))
		return err
	}
	body, err := captureSystemctl(context.WithValue(context.Background(), workerManagerUIDKey{}, 9001), "--user", "list-units")
	if err != nil || string(body) != "fixture" {
		t.Fatal("capture fixture failed", string(body), err)
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
