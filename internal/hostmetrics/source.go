package hostmetrics

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"strings"
)

type Budget struct {
	limits       Limits
	bytes, files int
	exhausted    bool
}

// read admits a fixed source file before opening it, bounds bytes at the reader,
// and rejects short-budget/truncated input rather than publishing partial data.
func (b *Budget) read(ctx context.Context, root fs.FS, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.files >= b.limits.MaxFiles || b.bytes >= b.limits.MaxReadBytes {
		b.exhausted = true
		return nil, errors.New("metrics read budget exhausted")
	}
	b.files++
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limit := min(b.limits.MaxFileBytes, b.limits.MaxReadBytes-b.bytes)
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	b.bytes += len(data)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		b.exhausted = true
		return nil, errors.New("metrics input exceeds budget")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

// ProcessRef is supplied by trusted local lifecycle code, not request input.
// StartTicks pins the exact process incarnation, preventing PID reuse from
// attributing an unrelated process's usage. Role selects a fixed private slot.
type ProcessRef struct {
	PID        int
	StartTicks uint64
	Role       string
}

// ProcSource reads only known kernel telemetry files. It is never constructed
// by an active command. A future native wiring slice supplies the root FS and
// scoped process pins. GPUs/jobs are separate typed providers, not shell probes.
type ProcSource struct {
	FS        fs.FS
	Processes []ProcessRef
	PageSize  int
	GPURead   func(context.Context) (Group, error)
	JobRead   func(context.Context) (Group, error)
}

func (s *ProcSource) Read(ctx context.Context, b *Budget) (Snapshot, error) {
	if s.FS == nil {
		return Snapshot{}, errors.New("missing metrics filesystem")
	}
	result := Snapshot{Groups: map[Family]Group{}}
	read := func(path string, parse func(string) ([]Reading, error), family Family) {
		data, err := b.read(ctx, s.FS, path)
		if err != nil {
			return
		}
		values, err := parse(string(data))
		if err == nil && len(values) > 0 {
			result.Groups[family] = Group{Known: true, Readings: values}
		}
	}
	read("proc/stat", parseCPU, CPU)
	read("proc/meminfo", parseMemory, Memory)
	read("proc/diskstats", parseDisk, Disk)
	read("proc/net/dev", parseNetwork, Network)
	// cpu0's counter is explicitly one sensor, never a claim about every core.
	read("sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_count", func(v string) ([]Reading, error) {
		n, e := number(strings.TrimSpace(v))
		return []Reading{{ThrottleEvents, "cpu0", n}}, e
	}, Throttle)
	// Only fixed, explicitly bounded thermal-zone paths; no recursive sysfs walk.
	var temps []Reading
	for i := 0; i < b.limits.MaxSlots; i++ {
		data, err := b.read(ctx, s.FS, "sys/class/thermal/thermal_zone"+strconv.Itoa(i)+"/temp")
		if err != nil {
			continue
		}
		n, e := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if e == nil && n >= -273150 && n <= 300000 {
			temps = append(temps, Reading{TemperatureCelsius, strconv.Itoa(i), n / 1000})
		}
	}
	if len(temps) > 0 {
		result.Groups[Temperature] = Group{true, temps}
	}
	if len(s.Processes) > b.limits.MaxSlots {
		return Snapshot{}, errors.New("process capacity exceeded")
	}
	var processes []Reading
	roles := map[string]bool{}
	for _, p := range s.Processes {
		if p.PID < 1 || p.StartTicks == 0 || (p.Role != "worker" && p.Role != "collector") || roles[p.Role] {
			return Snapshot{}, errors.New("invalid process pin")
		}
		roles[p.Role] = true
		data, err := b.read(ctx, s.FS, "proc/"+strconv.Itoa(p.PID)+"/stat")
		if err != nil {
			continue
		}
		// comm may contain spaces and ')' characters. Never retain or export it.
		end := strings.LastIndex(string(data), ")")
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 20 {
			continue
		}
		start, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil || start != p.StartTicks {
			continue
		}
		ut, err1 := number(fields[11])
		st, err2 := number(fields[12])
		if err1 != nil || err2 != nil {
			continue
		}
		// RSS and start time come from the same stat record: a PID replacement
		// between separate stat/status reads cannot mix process incarnations.
		if len(fields) < 22 || s.PageSize < 1024 || s.PageSize > 1<<20 || s.PageSize&(s.PageSize-1) != 0 {
			return Snapshot{}, errors.New("invalid process page size or stat")
		}
		rss, e := number(fields[21])
		if e != nil {
			continue
		}
		processes = append(processes, Reading{ProcessTicks, p.Role, ut + st}, Reading{ProcessResidentBytes, p.Role, rss * float64(s.PageSize)})
	}
	if len(processes) > 0 {
		result.Groups[Process] = Group{true, processes}
	}
	for _, adapter := range []struct {
		f  Family
		fn func(context.Context) (Group, error)
	}{{GPU, s.GPURead}, {Job, s.JobRead}} {
		if adapter.fn != nil {
			g, err := adapter.fn(ctx)
			if err == nil {
				result.Groups[adapter.f] = g
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	// Any hard budget exhaustion invalidates the scrape, even if a sensor was
	// absent. Do not misrepresent a resource-limit refusal as hardware absence.
	if b.exhausted {
		return Snapshot{}, errors.New("metrics budget exhausted")
	}
	return result, nil
}
func number(s string) (float64, error) { n, e := strconv.ParseUint(s, 10, 64); return float64(n), e }
func parseCPU(text string) ([]Reading, error) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && f[0] == "cpu" {
			var total, idle float64
			// guest/guest_nice already count in user/nice; do not double count them.
			for i, v := range f[1:] {
				if i >= 8 {
					break
				}
				n, e := number(v)
				if e != nil {
					return nil, e
				}
				total += n
				if i == 3 {
					idle += n
				}
			}
			return []Reading{{CPUTicks, "host", total}, {CPUIdleTicks, "host", idle}}, nil
		}
	}
	return nil, errors.New("CPU observation unavailable")
}
func parseMemory(text string) ([]Reading, error) {
	var out []Reading
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "kB" {
			continue
		}
		var m Metric
		switch f[0] {
		case "MemTotal:":
			m = MemoryTotal
		case "MemAvailable:":
			m = MemoryAvailable
		default:
			continue
		}
		n, e := number(f[1])
		if e != nil {
			return nil, e
		}
		out = append(out, Reading{m, "host", n * 1024})
	}
	return out, nil
}
func parseDisk(text string) ([]Reading, error) {
	var out []Reading
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 14 {
			return nil, errors.New("invalid disk observation")
		}
		if strings.HasPrefix(f[2], "loop") || strings.HasPrefix(f[2], "ram") {
			continue
		}
		r, e := number(f[5])
		if e != nil {
			return nil, e
		}
		w, e := number(f[9])
		if e != nil {
			return nil, e
		}
		out = append(out, Reading{DiskReadBytes, f[2], r * 512}, Reading{DiskWriteBytes, f[2], w * 512})
	}
	return out, nil
}
func parseNetwork(text string) ([]Reading, error) {
	var out []Reading
	for _, line := range strings.Split(text, "\n") {
		name, values, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(values)
		if len(f) != 16 {
			return nil, errors.New("invalid network observation")
		}
		r, e := number(f[0])
		if e != nil {
			return nil, e
		}
		w, e := number(f[8])
		if e != nil {
			return nil, e
		}
		out = append(out, Reading{NetworkReceiveBytes, strings.TrimSpace(name), r}, Reading{NetworkTransmitBytes, strings.TrimSpace(name), w})
	}
	return out, nil
}
