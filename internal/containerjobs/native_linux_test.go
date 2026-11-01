//go:build linux

package containerjobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestContainerNativeMandatoryLimitsAndArgv(t *testing.T) {
	r := baseRequest()
	r.Image = "ghcr.io/aceteam-ai/workspace:v1"
	r.CPUs = 1
	r.MemoryBytes = 2 << 30
	r.PidsLimit = 256
	args, err := createArguments(r, HostPolicy{Runtime: "runsc"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"--name", testContainer}, {"--cpus", "1"}, {"--memory", "2147483648"}, {"--memory-swap", "2147483648"}, {"--pids-limit", "256"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges:true"}, {"--runtime", "runsc"}, {"--network", "none"}, {"--storage-opt", "size=4G"}, {"--mount", "type=volume,src=" + testVolume + ",dst=/workspace"}} {
		found := false
		for i := 0; i < len(args)-1; i++ {
			if args[i] == pair[0] && args[i+1] == pair[1] {
				found = true
			}
		}
		if !found {
			t.Fatal("missing mandatory limit", pair[0])
		}
	}
	plain, err := createArguments(r, HostPolicy{}, false)
	if !errors.Is(err, ErrUnavailable) || plain != nil {
		t.Fatal("unbounded writable layer admitted")
	}
}
func nativeState(running bool) []byte {
	rows := []any{map[string]any{"Id": strings.Repeat("c", 64), "ExecIDs": []string{}, "Config": map[string]any{"Labels": map[string]string{"aceteam.org_id": testOrg, "aceteam.volume_ref": testVolume}}, "State": map[string]bool{"Running": running}, "Mounts": []any{map[string]string{"Name": testVolume, "Type": "volume", "Destination": "/workspace"}}}}
	b, _ := json.Marshal(rows)
	return b
}
func TestContainerNativeExecTimeoutStopsAndVerifiesWithoutDeletingDisk(t *testing.T) {
	for _, needsKill := range []bool{false, true} {
		t.Run(map[bool]string{true: "kill", false: "term"}[needsKill], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			running := true
			var calls [][]string
			n := NewNative(t.TempDir(), nil)
			control := &fakeExecControl{attach: func(context.Context, string) (io.ReadCloser, error) { cancel(); return nil, context.Canceled }}
			n.execControl = control
			n.command = func(c context.Context, bin string, args ...string) (commandResult, error) {
				calls = append(calls, append([]string{bin}, args...))
				if len(args) > 1 && args[0] == "container" && args[1] == "inspect" {
					return commandResult{bytes: nativeState(running)}, nil
				}
				if args[0] == "exec" {
					cancel()
					return commandResult{code: -1}, context.Canceled
				}
				if args[0] == "stop" {
					if !needsKill {
						running = false
					}
					return commandResult{}, nil
				}
				if args[0] == "kill" {
					running = false
					return commandResult{}, nil
				}
				t.Fatal("unexpected native command", args)
				return commandResult{}, ErrUnavailable
			}
			out, err := n.Exec(ctx, testContainer, []string{"python", "a;echo fixture"})
			if err != nil || !out.TimedOut || out.ExitCode != 124 || running {
				t.Fatal(out, err)
			}
			execSeen, stopSeen, killSeen := reflect.DeepEqual(control.argv, []string{"python", "a;echo fixture"}), false, false
			for _, args := range calls {
				if args[1] == "exec" {
					execSeen = reflect.DeepEqual(args, []string{"docker", "exec", testContainer, "python", "a;echo fixture"})
				}
				if args[1] == "stop" {
					stopSeen = reflect.DeepEqual(args, []string{"docker", "stop", "--signal", "TERM", "--time", "10", strings.Repeat("c", 64)})
				}
				if args[1] == "kill" {
					killSeen = true
				}
				if args[1] == "rm" || args[1] == "volume" {
					t.Fatal("exec timeout destroyed data")
				}
			}
			if !execSeen || !stopSeen || killSeen != needsKill {
				t.Fatal("argv or timeout escalation wrong", calls)
			}
		})
	}
}
func TestContainerNativeExecRefusesUnconfirmedStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewNative(t.TempDir(), nil)
	n.execControl = &fakeExecControl{attach: func(context.Context, string) (io.ReadCloser, error) { cancel(); return nil, context.Canceled }}
	n.command = func(c context.Context, bin string, args ...string) (commandResult, error) {
		if args[0] == "container" {
			return commandResult{bytes: nativeState(true)}, nil
		}
		if args[0] == "exec" {
			cancel()
			return commandResult{code: -1}, context.Canceled
		}
		return commandResult{}, nil
	}
	if out, err := n.Exec(ctx, testContainer, []string{"fixture"}); err == nil || out.TimedOut {
		t.Fatal("unconfirmed stop reported files-kept timeout", out, err)
	}
}
func TestContainerNativeQuotaImageIsExactAndNeverOverwritten(t *testing.T) {
	n := nativeProvisionFixture(t)
	r := baseRequest()
	r.QuotaBytes = 16 << 20
	_, _, mount, _ := n.volumePaths(r.VolumeRef)
	calls := 0
	n.command = func(c context.Context, bin string, args ...string) (commandResult, error) {
		calls++
		if len(args) > 1 && args[0] == "volume" && args[1] == "ls" {
			if calls == 1 {
				return commandResult{}, nil
			}
			return commandResult{bytes: []byte(testVolume)}, nil
		}
		if reflect.DeepEqual(args, []string{"volume", "inspect", testVolume}) {
			b, _ := json.Marshal([]any{map[string]any{"Driver": "local", "Options": map[string]string{"type": "none", "o": "bind", "device": mount}, "Labels": map[string]string{"aceteam.org_id": testOrg, "aceteam.quota_bytes": "16777216"}}})
			return commandResult{bytes: b}, nil
		}
		return commandResult{}, nil
	}
	if err := n.CreateVolume(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(n.root, testVolume, "volume.ext4"))
	if err != nil || info.Size() != r.QuotaBytes {
		t.Fatal("quota image size wrong", err)
	}
	before := calls
	if err := n.CreateVolume(context.Background(), r); err == nil || calls > before+1 {
		t.Fatal("existing disk was formatted")
	}
	if _, err := n.OpenVolume(context.Background(), testVolume); err == nil {
		t.Fatal("unmounted host directory accepted as quota volume")
	}
}
func TestContainerBoundedCommandOutputDrainsExcess(t *testing.T) {
	out := &boundedOutput{}
	p := make([]byte, OutputLimit+100)
	n, err := out.Write(p)
	if n != len(p) || err != nil || len(out.bytes) != OutputLimit || !out.truncated {
		t.Fatal("command output unbounded or not drained")
	}
}
func TestContainerNativeHostLockRejectsSymlinkAndCancelsWait(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(root, "linked")
	if os.Symlink(outside, path) != nil {
		t.Fatal("fixture")
	}
	if _, err := NewNative(path, nil).Lock(context.Background()); err == nil {
		t.Fatal("symlink host root accepted")
	}
	n := NewNative(filepath.Join(root, "private"), nil)
	// The host policy refuses /tmp. Exercise lock lifetime separately using an
	// explicitly owned fixture FD, without relaxing production ancestry checks.
	n.openHostRoot = func(path string) (*os.File, error) {
		if path != filepath.Join(root, "private") {
			t.Fatal("fixture escaped owned root")
		}
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		return os.Open(path)
	}
	unlock, err := n.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.Lock(ctx); err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatal("cancelled lock acquisition proceeded")
	}
}

func TestContainerNativeDestroyCleansOnlyOwnedPerContainerNetwork(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{true: "foreign", false: "owned"}[foreign], func(t *testing.T) {
			n := NewNative(t.TempDir(), nil)
			var removed []string
			n.command = func(c context.Context, bin string, args ...string) (commandResult, error) {
				if args[0] == "container" {
					return commandResult{bytes: nativeState(false)}, nil
				}
				if args[0] == "network" && args[1] == "ls" {
					return commandResult{bytes: []byte("aceteam-net-1234abcd")}, nil
				}
				if args[0] == "network" && args[1] == "inspect" {
					org := testOrg
					if foreign {
						org = otherOrg
					}
					b, _ := json.Marshal([]any{map[string]any{"Labels": map[string]string{"aceteam.org_id": org, "aceteam.container_ref": testContainer}}})
					return commandResult{bytes: b}, nil
				}
				if args[0] == "rm" || args[0] == "network" && args[1] == "rm" {
					removed = append(removed, strings.Join(args, " "))
					return commandResult{}, nil
				}
				t.Fatal("unexpected effect")
				return commandResult{}, ErrUnavailable
			}
			err := n.DestroyContainer(context.Background(), testContainer)
			if foreign {
				if !errors.Is(err, ErrOwnership) || len(removed) != 0 {
					t.Fatal("foreign network touched", err)
				}
			} else if err != nil || len(removed) != 2 || removed[0] != "rm "+testContainer || removed[1] != "network rm aceteam-net-1234abcd" {
				t.Fatal("owned network leaked", removed, err)
			}
		})
	}
}

// These controls are synthetic; no unit test dials a real Docker socket.
type fakeExecControl struct {
	argv                        []string
	attach                      func(context.Context, string) (io.ReadCloser, error)
	inspect                     func(context.Context, string) (execState, error)
	creates, attaches, inspects int
}

func (f *fakeExecControl) Create(_ context.Context, ref string, argv []string) (string, error) {
	f.creates++
	f.argv = append([]string{}, argv...)
	return strings.Repeat("e", 64), nil
}
func (f *fakeExecControl) Attach(ctx context.Context, id string) (io.ReadCloser, error) {
	f.attaches++
	if f.attach != nil {
		return f.attach(ctx, id)
	}
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeExecControl) Inspect(ctx context.Context, id string) (execState, error) {
	f.inspects++
	if f.inspect != nil {
		return f.inspect(ctx, id)
	}
	return execState{ContainerID: strings.Repeat("c", 64)}, nil
}

func TestContainerNativeExecUsesActualExitAndRefusesPriorRunningExec(t *testing.T) {
	n := NewNative(t.TempDir(), nil)
	n.command = func(context.Context, string, ...string) (commandResult, error) {
		return commandResult{bytes: nativeState(true)}, nil
	}
	fake := &fakeExecControl{}
	fake.inspect = func(context.Context, string) (execState, error) {
		return execState{ContainerID: strings.Repeat("c", 64), ExitCode: 7}, nil
	}
	n.execControl = fake
	out, err := n.Exec(context.Background(), testContainer, []string{"fixture"})
	if err != nil || out.ExitCode != 7 || out.TimedOut || fake.creates != 1 || fake.attaches != 1 || fake.inspects < 2 {
		t.Fatal("actual remote exit not observed", out, err)
	}
	prior := strings.Repeat("a", 64)
	n.command = func(context.Context, string, ...string) (commandResult, error) {
		var row []map[string]any
		json.Unmarshal(nativeState(true), &row)
		row[0]["ExecIDs"] = []string{prior}
		b, _ := json.Marshal(row)
		return commandResult{bytes: b}, nil
	}
	fake = &fakeExecControl{inspect: func(context.Context, string) (execState, error) {
		return execState{Running: true, ContainerID: strings.Repeat("c", 64)}, nil
	}}
	n.execControl = fake
	if _, err = n.Exec(context.Background(), testContainer, []string{"fixture"}); err == nil || fake.creates != 0 {
		t.Fatal("surviving remote exec inherited new authority")
	}
}
func TestContainerNativeAttachLossWaitsForActualExitOrTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running := true
	n := NewNative(t.TempDir(), nil)
	n.command = func(_ context.Context, _ string, args ...string) (commandResult, error) {
		if args[0] == "stop" {
			running = false
		}
		return commandResult{bytes: nativeState(running)}, nil
	}
	fake := &fakeExecControl{attach: func(context.Context, string) (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }}
	fake.inspect = func(context.Context, string) (execState, error) {
		if fake.inspects > 1 {
			cancel()
			return execState{Running: true, ContainerID: strings.Repeat("c", 64)}, nil
		}
		return execState{ContainerID: strings.Repeat("c", 64)}, nil
	}
	n.execControl = fake
	out, err := n.Exec(ctx, testContainer, []string{"fixture"})
	if err != nil || !out.TimedOut || running || fake.attaches != 1 || fake.creates != 1 {
		t.Fatal("attach loss replayed or abandoned active exec", out, err)
	}
}

func TestContainerNativeLostOutputStillReportsInspectedExit(t *testing.T) {
	n := NewNative(t.TempDir(), nil)
	n.command = func(context.Context, string, ...string) (commandResult, error) {
		return commandResult{bytes: nativeState(true)}, nil
	}
	fake := &fakeExecControl{attach: func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("bad frame")), nil
	}}
	fake.inspect = func(context.Context, string) (execState, error) {
		return execState{ContainerID: strings.Repeat("c", 64), ExitCode: 7}, nil
	}
	n.execControl = fake
	out, err := n.Exec(context.Background(), testContainer, []string{"fixture"})
	if err != nil || out.ExitCode != 7 || !out.Truncated || out.TimedOut || out.Message == "" {
		t.Fatal("verified exit was replaced by client outcome", out, err)
	}
}

func TestContainerNativeUnacknowledgedStartCannotFabricateSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running := true
	n := NewNative(t.TempDir(), nil)
	n.command = func(_ context.Context, _ string, args ...string) (commandResult, error) {
		if args[0] == "stop" {
			running = false
		}
		return commandResult{bytes: nativeState(running)}, nil
	}
	fake := &fakeExecControl{attach: func(context.Context, string) (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }}
	fake.inspect = func(context.Context, string) (execState, error) {
		if fake.inspects >= 3 {
			cancel()
		}
		return execState{ContainerID: strings.Repeat("c", 64)}, nil
	}
	n.execControl = fake
	out, err := n.Exec(ctx, testContainer, []string{"fixture"})
	if err != nil || !out.TimedOut || running || fake.attaches != 1 {
		t.Fatal("unacknowledged start was reported as completion", out, err)
	}
}

func TestContainerNativeCommandPinsLocalEngineAndDropsCredentials(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://other.example.invalid:2375")
	t.Setenv("DOCKER_CONTEXT", "foreign-context")
	t.Setenv("ACETEAM_API_KEY", "synthetic-private-value")
	t.Setenv("SSH_AUTH_SOCK", "/synthetic/private/socket")
	t.Setenv("HTTP_PROXY", "http://ambient.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://ambient.invalid:3128")
	t.Setenv("NO_PROXY", "*")
	cmd := nativeCommand(context.Background(), "docker", "info")
	if len(cmd.Args) < 3 || cmd.Args[1] != "--host=unix:///var/run/docker.sock" {
		t.Fatal("Docker host was caller/ambient selected")
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "DOCKER_HOST=") || strings.HasPrefix(entry, "DOCKER_CONTEXT=") || strings.HasPrefix(entry, "ACETEAM_API_KEY=") || strings.HasPrefix(entry, "SSH_AUTH_SOCK=") || strings.HasPrefix(entry, "HTTP_PROXY=") || strings.HasPrefix(entry, "HTTPS_PROXY=") || strings.HasPrefix(entry, "NO_PROXY=") {
			t.Fatal("unnecessary credential/context inherited")
		}
	}
}

func TestContainerNativeProgramExceptionHasClosedTrustedPaths(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"docker", "mkfs.ext4", "mount", "umount", "chown"} {
		command := nativeCommand(context.Background(), name, "fixture")
		if command == nil || command.Path != nativeProgramPath(name) || !filepath.IsAbs(command.Path) {
			t.Fatal("privileged binary followed ambient PATH")
		}
	}
	for _, name := range []string{"sh", "/bin/sh", "env", "unknown"} {
		if nativeCommand(context.Background(), name, "fixture") != nil {
			t.Fatal("unregistered native program admitted")
		}
		if _, err := runCommand(context.Background(), name, "fixture"); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unknown program reached execution")
		}
	}
}
