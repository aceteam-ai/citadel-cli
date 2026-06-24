//go:build linux

package power

import (
	"os"
	"path/filepath"
	"strings"
)

// powerSupplyBase is the sysfs directory holding per-supply power state. It is
// a package var (not a const) so tests can point detection at a fixture tree.
var powerSupplyBase = "/sys/class/power_supply"

// NewInhibitor returns a Linux sleep inhibitor backed by
// `systemd-inhibit --what=idle:sleep ... sleep infinity`. logind grants the
// inhibition for the lifetime of the child process; killing it (or Citadel
// crashing) releases the lock. This avoids the logind D-Bus Inhibit() fd dance
// while staying fully CGO-free.
func NewInhibitor() Inhibitor {
	return newProcInhibitor(
		"systemd-inhibit",
		"--what=idle:sleep",
		"--who=citadel",
		"--why=Citadel Fabric node keep-awake",
		"--mode=block",
		"sleep", "infinity",
	)
}

// DetectPowerSource reads /sys/class/power_supply/AC*/online. A value of "1"
// means plugged in. When no AC supply is present (e.g. a desktop or VM without
// power_supply entries) the source is Unknown, which the gating logic treats as
// "not AC".
func DetectPowerSource() Source {
	return detectPowerSourceAt(powerSupplyBase)
}

// detectPowerSourceAt is the testable core: it scans a power_supply-style
// directory for a "Mains"/AC supply and reads its online flag.
func detectPowerSourceAt(base string) Source {
	entries, err := os.ReadDir(base)
	if err != nil {
		return SourceUnknown
	}

	found := false
	for _, e := range entries {
		name := e.Name()
		// AC adapters are reported as type "Mains"; conventional names are
		// AC, AC0, ACAD, ADP1, etc. Match on the directory name prefix and,
		// when available, the type file.
		if !isMainsSupply(base, name) {
			continue
		}
		found = true

		data, err := os.ReadFile(filepath.Join(base, name, "online"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(data)) {
		case "1":
			return SourceAC
		case "0":
			return SourceBattery
		}
	}

	if found {
		// We saw an AC supply but couldn't read a definitive online flag.
		return SourceUnknown
	}
	return SourceUnknown
}

// isMainsSupply reports whether the named power_supply entry is an AC/mains
// adapter, preferring the authoritative "type" file and falling back to the
// conventional name prefix.
func isMainsSupply(base, name string) bool {
	if data, err := os.ReadFile(filepath.Join(base, name, "type")); err == nil {
		return strings.EqualFold(strings.TrimSpace(string(data)), "Mains")
	}
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "AC") || strings.HasPrefix(upper, "ADP")
}
