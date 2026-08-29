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

// TestMeetingOwnerAlive_IgnoresOwnPID pins that a pidfile naming THIS
// process as owner is never treated as a live foreign owner -- mirroring
// cobrowse's sweep, which excludes its own PID from the live-owner check.
func TestMeetingOwnerAlive_IgnoresOwnPID(t *testing.T) {
	dir := t.TempDir()
	writeMeetingPidfile(dir, os.Getpid(), 1, 2)
	if _, alive := meetingOwnerAlive(dir); alive {
		t.Error("meetingOwnerAlive must not report this process's own pid as a live foreign owner")
	}
}

func TestMeetingPidfilePath(t *testing.T) {
	got := meetingPidfilePath("/tmp/profile")
	want := filepath.Join("/tmp/profile", meetingPidfileName)
	if got != want {
		t.Errorf("meetingPidfilePath = %q, want %q", got, want)
	}
}
