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
	"reflect"
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

// TestHostMediaStart_MarksProfileOwnedBeforeSinkSweep is the regression test
// for the citadel-cli#925 review's blocking finding: hostMedia.Start() must
// mark the profile owned (MarkMeetingProfileOwned) BEFORE running the sink
// sweep (ReapOrphanedMeetingSinks), so a concurrent sweep landing in that
// exact window sees the profile as owned rather than unloading the
// live/loading sink.
//
// This is verified fully hermetically, in Go, via the injectable package
// vars hostMedia.Start() calls through (acquireMeetingProfileSetupLockFn,
// markMeetingProfileOwnedFn, reapOrphanedMeetingSinksFn,
// clearMeetingProfilePlaceholderFn) -- deliberately NOT via a real pactl
// subprocess reading a pidfile the parent Go process just wrote: that
// approach was tried first and found genuinely flaky in this sandboxed
// environment (a freshly-written file was sometimes not yet visible to a
// spawned child process, even though every same-process Go read of it
// succeeded immediately -- verified directly, not merely suspected). An
// injected fake Go func cannot be affected by any cross-process filesystem-
// visibility timing, so this test cannot flake for reasons unrelated to the
// code under test. PATH is additionally isolated to an empty temp dir so
// the real (unmocked) NullSinkRecorder.LoadSink() call fails immediately
// and safely (no real pactl/ffmpeg on PATH -> audioStackAvailable() is
// false), well before ever reaching MeetingBrowser.Start() -- no real
// process is spawned by this test at all.
//
// This test FAILS against the pre-fix hostMedia.Start() (sink sweep called
// with no prior MarkMeetingProfileOwned): "sweep" appears in the recorded
// call order before "mark", or "mark" is absent entirely.
func TestHostMediaStart_MarksProfileOwnedBeforeSinkSweep(t *testing.T) {
	var calls []string

	prevAcquire := acquireMeetingProfileSetupLockFn
	acquireMeetingProfileSetupLockFn = func(profileDirOverride string) (string, func(), error) {
		calls = append(calls, "acquire")
		return "fake-profile-dir", func() { calls = append(calls, "release") }, nil
	}
	t.Cleanup(func() { acquireMeetingProfileSetupLockFn = prevAcquire })

	prevMark := markMeetingProfileOwnedFn
	markMeetingProfileOwnedFn = func(profileDirOverride string) (string, bool, error) {
		calls = append(calls, "mark")
		return profileDirOverride, true, nil
	}
	t.Cleanup(func() { markMeetingProfileOwnedFn = prevMark })

	prevClear := clearMeetingProfilePlaceholderFn
	clearMeetingProfilePlaceholderFn = func(profileDir string) {
		calls = append(calls, "clear")
	}
	t.Cleanup(func() { clearMeetingProfilePlaceholderFn = prevClear })

	prevSweep := reapOrphanedMeetingSinksFn
	reapOrphanedMeetingSinksFn = func(profileDirOverride string) {
		calls = append(calls, "sweep")
	}
	t.Cleanup(func() { reapOrphanedMeetingSinksFn = prevSweep })

	// Isolate PATH to an empty dir so the real (unmocked) rec.LoadSink()
	// call fails immediately and safely -- no real pactl/ffmpeg is ever
	// resolved or spawned, regardless of what this host has installed.
	t.Setenv("PATH", t.TempDir())

	wavPath := filepath.Join(t.TempDir(), "out.wav")
	m := newHostMedia("owned-before-sweep-meeting", "unused-profile-dir", wavPath)

	// Expected to fail: audioStackAvailable() is false with no pactl/ffmpeg
	// on the isolated PATH, so rec.LoadSink() errors before anything else
	// (MeetingBrowser.Start, Chrome, Xvfb) is ever touched.
	if _, err := m.Start(); err == nil {
		t.Fatal("Start() unexpectedly succeeded with no pactl/ffmpeg on PATH")
	} else if !strings.Contains(err.Error(), "load meeting audio sink") {
		t.Fatalf("Start() failed for an unexpected reason (want a LoadSink error): %v", err)
	}

	markIdx, sweepIdx := -1, -1
	for i, c := range calls {
		switch c {
		case "mark":
			if markIdx == -1 {
				markIdx = i
			}
		case "sweep":
			if sweepIdx == -1 {
				sweepIdx = i
			}
		}
	}
	if markIdx == -1 {
		t.Fatalf("hostMedia.Start() never called MarkMeetingProfileOwned; call order: %v", calls)
	}
	if sweepIdx == -1 {
		t.Fatalf("hostMedia.Start() never called ReapOrphanedMeetingSinks; call order: %v", calls)
	}
	if markIdx >= sweepIdx {
		t.Errorf("MarkMeetingProfileOwned (index %d) must run BEFORE ReapOrphanedMeetingSinks "+
			"(index %d) -- reversing this order reopens the citadel-cli#925 race; full call order: %v",
			markIdx, sweepIdx, calls)
	}
	wantOrder := []string{"acquire", "mark", "sweep", "clear", "release"}
	if !reflect.DeepEqual(calls, wantOrder) {
		t.Errorf("full call order = %v, want %v", calls, wantOrder)
	}
}

// TestHostMediaStart_HandsOffProfileLockRatherThanReleasingBeforeBrowserStart
// is the citadel-cli#927 regression test for the setup-lock hand-off fix
// itself. Unlike TestHostMediaStart_MarksProfileOwnedBeforeSinkSweep above
// (whose isolated-to-an-empty-dir PATH makes the real rec.LoadSink() fail
// BEFORE the discriminating point below is ever reached — that test would
// stay green even if the hand-off fix were fully reverted), this test
// combines the same seam-injection technique with the fake-pactl-on-PATH
// setup from TestHostMediaStart_SweepsSinksBeforeLoadSink so the REAL
// (unmocked) rec.LoadSink() succeeds and execution reaches the REAL
// (unmocked) platform.MeetingBrowser.Start(), which then fails at
// findChromium (no fake chrome on the isolated PATH) — the same hermetic
// failure point that test uses, so nothing real is ever launched here either.
//
// Pre-#927, hostMedia.Start() released the setup lock SYNCHRONOUSLY,
// immediately before calling MeetingBrowser.Start() — so "release" would be
// recorded in the call order BEFORE "clear" (a deferred call that only fires
// once the function returns). Post-#927, release only happens via a deferred
// guard that fires on return, registered BEFORE the "clear" defer — so LIFO
// unwind runs "clear" first and "release" second. The exact-order assertion
// below is what actually distinguishes the two: a revert of the
// WithHeldProfileLock hand-off (restoring the old synchronous
// release-before-Start) would flip "clear" and "release" here while leaving
// every platform-level test (which exercises the extracted claimProfileLock
// seam or a hand-constructed MeetingBrowser, never a real hostMedia.Start()
// call) unaffected.
func TestHostMediaStart_HandsOffProfileLockRatherThanReleasingBeforeBrowserStart(t *testing.T) {
	var calls []string

	profileDir := t.TempDir()

	prevAcquire := acquireMeetingProfileSetupLockFn
	acquireMeetingProfileSetupLockFn = func(profileDirOverride string) (string, func(), error) {
		calls = append(calls, "acquire")
		return profileDir, func() { calls = append(calls, "release") }, nil
	}
	t.Cleanup(func() { acquireMeetingProfileSetupLockFn = prevAcquire })

	prevMark := markMeetingProfileOwnedFn
	markMeetingProfileOwnedFn = func(profileDirOverride string) (string, bool, error) {
		calls = append(calls, "mark")
		return profileDirOverride, true, nil
	}
	t.Cleanup(func() { markMeetingProfileOwnedFn = prevMark })

	prevClear := clearMeetingProfilePlaceholderFn
	clearMeetingProfilePlaceholderFn = func(profileDir string) {
		calls = append(calls, "clear")
	}
	t.Cleanup(func() { clearMeetingProfilePlaceholderFn = prevClear })

	prevSweep := reapOrphanedMeetingSinksFn
	reapOrphanedMeetingSinksFn = func(profileDirOverride string) {
		calls = append(calls, "sweep")
	}
	t.Cleanup(func() { reapOrphanedMeetingSinksFn = prevSweep })

	// Fake pactl: same shape as TestHostMediaStart_SweepsSinksBeforeLoadSink
	// -- answers just enough for the REAL rec.LoadSink() to succeed, so
	// execution actually reaches the REAL platform.MeetingBrowser.Start().
	// No log file needed here (unlike that test): ordering is read off
	// `calls`, not a parsed pactl invocation log.
	fakeBin := t.TempDir()
	writeFakeExecutable(t, fakeBin, "pactl", `
case "$1" in
  info) exit 0 ;;
  list) exit 0 ;;
  load-module) echo "99"; exit 0 ;;
  unload-module) exit 0 ;;
  *) exit 0 ;;
esac
`)
	// Fake ffmpeg: never actually executed here either (only by
	// NullSinkRecorder.Start, unreached), but audioStackAvailable() path-
	// checks its presence via exec.LookPath.
	writeFakeExecutable(t, fakeBin, "ffmpeg", "exit 0")
	t.Setenv("PATH", fakeBin)

	wavPath := filepath.Join(t.TempDir(), "out.wav")
	m := newHostMedia("lock-handoff-order-meeting", "unused-profile-dir", wavPath)

	// Expected to fail: findChromium() finds no browser binary on the
	// isolated PATH -- proving execution reached the REAL
	// MeetingBrowser.Start() (and therefore past the real LoadSink) without
	// ever needing a real browser.
	if _, err := m.Start(); err == nil {
		t.Fatal("Start() unexpectedly succeeded with no chrome/chromium on PATH")
	} else if !strings.Contains(strings.ToLower(err.Error()), "chrom") {
		t.Fatalf("Start() failed for an unexpected reason (want a chromium-not-found error): %v", err)
	}

	wantOrder := []string{"acquire", "mark", "sweep", "clear", "release"}
	if !reflect.DeepEqual(calls, wantOrder) {
		t.Errorf("full call order = %v, want %v -- a pre-#927 revert (releasing the setup lock "+
			"synchronously right before MeetingBrowser.Start(), instead of handing it off) would "+
			"instead produce [acquire mark sweep release clear], with release BEFORE the deferred "+
			"clear", calls, wantOrder)
	}
}
