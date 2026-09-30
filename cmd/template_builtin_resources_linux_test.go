//go:build linux

package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func stubRenderIsolation(t *testing.T, uid int) {
	t.Helper()
	originalRun, originalSystemctl, originalEnv := lookupRenderSystemdRun, lookupRenderSystemctl, lookupRenderEnv
	originalUID, originalCheck := renderEffectiveUID, checkRenderDelegation
	originalUnit, originalExec := allocateRenderScopeUnit, runRenderSystemctl
	lookupRenderSystemdRun = func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	lookupRenderSystemctl = func(string) (string, error) { return "/usr/bin/systemctl", nil }
	lookupRenderEnv = func(string) (string, error) { return "/usr/bin/env", nil }
	renderEffectiveUID = func() int { return uid }
	checkRenderDelegation = func(required []string) platform.CgroupDelegationHealth {
		return platform.CgroupDelegationHealth{Applicable: true, OK: true, Controllers: append([]string(nil), required...)}
	}
	allocateRenderScopeUnit = func() (string, error) { return "citadel-render-test.scope", nil }
	runRenderSystemctl = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() {
		lookupRenderSystemdRun, lookupRenderSystemctl, lookupRenderEnv = originalRun, originalSystemctl, originalEnv
		renderEffectiveUID, checkRenderDelegation = originalUID, originalCheck
		allocateRenderScopeUnit, runRenderSystemctl = originalUnit, originalExec
	})
}

func TestRenderLimitedCommandUsesInheritedCgroupCeilings(t *testing.T) {
	stubRenderIsolation(t, 1001)
	childEnv := []string{"HOME=/render", "PATH=/safe/bin"}
	limited, err := newRenderLimitedCommand(context.Background(), childEnv, "/usr/bin/chromium", "--headless")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--unit=citadel-render-test.scope",
		"--property=MemoryMax=2147483648", "--property=MemorySwapMax=0",
		"--property=CPUQuota=200%", "--property=TasksMax=256",
		"--property=RuntimeMaxSec=7200s",
		"--", "/usr/bin/env", "-i", "HOME=/render", "PATH=/safe/bin", "/usr/bin/chromium", "--headless",
	}
	if !reflect.DeepEqual(limited.cmd.Args, want) {
		t.Fatalf("args = %v, want %v", limited.cmd.Args, want)
	}
}

func TestRenderLimitedCommandRootUsesSystemManager(t *testing.T) {
	stubRenderIsolation(t, 0)
	checkRenderDelegation = func([]string) platform.CgroupDelegationHealth {
		t.Fatal("root system-manager launch must not use the rootless delegation path")
		return platform.CgroupDelegationHealth{}
	}
	limited, err := newRenderLimitedCommand(context.Background(), minimalRenderChildEnv(""), "/usr/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if containsString(limited.cmd.Args, "--user") {
		t.Fatalf("root command unexpectedly used user manager: %v", limited.cmd.Args)
	}
}

func TestRenderLimitedCommandFailsClosedWithoutDelegatedControllers(t *testing.T) {
	stubRenderIsolation(t, 1001)
	lookupRenderSystemdRun = func(string) (string, error) {
		t.Fatal("systemd-run lookup occurred before controller preflight")
		return "", nil
	}
	checkRenderDelegation = func(required []string) platform.CgroupDelegationHealth {
		if !reflect.DeepEqual(required, []string{"cpu", "memory", "pids"}) {
			t.Fatalf("required controllers = %v", required)
		}
		return platform.CgroupDelegationHealth{Applicable: true, Missing: []string{"memory", "pids"}, Message: "missing delegated controllers: memory, pids", Hint: "configure delegation"}
	}
	_, err := newRenderLimitedCommand(context.Background(), nil, "/usr/bin/ffmpeg")
	if err == nil || !strings.Contains(err.Error(), "missing delegated controllers: memory, pids") {
		t.Fatalf("error = %v, want fail-closed controller error", err)
	}
}

func TestRenderLimitedCommandFailsClosedWithoutSystemdRun(t *testing.T) {
	stubRenderIsolation(t, 1001)
	lookupRenderSystemdRun = func(string) (string, error) { return "", errors.New("missing") }
	_, err := newRenderLimitedCommand(context.Background(), nil, "/usr/bin/ffmpeg")
	if err == nil || !strings.Contains(err.Error(), "resource isolation unavailable") {
		t.Fatalf("error = %v, want fail-closed isolation error", err)
	}
}

func TestRenderLimitedCommandDoesNotLeakWorkerEnvironment(t *testing.T) {
	stubRenderIsolation(t, 1001)
	t.Setenv("CITADEL_DEVICE_API_TOKEN", "worker-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud-secret")
	limited, err := newRenderLimitedCommand(context.Background(), minimalRenderChildEnv("/render"), "/usr/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	for _, values := range [][]string{limited.cmd.Env, limited.cmd.Args} {
		for _, value := range values {
			if strings.Contains(value, "worker-secret") || strings.Contains(value, "cloud-secret") || strings.HasPrefix(value, "CITADEL_DEVICE_API_TOKEN=") || strings.HasPrefix(value, "AWS_SECRET_ACCESS_KEY=") {
				t.Fatalf("render command leaked worker environment in %q", value)
			}
		}
	}
}

func TestStopRenderSystemdScopeUserAndFailure(t *testing.T) {
	stubRenderIsolation(t, 1001)
	var gotBinary string
	var gotArgs []string
	runRenderSystemctl = func(_ context.Context, binary string, args ...string) ([]byte, error) {
		gotBinary, gotArgs = binary, append([]string(nil), args...)
		return []byte("access denied"), errors.New("exit 1")
	}
	err := stopRenderSystemdScope("/usr/bin/systemctl", "citadel-render-test.scope", true)
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("scope cleanup error = %v", err)
	}
	if gotBinary != "/usr/bin/systemctl" || !reflect.DeepEqual(gotArgs, []string{"--user", "stop", "citadel-render-test.scope"}) {
		t.Fatalf("systemctl call = %q %v", gotBinary, gotArgs)
	}
}

func TestStopRenderSystemdScopeToleratesCollectedUnit(t *testing.T) {
	stubRenderIsolation(t, 1001)
	runRenderSystemctl = func(context.Context, string, ...string) ([]byte, error) {
		return nil, &exec.ExitError{ProcessState: collectedUnitExitState(t)}
	}
	if err := stopRenderSystemdScope("/usr/bin/systemctl", "gone.scope", true); err != nil {
		t.Fatalf("collected scope cleanup = %v", err)
	}
}

func collectedUnitExitState(t *testing.T) *os.ProcessState {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 5")
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("create exit status 5: %v", err)
	}
	return exitErr.ProcessState
}
