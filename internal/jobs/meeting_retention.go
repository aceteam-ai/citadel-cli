// internal/jobs/meeting_retention.go
//
// Disk-safety retention for local meeting recordings (aceteam#5097).
//
// The lossless WAV is kept on the node by design, but it must not accumulate
// unbounded. At meeting-end (and whenever this sweep runs) we prune meeting
// recordings that are older than the configured retention window, and — under
// disk pressure — everything prunable regardless of age. A freshly recorded WAV
// whose backup upload has not (yet) confirmed is protected from the
// disk-pressure branch so a full disk can never eat an un-backed-up recording.
package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/disk"
)

const (
	// diskPressureFreeBytesFloor: below this much free space on the workspace
	// filesystem, the sweep prunes all prunable recordings regardless of age.
	diskPressureFreeBytesFloor = 2 * 1024 * 1024 * 1024 // 2 GiB
	// diskPressureUsedPercentCeil: at/above this used%, likewise treat the node
	// as under disk pressure (covers small disks where 2 GiB free is a lot).
	diskPressureUsedPercentCeil = 92.0
)

// meetingFileInfo is one candidate for the retention sweep. Kept minimal so the
// selection logic is a pure, table-tested function decoupled from the
// filesystem.
type meetingFileInfo struct {
	// Path is the absolute file path.
	Path string
	// ModTime is when the file was last written (its age reference).
	ModTime time.Time
	// Protected marks a file the sweep must never delete — e.g. a WAV whose
	// backup upload has not been confirmed. Protected files are excluded from
	// BOTH the age branch and the disk-pressure branch.
	Protected bool
}

// selectPrunableMeetingFiles returns the paths to delete. A non-protected file
// is pruned when it is older than maxAge, OR when the node is under disk
// pressure (in which case age is ignored). Protected files are never returned.
//
// Pure (no I/O, no clock) so both branches are unit-tested table-driven. The
// input order is preserved in the output so callers/tests are deterministic.
func selectPrunableMeetingFiles(files []meetingFileInfo, now time.Time, maxAge time.Duration, diskPressure bool) []string {
	var prune []string
	for _, f := range files {
		if f.Protected {
			continue
		}
		expired := now.Sub(f.ModTime) >= maxAge
		if expired || diskPressure {
			prune = append(prune, f.Path)
		}
	}
	return prune
}

// meetingDirUnderDiskPressure reports whether the filesystem holding dir is low
// on space. Best-effort: an error reading usage returns false (do NOT prune
// aggressively on unknown state). Uses gopsutil so it is cross-platform.
func meetingDirUnderDiskPressure(dir string) bool {
	u, err := disk.Usage(dir)
	if err != nil || u == nil {
		return false
	}
	if u.Free < diskPressureFreeBytesFloor {
		return true
	}
	return u.UsedPercent >= diskPressureUsedPercentCeil
}

// pruneMeetingRecordings sweeps {workspace}/meetings for stale .wav/.opus files
// and deletes those the selection logic returns. protectPaths (absolute) are
// never deleted — the caller passes the current run's WAV when its backup did
// not confirm, so a disk-pressure sweep cannot eat it. Best-effort: every step
// logs and continues; a prune failure never fails the meeting job.
func (h *MeetingJoinHandler) pruneMeetingRecordings(ctx JobContext, maxAge time.Duration, protectPaths ...string) {
	meetingsDir := filepath.Join(h.WorkspaceDir, "meetings")
	entries, err := os.ReadDir(meetingsDir)
	if err != nil {
		// No meetings dir yet (or unreadable) — nothing to prune.
		return
	}

	protected := make(map[string]struct{}, len(protectPaths))
	for _, p := range protectPaths {
		if p != "" {
			protected[p] = struct{}{}
		}
	}

	var candidates []meetingFileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".wav") && !strings.HasSuffix(lower, ".opus") {
			continue
		}
		abs := filepath.Join(meetingsDir, name)
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		_, isProtected := protected[abs]
		candidates = append(candidates, meetingFileInfo{
			Path:      abs,
			ModTime:   info.ModTime(),
			Protected: isProtected,
		})
	}
	if len(candidates) == 0 {
		return
	}

	pressure := h.diskUnderPressure(meetingsDir)
	toPrune := selectPrunableMeetingFiles(candidates, time.Now(), maxAge, pressure)
	for _, p := range toPrune {
		if err := os.Remove(p); err != nil {
			ctx.Log("warn", "     - meeting retention: could not prune %s (non-fatal): %v", p, err)
			continue
		}
		ctx.Log("info", "     - meeting retention: pruned %s", p)
	}
}

// diskUnderPressure honors an injected probe (tests) and otherwise delegates to
// the real gopsutil-backed detector.
func (h *MeetingJoinHandler) diskUnderPressure(dir string) bool {
	if h.diskPressureFn != nil {
		return h.diskPressureFn(dir)
	}
	return meetingDirUnderDiskPressure(dir)
}
