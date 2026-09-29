//go:build linux

package cmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRenderLimitedCommandUsesInheritedCgroupCeilings(t *testing.T) {
	originalLookup, originalUID := lookupRenderSystemdRun, renderEffectiveUID
	lookupRenderSystemdRun = func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	renderEffectiveUID = func() int { return 1001 }
	t.Cleanup(func() {
		lookupRenderSystemdRun, renderEffectiveUID = originalLookup, originalUID
	})

	cmd, err := newRenderLimitedCommand(context.Background(), "/usr/bin/chromium", "--headless")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--property=MemoryMax=2147483648", "--property=MemorySwapMax=0",
		"--property=CPUQuota=200%", "--property=TasksMax=256",
		"--property=RuntimeMaxSec=7200s",
		"--", "/usr/bin/chromium", "--headless",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
}

func TestRenderLimitedCommandRootUsesSystemManager(t *testing.T) {
	originalLookup, originalUID := lookupRenderSystemdRun, renderEffectiveUID
	lookupRenderSystemdRun = func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	renderEffectiveUID = func() int { return 0 }
	t.Cleanup(func() {
		lookupRenderSystemdRun, renderEffectiveUID = originalLookup, originalUID
	})
	cmd, err := newRenderLimitedCommand(context.Background(), "/usr/bin/ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if containsString(cmd.Args, "--user") {
		t.Fatalf("root command unexpectedly used user manager: %v", cmd.Args)
	}
}

func TestRenderLimitedCommandFailsClosedWithoutSystemdRun(t *testing.T) {
	originalLookup := lookupRenderSystemdRun
	lookupRenderSystemdRun = func(string) (string, error) { return "", errors.New("missing") }
	t.Cleanup(func() { lookupRenderSystemdRun = originalLookup })
	_, err := newRenderLimitedCommand(context.Background(), "/usr/bin/ffmpeg")
	if err == nil || !strings.Contains(err.Error(), "resource isolation unavailable") {
		t.Fatalf("error = %v, want fail-closed isolation error", err)
	}
}
