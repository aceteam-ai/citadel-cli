//go:build darwin

package power

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// NewInhibitor returns a macOS sleep inhibitor backed by `caffeinate -s`, which
// prevents system idle sleep while the process is held. We deliberately do NOT
// pass -d, so the display may still sleep — a headless node only needs the
// system to stay reachable, not the screen lit.
//
// We also pass `-w <pid>` (the Citadel PID) so caffeinate exits — releasing the
// assertion — the moment Citadel exits, no matter how: a normal Stop kills the
// child, but if Citadel exits via os.Exit, SIGKILL, or a crash without running
// our cleanup, the `-w` watch still tears the assertion down. Without it an
// orphaned caffeinate (reparented to init, not reaped) would keep the Mac awake
// forever.
func NewInhibitor() Inhibitor {
	return newProcInhibitor("caffeinate", "-s", "-w", strconv.Itoa(os.Getpid()))
}

// DetectPowerSource shells out to `pmset -g batt` and parses its output. On a
// machine with no battery (desktop Mac), pmset reports AC power.
func DetectPowerSource() Source {
	out, err := exec.Command("pmset", "-g", "batt").Output()
	if err != nil {
		return SourceUnknown
	}
	return parsePmsetBatt(string(out))
}

// parsePmsetBatt extracts the power source from `pmset -g batt` output. The
// first line looks like:
//
//	Now drawing from 'AC Power'
//	Now drawing from 'Battery Power'
//
// Pulled out as a pure function so it can be unit-tested with sample output.
func parsePmsetBatt(out string) Source {
	lower := strings.ToLower(out)
	switch {
	case strings.Contains(lower, "'ac power'"), strings.Contains(lower, "drawing from 'ac"):
		return SourceAC
	case strings.Contains(lower, "'battery power'"), strings.Contains(lower, "drawing from 'battery"):
		return SourceBattery
	default:
		return SourceUnknown
	}
}
