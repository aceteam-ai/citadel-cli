//go:build darwin

package power

import (
	"os/exec"
	"strings"
)

// NewInhibitor returns a macOS sleep inhibitor backed by `caffeinate -s`, which
// prevents system idle sleep while the process is held. We deliberately do NOT
// pass -d, so the display may still sleep — a headless node only needs the
// system to stay reachable, not the screen lit.
func NewInhibitor() Inhibitor {
	return newProcInhibitor("caffeinate", "-s")
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
