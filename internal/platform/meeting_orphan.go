// internal/platform/meeting_orphan.go
//
// Orphan reaper for the meeting bot (issue #488, the reaper half; the
// cancellation half shipped separately as #894 -- see meeting_join.go and the
// CLAUDE.md "Meeting-bot wait-loop cancellation" section). A SIGKILL (the
// watchdog's grace-period force-exit, an external kill -9, or an OOM) skips
// every Go defer, so MeetingBrowser.Close/NullSinkRecorder.Stop never run --
// leaking the Chrome process, its Xvfb virtual display, and the PulseAudio
// null sink it was routed into. This file is the startup/SIGKILL backstop
// that reclaims those three resources.
//
// Simpler than cobrowse's multi-session sweep (cobrowse_session.go): the
// meeting bot has exactly ONE persistent, signed-in profile directory (issue
// #5122; resolveMeetingProfileDir), not a per-session throwaway dir, so there
// is exactly one pidfile to reason about rather than a directory of them, and
// the pidfile is never deleted along with its profile (the profile survives
// forever -- only the pidfile's OWN removal on clean shutdown, and its
// overwrite on the next launch, keep it current).
//
// Identity verification reuses cobrowse's seams directly:
//   - pidAlive / pidKiller (cobrowse_orphan.go) to check/kill a PID.
//   - processMatchesRole + roleCobrowseBrowser/roleCobrowseXvfb
//     (cobrowse_identity.go) to fail closed on a recycled PID before killing
//     it -- a meeting node's pidfile survives a reboot exactly like
//     cobrowse's session dirs do (plain state dir, not tmpfs), so the same
//     "verify before kill" discipline applies here.
//
// Sink reaping is a second, independent mechanism (no pidfile per sink --
// PulseAudio modules carry no PID) built on the same live/dead OWNER check:
// pactl list short modules is enumerated for citadel_meeting_* sinks and they
// are unloaded ONLY when the meeting pidfile's owner is not alive. This is
// deliberately conservative -- with a live owner, a sink sweep is skipped
// entirely rather than trying to distinguish "this sink belongs to the live
// owner" from "this is a second orphan the live owner doesn't know about"; a
// live owner plus an accumulated stray sink is not a case this node's
// single-profile, single-concurrent-meeting design can produce anyway (see
// acquireMeetingProfileLock in meeting_browser.go).
//
// PLACEMENT IS LOAD-BEARING for the sink sweep: it must run BEFORE
// NullSinkRecorder.LoadSink creates the CURRENT meeting's sink (see
// hostMedia.Start in internal/jobs/meeting_media.go), or the sweep would
// enumerate and unload the sink it was just asked to protect. Reaping is
// therefore NOT invoked from MeetingBrowser.Start (chrome/Xvfb reap only) --
// callers that own the recorder call ReapOrphanedMeetingSinks explicitly, at
// the right point in their own startup sequence.
package platform

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// meetingPidfileName is the single, fixed pidfile recording the owning
// citadel process PID plus the browser and Xvfb child PIDs for the ONE
// persistent meeting profile. Unlike cobrowse's per-session pidfile, this
// lives INSIDE the persistent profile dir and is never removed along with
// it -- only its own contents are overwritten (on each successful launch) or
// removed (on graceful Close, so a clean shutdown never looks like an
// orphan).
const meetingPidfileName = ".citadel-meeting-pids"

// meetingPidfilePath returns the fixed pidfile path under a resolved meeting
// profile directory.
func meetingPidfilePath(profileDir string) string {
	return filepath.Join(profileDir, meetingPidfileName)
}

// writeMeetingPidfile records the owning process PID plus the browser and
// Xvfb child PIDs, mirroring cobrowse's writeSessionPidfile format (one PID
// per line: owner, browser, xvfb). Best-effort: a write failure is not fatal
// to the caller (mirrors cobrowse's identical tradeoff -- the browser is
// already running either way; a future process just cannot reap it if
// SIGKILLed).
func writeMeetingPidfile(profileDir string, ownerPID, browserPID, xvfbPID int) {
	path := meetingPidfilePath(profileDir)
	content := fmt.Sprintf("%d\n%d\n%d\n", ownerPID, browserPID, xvfbPID)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		log.Printf("[meeting] failed to write pidfile %s: %v", path, err)
	}
}

// readMeetingPidfile parses the owner, browser, and Xvfb PIDs from the fixed
// pidfile under profileDir. Missing or malformed lines yield 0, never an
// error, mirroring cobrowse's readSessionPidfile -- a partial file still lets
// the reaper act on whatever it can identify.
func readMeetingPidfile(profileDir string) (ownerPID, browserPID, xvfbPID int) {
	data, err := os.ReadFile(meetingPidfilePath(profileDir))
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(data))
	if len(fields) > 0 {
		fmt.Sscanf(fields[0], "%d", &ownerPID)
	}
	if len(fields) > 1 {
		fmt.Sscanf(fields[1], "%d", &browserPID)
	}
	if len(fields) > 2 {
		fmt.Sscanf(fields[2], "%d", &xvfbPID)
	}
	return ownerPID, browserPID, xvfbPID
}

// removeMeetingPidfile deletes the pidfile only -- never the profile
// directory itself, which holds the persistent signed-in Google session
// (issue #5122) and must survive every browser teardown. Best-effort: a
// removal failure just means a future reap sees stale (and, since the
// current process's PID is by then no longer alive, orphaned-looking)
// content, which is the same state a SIGKILL would have left anyway.
func removeMeetingPidfile(profileDir string) {
	if profileDir == "" {
		return
	}
	if err := os.Remove(meetingPidfilePath(profileDir)); err != nil && !os.IsNotExist(err) {
		log.Printf("[meeting] failed to remove pidfile under %s: %v", profileDir, err)
	}
}

// meetingOwnerAlive reports whether the pidfile under profileDir names a
// STILL-LIVE owning process. Shared by both reap halves (process + sink) so
// they agree on what counts as "orphaned".
//
// Deliberately does NOT exclude os.Getpid(), unlike cobrowse's sweep()'s
// analogous check. cobrowse's exclusion is safe only because sweep() runs
// exactly ONCE per process, via sweepOnce.Do, before that process's first
// StartSession -- so a session dir can never yet belong to this process at
// sweep time, and the self-PID case is structurally unreachable there. This
// reap has no once-guard: it runs on EVERY MeetingBrowser.Start() and every
// hostMedia.Start(), including a SECOND, genuinely concurrent meeting in the
// SAME process (citadel-cli#489 made two overlapping meetings real --
// acquireMeetingProfileLock exists precisely because of it). If a live
// second Start() excluded its own PID here, it would see the pidfile
// (correctly still naming ITS OWN process, from the FIRST meeting's
// writeMeetingPidfile) as "no live owner" and reap the first meeting's
// still-running Chrome/Xvfb/sink out from under it. pidAlive(os.Getpid()) is
// always true, so simply not special-casing it already gives the correct
// answer: same-process is alive, exactly like any other live PID.
func meetingOwnerAlive(profileDir string) (ownerPID int, alive bool) {
	ownerPID, _, _ = readMeetingPidfile(profileDir)
	if ownerPID <= 0 {
		return ownerPID, false
	}
	return ownerPID, pidAlive(ownerPID)
}

// reapMeetingProcessOrphans reclaims a leaked Chrome + Xvfb pair left behind
// by a SIGKILLed/crashed prior process, verified against the persistent
// meeting profile's fixed pidfile. Called from MeetingBrowser.Start() --
// AFTER acquireMeetingProfileLock succeeds, see the call site's comment for
// why that ordering matters -- and before this run's own browser/Xvfb are
// launched, so a stale --user-data-dir lock or a stray Xvfb display from a
// dead process never lingers into the new attempt. A live owner (another
// citadel process, OR a live sibling meeting in THIS SAME process -- see
// meetingOwnerAlive) is left completely alone -- verified the same way
// cobrowse's sweep protects a live sibling worker.
//
// SECURITY (mirrors cobrowse's sweep exactly, cobrowse_session.go): the
// pidfile is an on-disk claim from a PRIOR process, not a guarantee -- PIDs
// are recycled and this profile dir survives a reboot. processMatchesRole
// re-verifies each PID's /proc cmdline against the expected browser/Xvfb
// binary marker immediately before killing it; a PID that fails that check is
// never killed, only skipped.
func reapMeetingProcessOrphans(profileDir string) {
	if profileDir == "" {
		return
	}
	if _, alive := meetingOwnerAlive(profileDir); alive {
		return
	}
	_, browserPID, xvfbPID := readMeetingPidfile(profileDir)
	if browserPID <= 0 && xvfbPID <= 0 {
		return
	}
	reapMeetingPIDIfVerified(browserPID, roleCobrowseBrowser, "browser")
	reapMeetingPIDIfVerified(xvfbPID, roleCobrowseXvfb, "Xvfb")
	// The pidfile itself is now stale (its owner is dead and anything it
	// named has been reaped or was never real); remove it so a later reap
	// does not repeatedly re-verify already-handled PIDs. The profile
	// DIRECTORY is untouched.
	removeMeetingPidfile(profileDir)
}

// reapMeetingPIDIfVerified kills pid only when processMatchesRole confirms it
// is still the process it was recorded as for role. Fails closed: a mismatch
// or unverifiable PID (already gone, EPERM, or a recycled PID now belonging
// to something else) is logged and left alone.
func reapMeetingPIDIfVerified(pid int, role cobrowseProcRole, label string) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if !processMatchesRole(pid, role) {
		log.Printf("[meeting] reap: pid %d recorded as %s no longer matches (or is unverifiable); skipping kill", pid, label)
		return
	}
	if err := pidKiller(pid); err != nil {
		log.Printf("[meeting] reap: failed to kill orphaned %s pid %d: %v", label, pid, err)
	}
}

// pactlListModulesFn lists loaded PulseAudio modules (`pactl list short
// modules`), one per line, tab-separated: id, module name, argument string.
// A package var (mirroring portOwnerLookup/pidKiller in cobrowse_orphan.go)
// so tests substitute deterministic output instead of depending on a real
// pactl/PulseAudio server.
var pactlListModulesFn = defaultPactlListModules

func defaultPactlListModules() (string, error) {
	if !isCommandAvailable("pactl") {
		// Non-Linux node, or a Linux node with no PulseAudio stack: nothing to
		// sweep. Mirrors audioStackAvailable's own graceful-absence handling.
		return "", nil
	}
	out, err := exec.Command("pactl", "list", "short", "modules").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// pactlUnloadModuleFn unloads one PulseAudio module by id. A package var for
// the same test-injection reason as pactlListModulesFn.
var pactlUnloadModuleFn = defaultPactlUnloadModule

func defaultPactlUnloadModule(moduleID string) error {
	return exec.Command("pactl", "unload-module", moduleID).Run()
}

// orphanSinkModuleIDs parses `pactl list short modules` output and returns
// the module ids of every module-null-sink whose sink_name carries the
// meetingSinkPrefix -- i.e. every meeting null sink currently loaded,
// regardless of which meeting created it. Pure function so the parsing is
// unit-testable against fixed sample output without a real pactl.
func orphanSinkModuleIDs(pactlOutput string) []string {
	var ids []string
	for _, line := range strings.Split(pactlOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		id, name, args := fields[0], fields[1], fields[2]
		if name != "module-null-sink" {
			continue
		}
		if !strings.Contains(args, "sink_name="+meetingSinkPrefix) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// ReapOrphanedMeetingSinks unloads every currently-loaded `citadel_meeting_*`
// null sink, but ONLY when the persistent meeting profile's pidfile shows no
// live owner -- a live owner means a meeting may legitimately still be using
// one, and this node's single-profile design means at most one meeting runs
// at a time, so there is no way to selectively identify "this one is safe"
// among several without that owner signal.
//
// CALLER CONTRACT (load-bearing, see the package doc comment above): this
// must run BEFORE the caller's own NullSinkRecorder.LoadSink for the CURRENT
// meeting. Since it only ever unloads sinks that already exist at call time,
// calling it before LoadSink is sufficient to guarantee the current
// meeting's own sink (not yet created) is never a candidate -- there is
// nothing extra to gate on the sink's own name/age. Exported so
// internal/jobs (hostMedia.Start) can call it directly; profileDirOverride
// has the exact same resolution semantics as NewMeetingBrowser's
// profileDirOverride (resolveMeetingProfileDir): pass "" to use
// EnvMeetingProfileDir / the ConfigDir() default.
func ReapOrphanedMeetingSinks(profileDirOverride string) {
	profileDir := resolveMeetingProfileDir(profileDirOverride)
	if _, alive := meetingOwnerAlive(profileDir); alive {
		return
	}
	out, err := pactlListModulesFn()
	if err != nil {
		log.Printf("[meeting] sink reap: failed to list pactl modules: %v", err)
		return
	}
	for _, id := range orphanSinkModuleIDs(out) {
		if err := pactlUnloadModuleFn(id); err != nil {
			log.Printf("[meeting] sink reap: failed to unload orphaned sink module %s: %v", id, err)
		}
	}
}
