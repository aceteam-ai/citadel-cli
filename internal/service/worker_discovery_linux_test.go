//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryOutputBoundsCopyWithoutWriterTo(t *testing.T) {
	// Command stdout is copied with io.Copy. Hiding WriterTo exercises its
	// destination fast path, rather than only calling Write directly.
	reader := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", maxWorkerDiscoveryOutput+1))}
	var output discoveryOutput
	if _, err := io.Copy(&output, reader); !errors.Is(err, errWorkerDiscoveryBound) {
		t.Fatalf("copy escaped output limit: bytes=%d error=%v", output.Len(), err)
	}
	if output.Len() > maxWorkerDiscoveryOutput {
		t.Fatal("output retained bytes beyond its bound")
	}
}

func TestDiscoveryOutputBoundsStdoutPipeCopy(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer writer.Close()
		_, _ = writer.Write([]byte(strings.Repeat("x", maxWorkerDiscoveryOutput+1)))
	}()
	var output discoveryOutput
	_, copyErr := io.Copy(&output, reader)
	_ = reader.Close() // Release a writer if copying refused before its final byte.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture pipe writer did not stop")
	}
	if !errors.Is(copyErr, errWorkerDiscoveryBound) || output.Len() > maxWorkerDiscoveryOutput {
		t.Fatalf("stdout pipe copy escaped output limit: bytes=%d error=%v", output.Len(), copyErr)
	}
}

func TestDiscoverActiveWorkersRejectsCollectorOverflowInEitherManager(t *testing.T) {
	for _, overflowUser := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow_user=%t", overflowUser), func(t *testing.T) {
			f := discoveryFixture{
				list:   map[bool]string{!overflowUser: "worker.service loaded active running Worker\n"},
				units:  map[string]string{"worker.service": "[Service]\nExecStart=/fixture/citadel work\n"},
				reload: "no",
			}
			base := f.capture(t)
			capture := func(ctx context.Context, args ...string) ([]byte, error) {
				userMode, command := len(args) > 0 && args[0] == "--user", args[0]
				if userMode {
					command = args[1]
				}
				if command == "list-units" && userMode == overflowUser {
					return nil, fmt.Errorf("fixture capture: %w", errWorkerDiscoveryBound)
				}
				return base(ctx, args...)
			}
			workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", capture, fixtureCanonical)
			if !errors.Is(err, errWorkerDiscoveryBound) || len(workers) != 0 {
				t.Fatalf("overflow hid a manager and admitted partial discovery: workers=%+v error=%v", workers, err)
			}
		})
	}
}

func TestDiscoveryCapturePreservesOverflowOverCommandFailure(t *testing.T) {
	for _, runErr := range []error{nil, errors.New("fixture command failure"), &exec.ExitError{}} {
		var output discoveryOutput
		if _, err := output.Write(make([]byte, maxWorkerDiscoveryOutput+1)); !errors.Is(err, errWorkerDiscoveryBound) {
			t.Fatal("fixture did not overflow")
		}
		body, err := output.result(runErr)
		if body != nil || !errors.Is(err, errWorkerDiscoveryBound) {
			t.Fatalf("process error hid overflow: body bytes=%d error type=%T", len(body), err)
		}
		if _, err := output.Write([]byte("later")); !errors.Is(err, errWorkerDiscoveryBound) {
			t.Fatal("overflow observation was reset by a later write")
		}
	}
	var output discoveryOutput
	_, _ = output.Write([]byte("fixture"))
	processErr := errors.New("fixture manager unavailable")
	if body, err := output.result(processErr); body != nil || err != processErr {
		t.Fatal("ordinary command failure was reclassified")
	}
	if body, err := output.result(nil); err != nil || string(body) != "fixture" {
		t.Fatal("bounded successful output was dropped")
	}
}

type discoveryFixture struct {
	list      map[bool]string
	units     map[string]string
	reload    string
	reloads   map[string]string
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
			if reload, ok := f.reloads[args[len(args)-1]]; ok {
				return []byte(reload), nil
			}
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
		{"dynamic identity", "[Service]\nDynamicUser=yes\nExecStart=/fixture/citadel work\n", "/fixture/citadel", false},
		{"identity reset", "[Service]\nDynamicUser=yes\nUser=%i\nDynamicUser=no\nUser=fixture-worker\nExecStart=/fixture/citadel work\n", "/fixture/citadel", true},
		{"mapping reset", "[Service]\nRootDirectory=/fixture/root\nRootDirectory=\nBindPaths=/other:/fixture\nBindPaths=\nExecStart=/fixture/citadel work\n", "/fixture/citadel", true},
		{"read-only hardening", "[Service]\nProtectSystem=strict\nProtectHome=read-only\nReadWritePaths=/fixture\nExecStart=/fixture/citadel work\n", "/fixture/citadel", true},
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

func TestDiscoverActiveWorkersRefusesHiddenIdentityCandidates(t *testing.T) {
	for _, setting := range []string{
		"DynamicUser=yes", "User=%i", "RootDirectory=/fixture/root", "RootImage=/fixture/root.raw",
		"BindPaths=/foreign:/fixture", "BindReadOnlyPaths=/foreign:/fixture",
		"TemporaryFileSystem=/fixture", "MountImages=/fixture/image:/fixture",
		"ExtensionImages=/fixture/extension.raw", "ExtensionDirectories=/fixture/extension",
		"PrivateTmp=yes", "PrivateTmp=disconnected", "PrivateDevices=yes", "ProtectHome=yes", "ProtectHome=tmpfs",
	} {
		for _, hiddenFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/hidden_first=%t", setting, hiddenFirst), func(t *testing.T) {
				hidden, ordinary := "z.service", "a.service"
				if hiddenFirst {
					hidden, ordinary = ordinary, hidden
				}
				f := discoveryFixture{
					list: map[bool]string{false: hidden + " loaded active running Hidden\n" + ordinary + " loaded active running Worker\n"},
					units: map[string]string{
						hidden:   "[Service]\n" + setting + "\nExecStart=/fixture/citadel work\n",
						ordinary: "[Service]\nExecStart=/fixture/citadel work\n",
					}, reload: "no",
				}
				if workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical); err == nil || len(workers) != 0 {
					t.Fatalf("unverified identity became a unique restart target: workers=%+v error=%v", workers, err)
				}
			})
		}
	}
}

func TestDiscoverActiveWorkersRefusesMappedForeignLiteralBeforeHostResolution(t *testing.T) {
	f := discoveryFixture{
		list:   map[bool]string{false: "mapped.service loaded active running Worker\n"},
		units:  map[string]string{"mapped.service": "[Service]\nRootDirectory=/fixture/root\nExecStart=/inside/citadel work\n"},
		reload: "no",
	}
	canonical := func(path string) (string, error) {
		if path != "/fixture/citadel" {
			t.Fatal("service namespace path was interpreted in the host namespace")
		}
		return path, nil
	}
	if workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", f.capture(t), canonical); err == nil || len(workers) != 0 {
		t.Fatalf("mapped path was excluded using host resolution: workers=%+v error=%v", workers, err)
	}
}

func TestWorkerUnitIdentityMappingAndAccountResets(t *testing.T) {
	for _, key := range []string{"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories", "PrivateTmp", "PrivateDevices", "ProtectHome"} {
		t.Run(key, func(t *testing.T) {
			path, account, known := workerUnitIdentity("[Service]\n" + key + "=/fixture/unverified\n" + key + "=\nDynamicUser=yes\nDynamicUser=\nUser=%i\nUser=fixture-worker\nExecStart=/fixture/citadel work\n")
			if !known || path != "/fixture/citadel" || account != "fixture-worker" {
				t.Fatalf("effective reset did not clear prior uncertainty: path=%q account=%q known=%t", path, account, known)
			}
		})
	}
}

func TestDiscoverActiveWorkersRefusesStaleNonmatchingBackingFile(t *testing.T) {
	for _, staleFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale_first=%t", staleFirst), func(t *testing.T) {
			stale, ordinary := "z.service", "a.service"
			if staleFirst {
				stale, ordinary = ordinary, stale
			}
			f := discoveryFixture{
				list: map[bool]string{false: stale + " loaded active running Stale\n" + ordinary + " loaded active running Worker\n"},
				units: map[string]string{
					// Loaded ExecStart may still be /fixture/citadel; disk alone
					// cannot exclude this active unit before checking freshness.
					stale:    "[Service]\nExecStart=/foreign/citadel work\n",
					ordinary: "[Service]\nExecStart=/fixture/citadel work\n",
				}, reload: "no", reloads: map[string]string{stale: "yes"},
			}
			if workers, err := discoverActiveWorkers(context.Background(), "/fixture/citadel", f.capture(t), fixtureCanonical); err == nil || len(workers) != 0 {
				t.Fatalf("stale disk nonmatch hid a loaded worker: workers=%+v error=%v", workers, err)
			}
		})
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
