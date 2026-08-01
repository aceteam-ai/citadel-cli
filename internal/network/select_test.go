// internal/network/select_test.go
package network

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// deadPID finds a pid that names no running process, so tests can assert that
// stale bookkeeping is ignored. Kernels recycle pids, so this probes upward
// rather than assuming a fixed value.
func isWindowsTest() bool { return runtime.GOOS == "windows" }

func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 999000; pid < 999200; pid++ {
		if !processAlive(pid) {
			return pid
		}
	}
	t.Skip("no dead pid available on this machine")
	return 0
}

func TestSelectBackendDefaultsToUserspace(t *testing.T) {
	dir := t.TempDir()
	mode, err := SelectBackend(dir)
	if err != nil {
		t.Fatalf("SelectBackend() error = %v", err)
	}
	if mode != ModeUserspace {
		t.Errorf("mode = %q, want %q", mode, ModeUserspace)
	}
}

// The status quo must survive: several userspace processes against one state
// dir (a background `citadel work` plus an ad-hoc `citadel status`) has always
// worked, and turning it into an error here would break every node for a
// problem this change did not introduce.
func TestSelectBackendDoesNotBlockOnAnotherUserspaceProcess(t *testing.T) {
	dir := t.TempDir()

	// A live holder: our own test process, recorded under a different pid is
	// not possible, so use the parent pid, which is certainly alive.
	if err := writeHolders(userspaceHoldersPath(dir), []int{os.Getppid()}); err != nil {
		t.Fatalf("writeHolders: %v", err)
	}

	mode, err := SelectBackend(dir)
	if err != nil {
		t.Fatalf("SelectBackend() with a live userspace holder must not error, got %v", err)
	}
	if mode != ModeUserspace {
		t.Errorf("mode = %q, want %q", mode, ModeUserspace)
	}
}

// A socket file left by a crashed `citadel up` must not be mistaken for a
// live backend — attaching to a dead socket is worse than starting our own.
func TestSelectBackendIgnoresStaleSocketFile(t *testing.T) {
	if isWindowsTest() {
		t.Skip("unix socket semantics")
	}
	dir := t.TempDir()
	path := LocalAPISocketPath(dir)
	if err := os.WriteFile(path, []byte("not a live socket"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	mode, err := SelectBackend(dir)
	if err != nil {
		t.Fatalf("SelectBackend() error = %v", err)
	}
	if mode != ModeUserspace {
		t.Errorf("mode = %q, want %q (a dead socket is not an attachable backend)", mode, ModeUserspace)
	}
}

func TestRegisterUserspaceHolderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := userspaceHoldersPath(dir)

	release := registerUserspaceHolder(dir)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("holder file not written: %v", err)
	}
	if !strings.Contains(string(data), strconv.Itoa(os.Getpid())) {
		t.Errorf("holder file = %q, want it to contain our pid %d", data, os.Getpid())
	}

	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("holder file survived release (err = %v)", err)
	}

	release() // must be safe twice
}

// A holder file left by a crashed process must not wedge machine-wide mode
// until someone deletes it by hand.
func TestCheckNoUserspaceHoldersIgnoresDeadPIDs(t *testing.T) {
	dir := t.TempDir()
	if err := writeHolders(userspaceHoldersPath(dir), []int{deadPID(t)}); err != nil {
		t.Fatalf("writeHolders: %v", err)
	}
	if err := checkNoUserspaceHolders(dir); err != nil {
		t.Errorf("a dead pid must not block machine-wide mode, got %v", err)
	}
}

func TestCheckNoUserspaceHoldersBlocksOnLivePID(t *testing.T) {
	dir := t.TempDir()
	live := os.Getppid()
	if err := writeHolders(userspaceHoldersPath(dir), []int{live}); err != nil {
		t.Fatalf("writeHolders: %v", err)
	}
	err := checkNoUserspaceHolders(dir)
	if err == nil {
		t.Fatal("a live userspace holder must block machine-wide mode")
	}
	// The message must name the blocking pid — "something else is using it"
	// with no pid is not actionable.
	if !strings.Contains(err.Error(), strconv.Itoa(live)) {
		t.Errorf("error %q does not name the blocking pid %d", err, live)
	}
}

func TestCheckNoUserspaceHoldersPassesWhenAbsent(t *testing.T) {
	if err := checkNoUserspaceHolders(t.TempDir()); err != nil {
		t.Errorf("no holder file must not block, got %v", err)
	}
}

// removeStaleSocket must never delete a regular file: a mistyped or
// misconfigured state dir should fail loudly, not quietly eat data.
func TestRemoveStaleSocketRefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(path, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := removeStaleSocket(path); err == nil {
		t.Error("removeStaleSocket() on a regular file: want error, got nil")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("regular file was removed: %v", err)
	}
}

func TestRemoveStaleSocketNoopWhenMissing(t *testing.T) {
	if err := removeStaleSocket(filepath.Join(t.TempDir(), "absent.sock")); err != nil {
		t.Errorf("missing path must be a no-op, got %v", err)
	}
}

func TestLocalAPISocketPathIsInsideStateDir(t *testing.T) {
	if isWindowsTest() {
		t.Skip("windows uses a named pipe, not a path under the state dir")
	}
	dir := t.TempDir()
	got := LocalAPISocketPath(dir)
	if filepath.Dir(got) != dir {
		t.Errorf("socket path %q is not inside the state dir %q", got, dir)
	}
}

// CleanUpSystemState is the first statement of tunBackend.Up AND the entire
// body of `citadel down`, so a panic in it takes out both the start path and
// the recovery path. It ran with nil subsystems until this test existed:
// netmon.New calls bus.Client() on its argument, and dns.CleanUp threads the
// bus into a Manager, so nils were a guaranteed nil-pointer dereference on the
// first real `citadel up` — invisible to `citadel up --check`, which does not
// call it.
//
// Unprivileged, this changes nothing on the machine: it simply must not crash.
func TestCleanUpSystemStateDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CleanUpSystemState panicked: %v", r)
		}
	}()
	CleanUpSystemState()
}

// PreflightMachineWide must report the elevation requirement rather than
// attempting the device, and must never claim readiness it did not verify.
func TestPreflightWithoutElevation(t *testing.T) {
	res := PreflightMachineWide(false)
	if res.Elevated {
		t.Error("Elevated = true when told otherwise")
	}
	if res.DeviceOK {
		t.Error("DeviceOK = true without ever having tried the device")
	}
	if res.Detail == "" {
		t.Error("Detail is empty; the user needs to be told how to elevate")
	}
}
