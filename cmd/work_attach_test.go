package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

func TestDecideAttach(t *testing.T) {
	cases := []struct {
		name         string
		isTTY        bool
		attachFlag   bool
		noAttachFlag bool
		want         bool
	}{
		{"tty shows banner", true, false, false, true},
		{"non-tty refuses (systemd)", false, false, false, false},
		{"attach flag forces banner off tty", false, true, false, true},
		{"no-attach flag forces refusal on tty", true, false, true, false},
		{"no-attach beats attach", true, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideAttach(tc.isTTY, tc.attachFlag, tc.noAttachFlag); got != tc.want {
				t.Fatalf("decideAttach(tty=%v, attach=%v, noAttach=%v) = %v, want %v",
					tc.isTTY, tc.attachFlag, tc.noAttachFlag, got, tc.want)
			}
		})
	}
}

func TestBuildAttachBanner_LockRecordOnly(t *testing.T) {
	// Degraded path: no status probe (st == nil). The banner must still render the
	// holder PID, version, and worker uptime derived from the recorded start time.
	start := time.Date(2026, 7, 15, 9, 12, 44, 0, time.UTC)
	now := start.Add(51*time.Hour + 3*time.Minute)
	holder := worklock.HolderRecord{PID: 41372, StartTime: start, Version: "2.75.0"}

	out := buildAttachBanner(holder, now, nil)

	for _, want := range []string{
		"already running",
		"PID 41372",
		"version 2.75.0",
		"uptime 2d 3h",
		"citadel logs -f",
		"--no-single-instance",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q\n---\n%s", want, out)
		}
	}
	// No status payload -> must not invent a Node line.
	if strings.Contains(out, "Node:") {
		t.Errorf("banner should omit Node line without status probe\n---\n%s", out)
	}
}

func TestBuildAttachBanner_WithStatus(t *testing.T) {
	start := time.Date(2026, 7, 15, 9, 12, 44, 0, time.UTC)
	now := start.Add(90 * time.Minute)
	holder := worklock.HolderRecord{PID: 100, StartTime: start, Version: "2.75.0"}
	st := &attachStatus{Version: "2.75.0", NodeName: "gpu-1084", Health: "ok"}

	out := buildAttachBanner(holder, now, st)

	for _, want := range []string{"Node: gpu-1084 (ok)", "uptime 1h 30m"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildAttachBanner_VersionFallsBackToStatus(t *testing.T) {
	// Legacy lock record with no version -> the banner borrows the version from the
	// live /status payload rather than dropping it.
	holder := worklock.HolderRecord{PID: 7, StartTime: time.Now().Add(-time.Minute)}
	st := &attachStatus{Version: "9.9.9", NodeName: "n1"}

	out := buildAttachBanner(holder, time.Now(), st)
	if !strings.Contains(out, "version 9.9.9") {
		t.Errorf("banner should fall back to status version\n---\n%s", out)
	}
}

func TestHumanizeUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "0m"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{51*time.Hour + 3*time.Minute, "2d 3h"},
		{-time.Hour, "0m"},
	}
	for _, tc := range cases {
		if got := humanizeUptime(tc.d); got != tc.want {
			t.Errorf("humanizeUptime(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestResolveStatusPort(t *testing.T) {
	// Isolate the gateway-facts read to a temp dir via the existing test hook so
	// no real node state or sockets are touched.
	prev := provisionedStateDirOverride
	t.Cleanup(func() { provisionedStateDirOverride = prev })
	dir := t.TempDir()
	provisionedStateDirOverride = dir

	// No facts file -> default port.
	if got := resolveStatusPort(); got != defaultStatusPort {
		t.Fatalf("resolveStatusPort() without facts = %d, want %d", got, defaultStatusPort)
	}

	// Facts file with a recorded status port -> that port.
	writeFactsFile(t, dir, gatewayFacts{Port: 8443, UseTLS: true, StatusPort: 9099})
	if got := resolveStatusPort(); got != 9099 {
		t.Fatalf("resolveStatusPort() with facts = %d, want 9099", got)
	}

	// Facts file recording no status port -> fall back to default.
	writeFactsFile(t, dir, gatewayFacts{Port: 8443, UseTLS: true})
	if got := resolveStatusPort(); got != defaultStatusPort {
		t.Fatalf("resolveStatusPort() with zero status port = %d, want %d", got, defaultStatusPort)
	}
}

func writeFactsFile(t *testing.T, dir string, f gatewayFacts) {
	t.Helper()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, gatewayFactsFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
