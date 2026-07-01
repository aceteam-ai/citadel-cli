package footprint

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
)

// containerStat is one container's line from `<engine> stats --no-stream
// --format json`. Docker emits one JSON object per line; podman emits either
// per-line objects or a single JSON array — parseStatsJSON handles both.
//
// Field names follow docker's stats formatting keys (Name, CPUPerc, MemUsage).
// Podman's `--format json` uses the same capitalised keys, so a single struct
// covers both engines.
type containerStat struct {
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
}

// sampleContainerStats runs ONE `<engineBin> stats --no-stream --format json`
// and returns the parsed per-container rows. This is the single stats exec per
// tick — never one exec per service. A non-nil error means the stats command
// itself failed (engine not installed / daemon down); callers treat that as "no
// containers running" rather than aborting the tick.
func sampleContainerStats(ctx context.Context, engineBin string) ([]containerStat, error) {
	if engineBin == "" {
		engineBin = "docker"
	}
	cmd := exec.CommandContext(ctx, engineBin, "stats", "--no-stream", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseStatsJSON(out), nil
}

// parseStatsJSON parses the output of `<engine> stats --format json`, tolerating
// both docker's newline-delimited objects and podman's single JSON array.
func parseStatsJSON(out []byte) []containerStat {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	// Podman: a single JSON array.
	if strings.HasPrefix(trimmed, "[") {
		var arr []containerStat
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			return arr
		}
		return nil
	}
	// Docker: one JSON object per line.
	var stats []containerStat
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var cs containerStat
		if err := json.Unmarshal([]byte(line), &cs); err == nil {
			stats = append(stats, cs)
		}
	}
	return stats
}

// parseCPUPercent parses a docker/podman CPUPerc string such as "12.34%" into a
// float. Returns (0, false) when the field is empty or unparseable.
func parseCPUPercent(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseMemUsageMB parses the left-hand (used) side of a docker/podman MemUsage
// string such as "7.4GiB / 62.5GiB" or "512MiB / 16GiB" and returns the used
// value in MB. Returns (0, false) when the field is empty or unparseable.
//
// This is the load-bearing parse for the incident that motivated #422 (a service
// idling at 7.4 GB RSS), so unit handling (GiB/MiB/KiB/GB/MB/B) is explicit.
func parseMemUsageMB(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// Take the used side before the "/".
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	return parseByteSizeMB(strings.TrimSpace(s))
}

// parseByteSizeMB converts a size token like "7.4GiB" or "512MiB" into MB
// (megabytes, 1 MB = 1024*1024 bytes to match binary IEC units). It accepts both
// IEC (GiB/MiB/KiB) and the SI-style suffixes docker sometimes emits (GB/MB/kB).
func parseByteSizeMB(tok string) (float64, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return 0, false
	}
	lower := strings.ToLower(tok)

	// Ordered longest-suffix-first so "gib" is matched before "b".
	type unit struct {
		suffix   string
		bytesPer float64
	}
	units := []unit{
		{"gib", 1024 * 1024 * 1024},
		{"mib", 1024 * 1024},
		{"kib", 1024},
		{"gb", 1000 * 1000 * 1000},
		{"mb", 1000 * 1000},
		{"kb", 1000},
		{"b", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			num := strings.TrimSpace(lower[:len(lower)-len(u.suffix)])
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			return v * u.bytesPer / (1024 * 1024), true
		}
	}
	// No recognised suffix: treat as raw bytes.
	if v, err := strconv.ParseFloat(lower, 64); err == nil {
		return v / (1024 * 1024), true
	}
	return 0, false
}
