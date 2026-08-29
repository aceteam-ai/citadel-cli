// internal/platform/meeting_orphan_test.go
//
// Hermetic tests for the citadel-cli#488 orphan reaper: pidfile round-trip,
// process-orphan reaping (dead vs. live owner, identity verification before
// kill), and sink-orphan reaping (dead vs. live owner, parsing). All PID
// kills go through the injectable pidKiller seam and all pactl calls go
// through the injectable pactlListModulesFn/pactlUnloadModuleFn seams -- no
// real process is ever signalled and no real pactl is ever invoked.
package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// deadMeetingPID is a PID chosen the same way cobrowse_session_test.go picks
// its "definitely not a live process" owner PID: a large, arbitrary number
// vanishingly unlikely to be a live PID on the test host.
const deadMeetingPID = 424241

func TestMeetingPidfileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, 111, 222, 333)
	o, b, x := readMeetingPidfile(dir)
	if o != 111 || b != 222 || x != 333 {
		t.Errorf("pidfile round-trip mismatch: got (%d,%d,%d) want (111,222,333)", o, b, x)
	}
	// A missing pidfile yields zeros, never a panic.
	o, b, x = readMeetingPidfile(t.TempDir())
	if o != 0 || b != 0 || x != 0 {
		t.Errorf("missing pidfile should read (0,0,0), got (%d,%d,%d)", o, b, x)
	}
}

// TestReapMeetingProcessOrphans_KillsDeadOwnerPIDs verifies the reap step
// kills the recorded browser+Xvfb PIDs when the pidfile's owner is dead, and
// removes the (now-stale) pidfile -- but never the persistent profile
// directory itself.
func TestReapMeetingProcessOrphans_KillsDeadOwnerPIDs(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, deadMeetingPID, 424242, 424243)
	installMeetingMatchingCmdlineReader(t)

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	reapMeetingProcessOrphans(dir)

	wantKilled := map[int]bool{424242: true, 424243: true}
	for _, pid := range killed {
		delete(wantKilled, pid)
	}
	if len(wantKilled) != 0 {
		t.Errorf("reap did not kill all recorded orphan pids; missed %v (killed %v)", wantKilled, killed)
	}

	if _, err := os.Stat(meetingPidfilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("stale pidfile should be removed after reap, stat err=%v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("persistent profile directory must survive reap, stat err=%v", err)
	}
}

// TestReapMeetingProcessOrphans_SkipsLiveOwner verifies the reap step leaves
// a still-live owner's browser/Xvfb completely alone -- e.g. a sibling
// citadel process legitimately mid-meeting on the same profile.
func TestReapMeetingProcessOrphans_SkipsLiveOwner(t *testing.T) {
	dir := t.TempDir()
	// os.Getppid() models a live process distinct from our own PID, mirroring
	// cobrowse_session_test.go's TestCobrowseSession_SweepSkipsLiveOwner.
	writeMeetingPidfile(dir, os.Getppid(), 515151, 515152)
	installMeetingMatchingCmdlineReader(t) // would match/kill if reap got this far

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	reapMeetingProcessOrphans(dir)

	if len(killed) != 0 {
		t.Errorf("reap must not kill a live owner's browser/Xvfb pids, killed %v", killed)
	}
	if _, err := os.Stat(meetingPidfilePath(dir)); err != nil {
		t.Errorf("a live owner's pidfile must be left in place, stat err=%v", err)
	}
}

// TestReapMeetingProcessOrphans_VerifiesIdentityBeforeKill is the security
// analogue of cobrowse's TestCobrowseSession_SweepVerifiesIdentityBeforeKill:
// a recorded PID whose /proc cmdline no longer matches the expected
// browser/Xvfb binary (e.g. recycled by the OS after a reboot) must NOT be
// killed, even though its recorded owner is dead.
func TestReapMeetingProcessOrphans_VerifiesIdentityBeforeKill(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, deadMeetingPID, 626261, 626262)

	prevReader := procCmdlineReader
	procCmdlineReader = func(pid int) (string, error) {
		return "/usr/sbin/sshd -D", nil // matches neither browser nor Xvfb markers
	}
	t.Cleanup(func() { procCmdlineReader = prevReader })

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	reapMeetingProcessOrphans(dir)

	if len(killed) != 0 {
		t.Errorf("reap must not kill a pid whose identity no longer matches, killed %v", killed)
	}
	// The stale pidfile itself is still cleaned up even though nothing was killed.
	if _, err := os.Stat(meetingPidfilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("stale pidfile should still be removed, stat err=%v", err)
	}
}

// installMeetingMatchingCmdlineReader stubs procCmdlineReader to report a
// cmdline matching BOTH the browser and Xvfb identity markers, mirroring
// cobrowse_session_test.go's installMatchingCmdlineReader, so a reap test
// exercising something other than the identity check itself isn't tripped up
// by the fail-closed default.
func installMeetingMatchingCmdlineReader(t *testing.T) {
	t.Helper()
	prev := procCmdlineReader
	procCmdlineReader = func(pid int) (string, error) {
		return "/usr/bin/chromium --headless\x00Xvfb :99", nil
	}
	t.Cleanup(func() { procCmdlineReader = prev })
}

func TestOrphanSinkModuleIDs_ParsesMeetingSinksOnly(t *testing.T) {
	sample := "5\tmodule-udev-detect\t\n" +
		"10\tmodule-null-sink\tsink_name=citadel_meeting_abc123 sink_properties=device.description=citadel_meeting_abc123\n" +
		"11\tmodule-null-sink\tsink_name=some_other_sink sink_properties=device.description=some_other_sink\n" +
		"12\tmodule-null-sink\tsink_name=citadel_meeting_def456 sink_properties=device.description=citadel_meeting_def456\n"

	got := orphanSinkModuleIDs(sample)
	want := []string{"10", "12"}
	if len(got) != len(want) {
		t.Fatalf("orphanSinkModuleIDs = %v, want %v", got, want)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("orphanSinkModuleIDs[%d] = %q, want %q (full: %v)", i, got[i], id, got)
		}
	}
}

// TestReapOrphanedMeetingSinks_UnloadsWhenOwnerDead verifies every currently
// loaded citadel_meeting_* sink is unloaded when the persistent profile's
// pidfile shows no live owner, and that a non-meeting sink is left alone.
func TestReapOrphanedMeetingSinks_UnloadsWhenOwnerDead(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, deadMeetingPID, 0, 0)

	prevList := pactlListModulesFn
	pactlListModulesFn = func() (string, error) {
		return "10\tmodule-null-sink\tsink_name=citadel_meeting_orphan1 sink_properties=device.description=citadel_meeting_orphan1\n" +
			"11\tmodule-null-sink\tsink_name=not_a_meeting_sink sink_properties=device.description=not_a_meeting_sink\n" +
			"12\tmodule-null-sink\tsink_name=citadel_meeting_orphan2 sink_properties=device.description=citadel_meeting_orphan2\n", nil
	}
	t.Cleanup(func() { pactlListModulesFn = prevList })

	var unloaded []string
	prevUnload := pactlUnloadModuleFn
	pactlUnloadModuleFn = func(id string) error {
		unloaded = append(unloaded, id)
		return nil
	}
	t.Cleanup(func() { pactlUnloadModuleFn = prevUnload })

	ReapOrphanedMeetingSinks(dir)

	want := map[string]bool{"10": true, "12": true}
	for _, id := range unloaded {
		if id == "11" {
			t.Errorf("must never unload a non-meeting sink, unloaded %v", unloaded)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("did not unload all orphaned meeting sinks; missed %v (unloaded %v)", want, unloaded)
	}
}

// TestReapOrphanedMeetingSinks_SkipsWhenOwnerAlive verifies the sink sweep
// never even lists modules -- let alone unloads one -- when a live owner is
// recorded, since a live owner may legitimately still be using a sink.
func TestReapOrphanedMeetingSinks_SkipsWhenOwnerAlive(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getppid(), 0, 0)

	listCalled := false
	prevList := pactlListModulesFn
	pactlListModulesFn = func() (string, error) {
		listCalled = true
		return "10\tmodule-null-sink\tsink_name=citadel_meeting_live sink_properties=device.description=citadel_meeting_live\n", nil
	}
	t.Cleanup(func() { pactlListModulesFn = prevList })

	var unloaded []string
	prevUnload := pactlUnloadModuleFn
	pactlUnloadModuleFn = func(id string) error {
		unloaded = append(unloaded, id)
		return nil
	}
	t.Cleanup(func() { pactlUnloadModuleFn = prevUnload })

	ReapOrphanedMeetingSinks(dir)

	if listCalled {
		t.Error("sink sweep must not list pactl modules at all when the owner is alive")
	}
	if len(unloaded) != 0 {
		t.Errorf("sink sweep must not unload anything when the owner is alive, unloaded %v", unloaded)
	}
}

// TestMeetingBrowser_CloseLockedDeletesPidfile pins that closeLocked (the
// common teardown path for Close() and the CDP-not-ready failure inside
// Start()) deletes the pidfile but leaves the persistent profile directory
// itself untouched. Exercises closeLocked directly with no browser/Xvfb
// fields set, so no real process is involved.
func TestMeetingBrowser_CloseLockedDeletesPidfile(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 777, 778)

	b := &MeetingBrowser{profileDir: dir}
	if err := b.closeLocked(); err != nil {
		t.Fatalf("closeLocked: unexpected error: %v", err)
	}

	if _, err := os.Stat(meetingPidfilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("closeLocked must delete the pidfile, stat err=%v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("closeLocked must NOT delete the persistent profile directory, stat err=%v", err)
	}
	if b.profileDir != "" {
		t.Errorf("closeLocked must clear the in-memory profileDir field, got %q", b.profileDir)
	}
}

// TestMeetingBrowser_CloseLockedNoPidfileIsSafe verifies closeLocked on a
// browser that never wrote a pidfile (e.g. Close() called before Start()
// ever launched a process) is a harmless no-op, not an error.
func TestMeetingBrowser_CloseLockedNoPidfileIsSafe(t *testing.T) {
	b := &MeetingBrowser{}
	if err := b.closeLocked(); err != nil {
		t.Fatalf("closeLocked on a never-started browser: unexpected error: %v", err)
	}
}

// TestMeetingOwnerAlive_TreatsOwnPIDAsAlive pins the OPPOSITE of what
// cobrowse's sweep does, deliberately: unlike sweep() (which runs exactly
// once per process, before its own first StartSession, so a self-owned
// pidfile can never exist yet), reapMeetingProcessOrphans/
// ReapOrphanedMeetingSinks run on EVERY MeetingBrowser.Start()/
// hostMedia.Start(), including a second, genuinely concurrent meeting in the
// SAME process (citadel-cli#489). A pidfile naming this process as owner
// means a first meeting THIS process is still running, not an orphan --
// excluding self-PID here would make a live second Start() reap the first,
// still-running meeting's Chrome/Xvfb/sink out from under it.
func TestMeetingOwnerAlive_TreatsOwnPIDAsAlive(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 1, 2)
	if _, alive := meetingOwnerAlive(dir); !alive {
		t.Error("meetingOwnerAlive must report this process's own pid as alive -- a same-process pidfile owner is a live sibling meeting, not an orphan")
	}
}

// TestReapMeetingProcessOrphans_SkipsSameProcessOwner is the discriminating
// regression test for the same hazard: a pidfile whose owner is THIS
// process (a live sibling meeting already running in it) must never be
// reaped, even though its recorded browser/Xvfb PIDs would otherwise
// verify as real chrome/Xvfb processes.
func TestReapMeetingProcessOrphans_SkipsSameProcessOwner(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 828281, 828282)
	installMeetingMatchingCmdlineReader(t) // would match/kill if reap got this far

	var killed []int
	prevKiller := pidKiller
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { pidKiller = prevKiller })

	reapMeetingProcessOrphans(dir)

	if len(killed) != 0 {
		t.Errorf("reap must never kill a same-process (sibling meeting) owner's browser/Xvfb pids, killed %v", killed)
	}
	if _, err := os.Stat(meetingPidfilePath(dir)); err != nil {
		t.Errorf("a same-process owner's pidfile must be left in place, stat err=%v", err)
	}
}

// TestReapOrphanedMeetingSinks_SkipsSameProcessOwner is the sink-side
// analogue: a live sibling meeting in THIS process, with a REAL (already
// launched -- non-zero chrome/xvfb) pidfile, must keep its sink. Uses a real
// pidfile shape deliberately, not a placeholder: see
// meetingSinkSweepBlocked's doc comment for why a same-process PLACEHOLDER
// (chrome==0 && xvfb==0) is, by contrast, NOT a block -- it cannot own a
// sink, since nothing has launched yet.
func TestReapOrphanedMeetingSinks_SkipsSameProcessOwner(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 606001, 606002)

	listCalled := false
	prevList := pactlListModulesFn
	pactlListModulesFn = func() (string, error) {
		listCalled = true
		return "10\tmodule-null-sink\tsink_name=citadel_meeting_sibling sink_properties=device.description=citadel_meeting_sibling\n", nil
	}
	t.Cleanup(func() { pactlListModulesFn = prevList })

	var unloaded []string
	prevUnload := pactlUnloadModuleFn
	pactlUnloadModuleFn = func(id string) error {
		unloaded = append(unloaded, id)
		return nil
	}
	t.Cleanup(func() { pactlUnloadModuleFn = prevUnload })

	ReapOrphanedMeetingSinks(dir)

	if listCalled {
		t.Error("sink sweep must not list pactl modules at all for a same-process (sibling meeting) real owner")
	}
	if len(unloaded) != 0 {
		t.Errorf("sink sweep must not unload a same-process sibling meeting's sink, unloaded %v", unloaded)
	}
}

// TestReapOrphanedMeetingSinks_SameProcessPlaceholderDoesNotBlock is the
// deliberate CONTRAST to the test above: a same-process PLACEHOLDER
// (chrome==0 && xvfb==0 -- what MarkMeetingProfileOwned writes BEFORE any
// browser launches) must NOT block the sweep, or the sweep would never run
// at all for the ordinary sequential case (hostMedia.Start() always marks
// itself owned immediately before calling ReapOrphanedMeetingSinks). Pins
// the exact distinction meetingSinkSweepBlocked's doc comment describes.
func TestReapOrphanedMeetingSinks_SameProcessPlaceholderDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 0, 0)

	prevList := pactlListModulesFn
	pactlListModulesFn = func() (string, error) {
		return "10\tmodule-null-sink\tsink_name=citadel_meeting_stale sink_properties=device.description=citadel_meeting_stale\n", nil
	}
	t.Cleanup(func() { pactlListModulesFn = prevList })

	var unloaded []string
	prevUnload := pactlUnloadModuleFn
	pactlUnloadModuleFn = func(id string) error {
		unloaded = append(unloaded, id)
		return nil
	}
	t.Cleanup(func() { pactlUnloadModuleFn = prevUnload })

	ReapOrphanedMeetingSinks(dir)

	if len(unloaded) != 1 || unloaded[0] != "10" {
		t.Errorf("a same-process PLACEHOLDER owner must not block the sweep (nothing has launched yet to protect), unloaded %v", unloaded)
	}
}

func TestMeetingPidfilePath(t *testing.T) {
	got := meetingPidfilePath("/tmp/profile")
	want := filepath.Join("/tmp/profile", meetingPidfileName)
	if got != want {
		t.Errorf("meetingPidfilePath = %q, want %q", got, want)
	}
}

// TestMarkMeetingProfileOwned_WritesPlaceholderWhenUnowned is the common
// case: a fresh profile (no pidfile yet, or a dead owner) gets a placeholder
// naming this process, and wrote reports true so the caller knows to clean
// it up later.
func TestMarkMeetingProfileOwned_WritesPlaceholderWhenUnowned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meeting-profile")

	got, wrote, err := MarkMeetingProfileOwned(dir)
	if err != nil {
		t.Fatalf("MarkMeetingProfileOwned: unexpected error: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true for an unowned profile")
	}
	if got != dir {
		t.Errorf("resolved profileDir = %q, want %q", got, dir)
	}
	owner, chrome, xvfb := readMeetingPidfile(dir)
	if owner != os.Getpid() || chrome != 0 || xvfb != 0 {
		t.Errorf("placeholder pidfile = (%d,%d,%d), want (%d,0,0)", owner, chrome, xvfb, os.Getpid())
	}
}

// TestMarkMeetingProfileOwned_DoesNotOverwriteLiveOwner is the corruption
// guard: MarkMeetingProfileOwned must NEVER overwrite a pidfile that already
// shows a live owner -- doing so unconditionally is exactly what would
// clobber a genuinely in-flight launch's real chrome/xvfb PIDs (e.g. a
// second hostMedia.Start() racing in after the first has already written
// its real pidfile but is still inside waitForCDPReady).
func TestMarkMeetingProfileOwned_DoesNotOverwriteLiveOwner(t *testing.T) {
	dir := t.TempDir()
	// A real, in-flight launch: owner is THIS process (alive by definition),
	// with real (non-placeholder) chrome/xvfb PIDs already recorded.
	writeMeetingPidfile(dir, os.Getpid(), 777001, 777002)

	got, wrote, err := MarkMeetingProfileOwned(dir)
	if err != nil {
		t.Fatalf("MarkMeetingProfileOwned: unexpected error: %v", err)
	}
	if wrote {
		t.Fatal("expected wrote=false when a live owner already exists -- must not have overwritten it")
	}
	if got != dir {
		t.Errorf("resolved profileDir = %q, want %q", got, dir)
	}
	owner, chrome, xvfb := readMeetingPidfile(dir)
	if owner != os.Getpid() || chrome != 777001 || xvfb != 777002 {
		t.Errorf("pidfile was corrupted by MarkMeetingProfileOwned: got (%d,%d,%d), want (%d,777001,777002)",
			owner, chrome, xvfb, os.Getpid())
	}
}

// TestClearMeetingProfilePlaceholder_ClearsExactPlaceholder verifies the
// happy path: a placeholder this process wrote is removed.
func TestClearMeetingProfilePlaceholder_ClearsExactPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 0, 0)

	ClearMeetingProfilePlaceholder(dir)

	if _, err := os.Stat(meetingPidfilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("placeholder pidfile should be removed, stat err=%v", err)
	}
}

// TestClearMeetingProfilePlaceholder_DoesNotClearRealPidfile is the other
// half of the corruption guard: if the pidfile has since been upgraded to a
// REAL one (non-zero chrome/xvfb), clearing must be a no-op -- it must never
// delete a live launch's actual PIDs just because this process happened to
// write the placeholder that preceded it.
func TestClearMeetingProfilePlaceholder_DoesNotClearRealPidfile(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 888001, 888002)

	ClearMeetingProfilePlaceholder(dir)

	owner, chrome, xvfb := readMeetingPidfile(dir)
	if owner != os.Getpid() || chrome != 888001 || xvfb != 888002 {
		t.Errorf("real pidfile was incorrectly cleared: got (%d,%d,%d), want (%d,888001,888002)",
			owner, chrome, xvfb, os.Getpid())
	}
}

// TestClearMeetingProfilePlaceholder_DoesNotClearForeignOwner verifies
// clearing never touches a pidfile owned by a DIFFERENT process (even one
// that happens to also be a placeholder shape) -- it only ever removes what
// THIS process itself wrote.
func TestClearMeetingProfilePlaceholder_DoesNotClearForeignOwner(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, deadMeetingPID, 0, 0)

	ClearMeetingProfilePlaceholder(dir)

	owner, _, _ := readMeetingPidfile(dir)
	if owner != deadMeetingPID {
		t.Errorf("a foreign-owner pidfile must not be cleared, owner now = %d", owner)
	}
}

// TestReapOrphanedMeetingSinks_PlaceholderProtectsConcurrentLoadWindow pins
// the citadel-cli#925 review's blocking finding directly, at the level the
// placeholder mechanism ALONE (a PID-keyed pidfile, without the additional
// AcquireMeetingProfileSetupLock hostMedia.Start() also takes -- see
// meetingSinkSweepBlocked's doc comment) actually protects: a DIFFERENT
// process. It reproduces the exact vulnerable steady state the review
// described -- meeting A (a sibling process, os.Getppid() standing in for
// "alive, not us") has already loaded its sink -- visible in the fake pactl
// module list -- but the real pidfile does not exist yet, since that is
// only written after Chrome launches; only A's OWN placeholder is on disk --
// and proves a concurrent caller's sweep (B, this test, a different PID)
// does not unload it.
//
// This test FAILS against the pre-fix code: without A's placeholder pidfile
// on disk at all (i.e. deleting the writeMeetingPidfile call below), the
// profile has no pidfile, meetingOwnerAlive reports "no live owner", and the
// sweep unloads citadel_meeting_A -- exactly the reported bug (pinned
// directly, without the placeholder, by the negative-control test below).
func TestReapOrphanedMeetingSinks_PlaceholderProtectsConcurrentLoadWindow(t *testing.T) {
	dir := t.TempDir()
	// The vulnerable steady state per the review: meeting A (a DIFFERENT,
	// still-alive process -- os.Getppid() stands in for "not us" the same
	// way TestReapMeetingProcessOrphans_SkipsLiveOwner already does) has
	// already loaded its sink (LoadSink has run) but NO real pidfile exists
	// yet -- only the placeholder A's own MarkMeetingProfileOwned wrote
	// before its sink sweep and LoadSink.
	const liveSinkModuleID = "77"
	writeMeetingPidfile(dir, os.Getppid(), 0, 0)

	prevList := pactlListModulesFn
	pactlListModulesFn = func() (string, error) {
		return liveSinkModuleID + "\tmodule-null-sink\tsink_name=citadel_meeting_A sink_properties=device.description=citadel_meeting_A\n", nil
	}
	t.Cleanup(func() { pactlListModulesFn = prevList })

	var unloaded []string
	prevUnload := pactlUnloadModuleFn
	pactlUnloadModuleFn = func(id string) error {
		unloaded = append(unloaded, id)
		return nil
	}
	t.Cleanup(func() { pactlUnloadModuleFn = prevUnload })

	// B's (this call's) own sweep, landing in the window A's placeholder
	// protects.
	ReapOrphanedMeetingSinks(dir)

	if len(unloaded) != 0 {
		t.Errorf("placeholder-protected sweep must not unload another live process's loading sink, unloaded module(s) %v", unloaded)
	}
}

// TestReapOrphanedMeetingSinks_WithoutPlaceholderReproducesTheRace is the
// explicit negative control for the test above: with NO placeholder written
// (the exact pre-fix hostMedia.Start() behavior -- sweep called with no
// prior pidfile at all), the sweep DOES unload the live sink. This pins that
// the protection above comes from the placeholder, not from some unrelated
// change to orphanSinkModuleIDs/ReapOrphanedMeetingSinks itself.
func TestReapOrphanedMeetingSinks_WithoutPlaceholderReproducesTheRace(t *testing.T) {
	dir := t.TempDir()
	const liveSinkModuleID = "77"
	prevList := pactlListModulesFn
	pactlListModulesFn = func() (string, error) {
		return liveSinkModuleID + "\tmodule-null-sink\tsink_name=citadel_meeting_A sink_properties=device.description=citadel_meeting_A\n", nil
	}
	t.Cleanup(func() { pactlListModulesFn = prevList })

	var unloaded []string
	prevUnload := pactlUnloadModuleFn
	pactlUnloadModuleFn = func(id string) error {
		unloaded = append(unloaded, id)
		return nil
	}
	t.Cleanup(func() { pactlUnloadModuleFn = prevUnload })

	// No MarkMeetingProfileOwned call: pre-fix hostMedia.Start() behavior.
	ReapOrphanedMeetingSinks(dir)

	if len(unloaded) != 1 || unloaded[0] != liveSinkModuleID {
		t.Fatalf("expected the unprotected sweep to reproduce the race and unload module %q, got %v",
			liveSinkModuleID, unloaded)
	}
}
