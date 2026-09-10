//go:build !windows

package agentsprobe

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBuildVersionCmd_DropSetsCredential is the privesc pin the task requires:
// SysProcAttr.Credential must be the TARGET uid/gid when a drop is requested, the
// env must be minimal (identity + PATH, not the worker's), and the cwd must be
// the target home. Setpgid + Cancel are always set (grandchild reaping).
func TestBuildVersionCmd_DropSetsCredential(t *testing.T) {
	t.Setenv("AGENTSPROBE_SENTINEL", "must-not-leak")

	opts := Options{
		HomeDir: "/home/alice",
		PathEnv: "/home/alice/.npm-global/bin:/usr/bin",
		DropTo:  &Credential{UID: 1234, GID: 5678, Username: "alice"},
	}
	cmd := buildVersionCmd(context.Background(), "/home/alice/.npm-global/bin/claude", opts)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil; expected privilege drop")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid should be true for grandchild reaping")
	}
	cred := cmd.SysProcAttr.Credential
	if cred == nil {
		t.Fatal("Credential is nil; the drop was not applied")
	}
	if cred.Uid != 1234 || cred.Gid != 5678 {
		t.Errorf("Credential = {Uid:%d Gid:%d}, want {1234 5678}", cred.Uid, cred.Gid)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel should be set (process-group kill on timeout)")
	}
	if cmd.Dir != "/home/alice" {
		t.Errorf("cmd.Dir = %q, want /home/alice (target home cwd)", cmd.Dir)
	}
	// Minimal env: the target identity + PATH, NOT the worker's environment.
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"HOME=/home/alice", "PATH=/home/alice/.npm-global/bin:/usr/bin", "USER=alice", "LOGNAME=alice"} {
		if !strings.Contains(env, want) {
			t.Errorf("child env missing %q; got %v", want, cmd.Env)
		}
	}
	if strings.Contains(env, "AGENTSPROBE_SENTINEL") {
		t.Errorf("child env leaked the worker's environment: %v", cmd.Env)
	}
}

// TestBuildVersionCmd_NoDropWhenNil: without DropTo, no Credential and no forced
// env (inherits the process env, S1 behavior), but Setpgid still applies.
func TestBuildVersionCmd_NoDropWhenNil(t *testing.T) {
	cmd := buildVersionCmd(context.Background(), "/usr/bin/claude", Options{})
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid should be set even without a drop")
	}
	if cmd.SysProcAttr.Credential != nil {
		t.Errorf("Credential should be nil without DropTo; got %+v", cmd.SysProcAttr.Credential)
	}
	if cmd.Env != nil {
		t.Errorf("child env should be inherited (nil) without a drop; got %v", cmd.Env)
	}
	if cmd.Dir != "" {
		t.Errorf("cmd.Dir should be empty without a drop; got %q", cmd.Dir)
	}
}

// TestProbeVersion_DropStartFailureSurfacesError is the honesty pin for a FAILED
// privilege drop: a non-root worker cannot setgroups/setuid, so requesting a drop
// makes the exec fail to START. That must surface as a versionError, never a
// silent empty version that looks identical to a timed-out vendor.
func TestProbeVersion_DropStartFailureSurfacesError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can perform the credential drop; this test needs the drop to FAIL")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'claude 1.0.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		HomeDir: dir,
		PathEnv: dir,
		DropTo:  &Credential{UID: 12345, GID: 12345, Username: "nobody"},
	}
	version, versionErr := probeVersion(context.Background(), bin, opts)
	if version != "" {
		t.Fatalf("expected empty version when the drop fails, got %q", version)
	}
	if versionErr == "" {
		t.Fatal("a failed privilege drop must surface a versionError, not a silent empty version")
	}
	if !strings.Contains(versionErr, "privilege drop failed") {
		t.Fatalf("versionError = %q, want it to name the privilege drop failure", versionErr)
	}

	// Without a drop, the same binary probes cleanly with no error surfaced.
	v, ve := probeVersion(context.Background(), bin, Options{HomeDir: dir, PathEnv: dir})
	if v == "" || ve != "" {
		t.Fatalf("no-drop probe: got version=%q versionErr=%q, want a version and no error", v, ve)
	}
}

// TestProbeVersion_KillsProcessGroupOnTimeout proves the Setpgid + Cancel wiring
// reaps a daemonizing GRANDCHILD (an update-check helper), not just the direct
// child, when the probe times out. No privilege drop needed for this property.
func TestProbeVersion_KillsProcessGroupOnTimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := filepath.Join(dir, "claude")
	// A `--version` that backgrounds a long sleep (the grandchild) in the same
	// process group, records its pid, then hangs so the probe must time out.
	body := "#!/bin/sh\nsleep 60 &\necho \"$!\" > " + pidFile + "\nsleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// Generous ctx so the script reliably records the grandchild pid before the
	// timeout kill fires, even on a loaded CI runner (the pid is $! of the
	// backgrounded sleep, so it can only be written AFTER the spawn).
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	// Returns "" on timeout; the point is the side effect (group killed).
	_, _ = probeVersion(ctx, script, Options{})

	pidBytes := waitForFile(t, pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		t.Fatalf("could not read grandchild pid from %q: %v", pidBytes, err)
	}

	// The grandchild must be dead: kill(pid, 0) -> ESRCH once reaped.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return // reaped, as required
		}
		if time.Now().After(deadline) {
			// best effort cleanup so a leaked sleep does not linger
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild pid %d still alive after timeout kill; process group not reaped (err=%v)", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestJSONFileNonEmptyObjectState_FifoIsUnknownNotBlock is the DoS pin: a
// target-controlled FIFO at the credential path must resolve to Unknown, never
// block a root ReadFile forever (which would wedge the singleflight probe).
func TestJSONFileNonEmptyObjectState_FifoIsUnknownNotBlock(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "creds.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	done := make(chan AuthState, 1)
	go func() { done <- jsonFileNonEmptyObjectState(fifo) }()
	select {
	case got := <-done:
		if got != AuthStateUnknown {
			t.Fatalf("FIFO credential path: got %q, want %q", got, AuthStateUnknown)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("jsonFileNonEmptyObjectState BLOCKED on a FIFO (DoS); expected an immediate Unknown")
	}
}

// TestJSONFileNonEmptyObjectState_SymlinkIsUnknown: a symlink at the credential
// path is never followed as root; it degrades to Unknown, not a value read.
func TestJSONFileNonEmptyObjectState_SymlinkIsUnknown(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "creds.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got := jsonFileNonEmptyObjectState(link); got != AuthStateUnknown {
		t.Fatalf("symlink credential path: got %q, want %q", got, AuthStateUnknown)
	}
}

// waitForFile polls until path is non-empty (the script writes it before hanging).
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %q never populated", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
