package pulse

import (
	"context"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// nvidiaSMITimeout bounds the nvidia-smi subprocess so a hung driver can never
// stall a collection cycle (the heartbeat itself is already isolated from the
// collector, but a wedged cycle would stop refreshing the cache).
const nvidiaSMITimeout = 3 * time.Second

// nvidiaSMIQuery is the exact --query-gpu field list; parseNvidiaSMICSV
// depends on this order.
const nvidiaSMIQuery = "index,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw"

// collectGPUStats queries nvidia-smi for per-GPU utilization. It returns nil
// (omit the gpus array) when the binary is absent, times out, or errors —
// a CPU-only node is the normal case, not a failure.
func collectGPUStats(ctx context.Context) []GPUStat {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path,
		"--query-gpu="+nvidiaSMIQuery,
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSMICSV(string(out))
}

// parseNvidiaSMICSV parses `nvidia-smi --query-gpu=... --format=csv,noheader,nounits`
// output, one GPU per line. Fields nvidia-smi reports as unavailable
// ("[N/A]", "[Not Supported]", ...) are omitted from the entry rather than
// zero-filled. A row whose index does not parse falls back to its line order.
func parseNvidiaSMICSV(out string) []GPUStat {
	var gpus []GPUStat
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		// index, util, mem.used, mem.total, temp, power
		if len(fields) < 6 {
			continue
		}
		gpu := GPUStat{Index: len(gpus)}
		if v, ok := smiInt(fields[0]); ok {
			gpu.Index = v
		}
		if v, ok := smiFloat(fields[1]); ok {
			gpu.UtilPct = float64Ptr(v)
		}
		if v, ok := smiInt(fields[2]); ok {
			gpu.MemUsedMB = intPtr(v)
		}
		if v, ok := smiInt(fields[3]); ok {
			gpu.MemTotalMB = intPtr(v)
		}
		if v, ok := smiInt(fields[4]); ok {
			gpu.TempC = intPtr(v)
		}
		if v, ok := smiFloat(fields[5]); ok {
			gpu.PowerW = float64Ptr(round1(v))
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

// smiFloat parses one nvidia-smi CSV field, reporting ok=false for the
// sentinel "not available" markers so callers omit the field.
func smiFloat(field string) (float64, bool) {
	s := strings.TrimSpace(field)
	if s == "" || strings.HasPrefix(s, "[") || strings.EqualFold(s, "N/A") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func smiInt(field string) (int, bool) {
	v, ok := smiFloat(field)
	if !ok {
		return 0, false
	}
	return int(math.Round(v)), true
}
