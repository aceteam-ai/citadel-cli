// internal/jobs/meeting_orphan_order_test.go
//
// Pins the citadel-cli#488 sink-sweep ORDERING contract at the real call
// site: hostMedia.Start() must call platform.ReapOrphanedMeetingSinks
// BEFORE NullSinkRecorder.LoadSink() creates the current meeting's own
// sink. Getting this backwards would unload the sink the very same Start()
// call just created.
//
// Verified by fully isolating PATH to a directory containing only FAKE
// pactl/ffmpeg scripts (never the real binaries, even though this host may
// have real ones installed) that log each invocation with its arguments.
// The fake pactl answers `info`/`load-module`/`unload-module` successfully
// so hostMedia's real, unmodified code path runs all the way through
// LoadSink; findChromium then fails immediately (no fake chrome on the
// isolated PATH) before any Xvfb or browser process is ever spawned -- so
// this test starts nothing that needs killing and touches no real
// pactl/process.
package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeExecutable creates an executable shell script at dir/name.
func writeFakeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// TestHostMediaStart_SweepsSinksBeforeLoadSink drives hostMedia.Start()
// through its real code (no mocked Go seams) and asserts, from the fake
// pactl's own invocation log, that a `list short modules` call (the sink
// sweep) precedes the `load-module` call (LoadSink creating the current
// meeting's sink).
func TestHostMediaStart_SweepsSinksBeforeLoadSink(t *testing.T) {
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "pactl.log")

	// Fake pactl: logs every invocation, then answers just enough to let
	// hostMedia's real LoadSink path succeed (info -> ok, load-module -> a
	// module id, unload-module -> ok for the cleanup Start() does on the
	// browser-launch failure below). `list short modules` reports an empty
	// module list -- there is nothing to sweep in this test, only ORDER is
	// under test.
	writeFakeExecutable(t, fakeBin, "pactl", fmt.Sprintf(`
echo "$@" >> %q
case "$1" in
  info) exit 0 ;;
  list) exit 0 ;;
  load-module) echo "99"; exit 0 ;;
  unload-module) exit 0 ;;
  *) exit 0 ;;
esac
`, logPath))
	// Fake ffmpeg: never actually executed by hostMedia.Start() (only by
	// NullSinkRecorder.Start, which this test never reaches), but
	// audioStackAvailable() path-checks its presence via exec.LookPath.
	writeFakeExecutable(t, fakeBin, "ffmpeg", "exit 0")

	// Isolate PATH to ONLY the fake dir: exec.LookPath must never resolve to
	// this host's real pactl/ffmpeg/chrome, so nothing real is ever invoked
	// or launched, regardless of what happens to be installed here.
	t.Setenv("PATH", fakeBin)

	profileDir := t.TempDir()
	wavPath := filepath.Join(t.TempDir(), "out.wav")
	m := newHostMedia("order-test-meeting", profileDir, wavPath)

	// Expected to fail: findChromium() finds no browser binary on the
	// isolated PATH. That failure is the point -- it proves execution
	// reached (and passed through) LoadSink without ever needing a real
	// browser, so the pactl log fully captures the ordering under test.
	if _, err := m.Start(); err == nil {
		t.Fatal("Start() unexpectedly succeeded with no chrome/chromium on PATH")
	} else if !strings.Contains(strings.ToLower(err.Error()), "chrom") {
		t.Fatalf("Start() failed for an unexpected reason (want a chromium-not-found error): %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read pactl invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(logBytes), "\n"), "\n")

	listIdx, loadIdx := -1, -1
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "list":
			if listIdx == -1 {
				listIdx = i
			}
		case "load-module":
			if loadIdx == -1 {
				loadIdx = i
			}
		}
	}

	if listIdx == -1 {
		t.Fatalf("sink sweep never called `pactl list ...`; invocation log: %v", lines)
	}
	if loadIdx == -1 {
		t.Fatalf("LoadSink never called `pactl load-module ...`; invocation log: %v", lines)
	}
	if listIdx >= loadIdx {
		t.Errorf("sink sweep (`pactl list`, index %d) must run BEFORE LoadSink's `pactl load-module` "+
			"(index %d) -- reversing this order would unload the current meeting's own just-created "+
			"sink; full invocation log: %v", listIdx, loadIdx, lines)
	}
}
