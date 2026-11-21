package hostmetrics

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func syntheticFS() fstest.MapFS {
	return fstest.MapFS{
		"proc/stat":                            {Data: []byte("cpu 10 1 5 20 3 1 2 4 9 1\n")},
		"proc/meminfo":                         {Data: []byte("MemTotal: 100 kB\nMemAvailable: 25 kB\n")},
		"proc/diskstats":                       {Data: []byte("8 0 secret-disk 1 0 4 0 1 0 6 0 0 0 0\n")},
		"proc/net/dev":                         {Data: []byte("Inter-| Receive | Transmit\n private-interface: 42 0 0 0 0 0 0 0 84 0 0 0 0 0 0 0\n")},
		"sys/class/thermal/thermal_zone0/temp": {Data: []byte("42000\n")},
		"sys/devices/system/cpu/cpu0/thermal_throttle/core_throttle_count": {Data: []byte("3\n")},
	}
}
func processStat(start int) []byte {
	f := make([]string, 22)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[11] = "7"
	f[12] = "3"
	f[19] = strconv.Itoa(start)
	f[21] = "4"
	return []byte("42 (private user ) secret command) " + strings.Join(f, " "))
}
func TestNativeSourceUsesOnlyBoundedSyntheticKernelFiles(t *testing.T) {
	root := syntheticFS()
	root["proc/42/stat"] = &fstest.MapFile{Data: processStat(100)}
	source := &ProcSource{FS: root, PageSize: 4096, Processes: []ProcessRef{{PID: 42, StartTicks: 100, Role: "worker"}}, GPURead: func(context.Context) (Group, error) {
		return Group{true, []Reading{{GPUUtilizationRatio, "privateGPU", .25}, {GPUMemoryBytes, "privateGPU", 512}}}, nil
	}}
	c, _ := New("1297", testLimits(), source)
	out, e := c.Collect(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, value := range []string{`citadel_cpu_ticks_total{node_id="1297",slot="0"} 46`, `citadel_cpu_idle_ticks_total{node_id="1297",slot="0"} 20`, `citadel_memory_available_bytes{node_id="1297",slot="0"} 25600`, `citadel_disk_read_bytes_total{node_id="1297",slot="0"} 2048`, `citadel_disk_written_bytes_total{node_id="1297",slot="0"} 3072`, `citadel_network_received_bytes_total{node_id="1297",slot="0"} 42`, `citadel_temperature_celsius{node_id="1297",slot="0"} 42`, `citadel_throttle_events_total{node_id="1297",slot="0"} 3`, `citadel_process_cpu_ticks_total{node_id="1297",slot="0"} 10`, `citadel_process_resident_bytes{node_id="1297",slot="0"} 16384`, `citadel_gpu_utilization_ratio{node_id="1297",slot="0"} 0.25`} {
		if !strings.Contains(out, value) {
			t.Fatalf("missing observation %s\n%s", value, out)
		}
	}
	for _, secret := range []string{"secret", "private", "command", "interface", "disk="} {
		if strings.Contains(out, secret) {
			t.Fatal("private telemetry text leaked", secret)
		}
	}
}
func TestMissingMalformedAndReusedProcessAreUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		root fstest.MapFS
	}{
		{"missing", fstest.MapFS{}}, {"malformed", fstest.MapFS{"proc/stat": {Data: []byte("cpu bad data\n")}}},
		{"reused PID", fstest.MapFS{"proc/42/stat": {Data: processStat(101)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := New("1", testLimits(), &ProcSource{FS: tc.root, PageSize: 4096, Processes: []ProcessRef{{42, 100, "worker"}}})
			out, e := c.Collect(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(out, "citadel_process_cpu_ticks_total") || !strings.Contains(out, `family="process"} 0`) {
				t.Fatal(out)
			}
		})
	}
}
func TestHardReadBudgetsRefuseWithoutPartialMetrics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Limits)
	}{
		{"file", func(l *Limits) { l.MaxFileBytes = 8 }},
		{"aggregate", func(l *Limits) { l.MaxReadBytes = 64; l.MaxFileBytes = 64 }},
		{"open attempts", func(l *Limits) { l.MaxFiles = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := testLimits()
			tc.change(&l)
			c, _ := New("1", l, &ProcSource{FS: syntheticFS()})
			out, e := c.Collect(context.Background())
			if e == nil || out != "" {
				t.Fatal("read budget produced partial success", out, e)
			}
		})
	}
}

type tripwireFS struct {
	fstest.MapFS
	t     *testing.T
	opens int
}

func (f *tripwireFS) Open(name string) (fs.File, error) {
	f.opens++
	if !(strings.HasPrefix(name, "proc/") || strings.HasPrefix(name, "sys/")) || strings.Contains(name, "cmdline") || strings.Contains(name, "environ") || strings.Contains(name, "citadel") || strings.Contains(name, "config") {
		f.t.Fatal("forbidden host path", name)
	}
	return f.MapFS.Open(name)
}
func TestReadCountAndNoHostStateAccess(t *testing.T) {
	root := &tripwireFS{MapFS: syntheticFS(), t: t}
	l := testLimits()
	c, _ := New("1", l, &ProcSource{FS: root})
	if _, e := c.Collect(context.Background()); e != nil {
		t.Fatal(e)
	}
	if root.opens != 5+l.MaxSlots {
		t.Fatalf("unexpected unbounded enumeration: %d", root.opens)
	}
}
func TestSourceParserRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		parse      func(string) ([]Reading, error)
	}{
		{"disk", "8 0 disk", parseDisk}, {"disk counter", "8 0 disk 0 0 bad 0 0 0 1 0 0 0 0", parseDisk},
		{"network", "eth: 1 2", parseNetwork}, {"network counter", "eth: bad 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0", parseNetwork},
		{"cpu", "cpu 1 bad 3 4", parseCPU},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := tc.parse(tc.text); e == nil {
				t.Fatal("accepted malformed kernel sample")
			}
		})
	}
}
func TestJobSeriesDoNotGrowWithJobCount(t *testing.T) {
	tracker, _ := NewJobTracker(1)
	source := &ProcSource{FS: fstest.MapFS{}, JobRead: func(context.Context) (Group, error) { g, _ := tracker.Snapshot(); return g, nil }}
	c, _ := New("1", testLimits(), source)
	for i := 0; i < 200; i++ {
		binding, e := tracker.Begin(fmt.Sprintf("synthetic-secret-job-%d", i), "inference", "inference")
		if e != nil {
			t.Fatal(e)
		}
		cpu, rss := float64(i), float64(i*100)
		if e = tracker.Observe(binding, &cpu, &rss); e != nil {
			t.Fatal(e)
		}
		out, e := c.Collect(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(out, "synthetic") || strings.Contains(out, "secret") || strings.Count(out, "citadel_job_") != 4 || !strings.Contains(out, `citadel_job_generation{node_id="1",slot="0"} `+strconv.Itoa(i+1)+"\n") {
			t.Fatal("unbounded or uncorrelated job output", out)
		}
		if e = tracker.End(binding); e != nil {
			t.Fatal(e)
		}
		if e = tracker.Observe(binding, &cpu, &rss); e == nil {
			t.Fatal("late observation was attributed to a finished job")
		}
	}
}
func TestJobAttributionUnknownAndStaleFences(t *testing.T) {
	tracker, _ := NewJobTracker(1)
	first, e := tracker.Begin("first", "meeting", "realtime")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tracker.Begin("overflow", "meeting", "realtime"); e == nil {
		t.Fatal("unbounded jobs")
	}
	g, bindings := tracker.Snapshot()
	if len(g.Readings) != 2 || len(bindings) != 1 || bindings[0].JobID != "first" {
		t.Fatal("unknown usage fabricated or mapping missing")
	}
	value := 2.0
	if e = tracker.Observe(first, &value, nil); e != nil {
		t.Fatal(e)
	}
	value = 3
	g, _ = tracker.Snapshot()
	if g.Readings[2].Value != 2 {
		t.Fatal("caller mutated stored usage")
	}
	backwards := 1.0
	if e = tracker.Observe(first, &backwards, nil); e == nil {
		t.Fatal("counter rollback")
	}
	if e = tracker.End(first); e != nil {
		t.Fatal(e)
	}
	second, _ := tracker.Begin("second", "file", "heavy")
	if e = tracker.End(first); e == nil {
		t.Fatal("old generation ended replacement job")
	}
	if second.Generation == first.Generation {
		t.Fatal("job generation reused")
	}
}

type countedFile struct {
	fs.File
	bytes *int
}

func (f countedFile) Read(p []byte) (int, error) { n, e := f.File.Read(p); *f.bytes += n; return n, e }

type countedFS struct {
	fstest.MapFS
	bytes int
}

func (f *countedFS) Open(name string) (fs.File, error) {
	r, e := f.MapFS.Open(name)
	if e != nil {
		return nil, e
	}
	return countedFile{r, &f.bytes}, nil
}
func TestReadBudgetStopsTheReaderBeforeAllocatingWholeFile(t *testing.T) {
	l := testLimits()
	l.MaxFileBytes = 64
	root := &countedFS{MapFS: fstest.MapFS{"proc/stat": {Data: []byte(strings.Repeat("x", 1<<20))}}}
	budget := &Budget{limits: l}
	if _, e := budget.read(context.Background(), root, "proc/stat"); e == nil {
		t.Fatal("accepted oversized input")
	}
	if root.bytes != 65 {
		t.Fatalf("reader consumed %d bytes, expected limit plus one", root.bytes)
	}
	if !budget.exhausted {
		t.Fatal("oversize was not a hard-budget refusal")
	}
}

var _ io.Reader = countedFile{}

func TestSameJobIDReplacementStillRequiresCurrentGeneration(t *testing.T) {
	tracker, _ := NewJobTracker(1)
	first, _ := tracker.Begin("same-job", "meeting", "realtime")
	_ = tracker.End(first)
	second, _ := tracker.Begin("same-job", "meeting", "realtime")
	value := 1.0
	if e := tracker.Observe(first, &value, nil); e == nil {
		t.Fatal("old generation updated same-ID replacement")
	}
	if e := tracker.End(first); e == nil {
		t.Fatal("old generation ended same-ID replacement")
	}
	_, bindings := tracker.Snapshot()
	if len(bindings) != 1 || bindings[0] != second {
		t.Fatal("replacement lost its identity")
	}
}

func TestJobCPUHighWaterSurvivesAnUnknownObservation(t *testing.T) {
	tracker, _ := NewJobTracker(1)
	binding, _ := tracker.Begin("same-job", "inference", "inference")
	initial := 100.0
	if e := tracker.Observe(binding, &initial, nil); e != nil {
		t.Fatal(e)
	}
	if e := tracker.Observe(binding, nil, nil); e != nil {
		t.Fatal(e)
	}
	group, _ := tracker.Snapshot()
	for _, reading := range group.Readings {
		if reading.Metric == JobCPUSeconds {
			t.Fatal("unknown CPU must stay absent from the exported samples")
		}
	}
	lower := 1.0
	if e := tracker.Observe(binding, &lower, nil); e == nil {
		t.Fatal("unknown interval erased the CPU counter high-water mark")
	}
	group, _ = tracker.Snapshot()
	for _, reading := range group.Readings {
		if reading.Metric == JobCPUSeconds {
			t.Fatal("rejected counter fabricated a current CPU sample")
		}
	}
	next := 101.0
	if e := tracker.Observe(binding, &next, nil); e != nil {
		t.Fatal(e)
	}
	if e := tracker.End(binding); e != nil {
		t.Fatal(e)
	}
	replacement, _ := tracker.Begin("same-job", "inference", "inference")
	if e := tracker.Observe(replacement, &lower, nil); e != nil {
		t.Fatal("a new job generation inherited the old counter floor", e)
	}
}
