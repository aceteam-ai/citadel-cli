//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type discoveryFixture struct {
	list      map[bool]string
	units     map[string]string
	reload    string
	listError map[bool]bool
	catError  bool
}

func (f discoveryFixture) capture(t *testing.T) func(context.Context, ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, args ...string) ([]byte, error) {
		userMode := len(args) > 0 && args[0] == "--user"
		if userMode {
			args = args[1:]
		}
		if len(args) == 0 {
			t.Fatal("empty discovery command")
		}
		switch args[0] {
		case "list-units":
			if f.listError[userMode] {
				return nil, errors.New("fixture manager unavailable")
			}
			return []byte(f.list[userMode]), nil
		case "cat":
			if f.catError {
				return nil, errors.New("fixture unit unavailable")
			}
			return []byte(f.units[args[len(args)-1]]), nil
		case "show":
			return []byte(f.reload), nil
		default:
			t.Fatalf("discovery attempted unexpected command %q", args[0])
			return nil, nil
		}
	}
}

func fixtureCanonical(path string) (string, error) { return filepath.Clean(path), nil }

func TestDiscoverActiveWorkersUsesExecutableInsteadOfUnitName(t *testing.T) {
	fixture := discoveryFixture{
		list: map[bool]string{false: "citadel-work.service loaded active running Custom worker\nforeign.service loaded active running Foreign\n", true: "another.service loaded active running Another\n"},
		units: map[string]string{
			"citadel-work.service": "[Unit]\nDescription=GPU worker\n[Service]\nUser=fixture-worker\nExecStart=/fixture/bin/citadel work\nEnvironment=CITADEL_NO_AUTO_UPDATE=1\nEnvironment=PRIVATE_FIXTURE_VALUE=synthetic-only\n",
			"foreign.service":      "[Service]\nExecStart=/foreign/bin/citadel work\n",
			"another.service":      "[Service]\nExecStart=/fixture/bin/citadel status\n",
		}, reload: "no\n",
	}
	workers, err := discoverActiveWorkers(context.Background(), "/fixture/bin/citadel", fixture.capture(t), fixtureCanonical)
	want := []ManagedWorker{{Unit: ManagedUnit{Name: "citadel-work"}, Executable: "/fixture/bin/citadel", User: "fixture-worker"}}
	if err != nil || !reflect.DeepEqual(workers, want) {
		t.Fatalf("custom worker not discovered or foreign binary selected: %+v %v", workers, err)
	}
	if strings.Contains(fmt.Sprint(workers), "PRIVATE_FIXTURE_VALUE") {
		t.Fatal("discovery exported unit environment")
	}
}

func TestWorkerUnitIdentityHonorsSectionsQuotesAndDropInResets(t *testing.T) {
	for _, tc := range []struct {
		name, content, path string
		known               bool
	}{
		{"literal", "[Service]\nExecStart=/fixture/citadel work\n", "/fixture/citadel", true},
		{"quoted path", "[Service]\nExecStart=\"/fixture/space dir/citadel\" work\n", "/fixture/space dir/citadel", true},
		{"ignore-failure prefix", "[Service]\nExecStart=-/fixture/citadel work\n", "/fixture/citadel", true},
		{"root opt-out", "[Service]\nExecStart=/fixture/citadel --no-auto-update work\n", "/fixture/citadel", true},
		{"drop-in reset", "[Service]\nExecStart=/foreign/citadel work\n# drop-in\n[Service]\nExecStart=\nExecStart=/fixture/citadel work\n", "/fixture/citadel", true},
		{"multiple commands", "[Service]\nExecStart=/fixture/citadel work\nExecStart=/foreign/citadel work\n", "", false},
		{"wrong section", "[Unit]\nExecStart=/fixture/citadel work\n", "", false},
		{"shell wrapper", "[Service]\nExecStart=/bin/sh -c '/fixture/citadel work'\n", "", false},
		{"different subcommand", "[Service]\nExecStart=/fixture/citadel worker\n", "", false},
		{"specifier", "[Service]\nExecStart=%h/citadel work\n", "", false},
		{"environment expansion", "[Service]\nExecStart=$EXECUTABLE work\n", "", false},
		{"unknown escape", "[Service]\nExecStart=/fixture/space\\sdir/citadel work\n", "", false},
		{"continuation", "[Service]\nExecStart=/fixture/citadel \\\n work\n", "", false},
		{"unclosed quote", "[Service]\nExecStart=\"/fixture/citadel work\n", "", false},
		{"dynamic identity", "[Service]\nDynamicUser=yes\nExecStart=/fixture/citadel work\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _, known := workerUnitIdentity(tc.content)
			if path != tc.path || known != tc.known {
				t.Fatalf("identity=%q/%t, want %q/%t", path, known, tc.path, tc.known)
			}
		})
	}
}

func TestDiscoverActiveWorkersResolvesAliasAndPreservesManager(t *testing.T) {
	f := discoveryFixture{list: map[bool]string{true: "custom.service loaded active running Worker\n"}, units: map[string]string{"custom.service": "[Service]\nExecStart=/fixture/symlink work\n"}, reload: "no"}
	canonical := func(path string) (string, error) {
		if path == "/fixture/symlink" {
			return "/fixture/citadel", nil
		}
		return filepath.Clean(path), nil
	}
	workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", f.capture(t), canonical)
	if err != nil || len(workers) != 1 || !workers[0].Unit.UserMode || workers[0].Unit.Name != "custom" {
		t.Fatalf("user-manager alias not resolved: %+v %v", workers, err)
	}
}

func TestDiscoverActiveWorkersRefusesStaleOrUnknownLoadedConfiguration(t *testing.T) {
	for _, reload := range []string{"yes", "", "unknown"} {
		f := discoveryFixture{list: map[bool]string{false: "custom.service loaded active running Worker\n"}, units: map[string]string{"custom.service": "[Service]\nExecStart=/fixture/citadel work\n"}, reload: reload}
		if workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical); err == nil || len(workers) != 0 {
			t.Fatalf("stale manager admitted restart target: %+v %v", workers, err)
		}
	}
}

func TestDiscoverActiveWorkersIgnoresInactiveUnitsAndUnsafeNames(t *testing.T) {
	f := discoveryFixture{list: map[bool]string{false: "inactive.service loaded inactive dead Stopped\n../other.service loaded active running Invalid\n-bad.service loaded active running Invalid\n"}, units: map[string]string{"inactive.service": "[Service]\nExecStart=/fixture/citadel work\n"}, reload: "no"}
	workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical)
	if err != nil || len(workers) != 0 {
		t.Fatalf("inactive/unsafe unit was selected: %+v %v", workers, err)
	}
}

func TestDiscoverActiveWorkersBoundsAndUnknownManagers(t *testing.T) {
	var many strings.Builder
	for i := 0; i <= maxWorkerDiscoveryUnits; i++ {
		fmt.Fprintf(&many, "worker%d.service loaded active running Worker\n", i)
	}
	for _, f := range []discoveryFixture{
		{list: map[bool]string{false: strings.Repeat("x", maxWorkerDiscoveryOutput+1)}},
		{list: map[bool]string{false: many.String()}},
		{listError: map[bool]bool{false: true, true: true}},
		{list: map[bool]string{false: "worker.service loaded active running Worker\n"}, catError: true},
		{list: map[bool]string{false: "worker.service loaded active running Worker\n"}, units: map[string]string{"worker.service": strings.Repeat("x", maxWorkerDiscoveryOutput+1)}},
	} {
		if workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical); err == nil || len(workers) != 0 {
			t.Fatalf("incomplete/bounded discovery selected a target: %+v %v", workers, err)
		}
	}
	var output discoveryOutput
	if _, err := output.Write(make([]byte, maxWorkerDiscoveryOutput+1)); !errors.Is(err, errWorkerDiscoveryBound) {
		t.Fatal("command output writer did not enforce its bound")
	}
}

func TestDiscoverActiveWorkersCancellationBeforeAnyObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	capture := func(context.Context, ...string) ([]byte, error) {
		t.Fatal("cancelled discovery performed IO")
		return nil, nil
	}
	if _, err := discoverActiveWorkers(ctx, "/fixture/citadel", capture, fixtureCanonical); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation not refused: %v", err)
	}
}
