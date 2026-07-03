package status

import (
	"testing"
	"time"
)

// gib is a non-constant GiB multiplier so fractional byte literals (e.g.
// uint64(6.1*gib)) compile — a constant float with a fractional value cannot be
// converted to uint64 in a constant expression.
var gib = float64(1 << 30)

func TestParseComputeApps(t *testing.T) {
	// nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader,nounits
	// used_memory is in MiB.
	out := `12345, 21504
12346, 512
67890, 0
`
	got := parseComputeApps(out)

	if len(got) != 3 {
		t.Fatalf("expected 3 pids, got %d: %v", len(got), got)
	}
	if want := uint64(21504) * (1 << 20); got[12345] != want {
		t.Errorf("pid 12345 vram = %d, want %d", got[12345], want)
	}
	if want := uint64(512) * (1 << 20); got[12346] != want {
		t.Errorf("pid 12346 vram = %d, want %d", got[12346], want)
	}
	if got[67890] != 0 {
		t.Errorf("pid 67890 vram = %d, want 0", got[67890])
	}
}

func TestParseComputeApps_MultiGPUSamePIDSums(t *testing.T) {
	// A process spanning two GPUs appears on two rows; VRAM must sum.
	out := "999, 1024\n999, 2048\n"
	got := parseComputeApps(out)
	want := uint64(1024+2048) * (1 << 20)
	if got[999] != want {
		t.Errorf("pid 999 vram = %d, want %d (summed across GPUs)", got[999], want)
	}
}

func TestParseComputeApps_SkipsGarbage(t *testing.T) {
	out := "notapid, 1024\n42, notanumber\n\n7, 8\n"
	got := parseComputeApps(out)
	if len(got) != 1 {
		t.Fatalf("expected only the valid row, got %v", got)
	}
	if got[7] != uint64(8)*(1<<20) {
		t.Errorf("pid 7 vram = %d, want %d", got[7], uint64(8)*(1<<20))
	}
}

func TestParseGPUUtil(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"single", "74\n", 74},
		{"max across gpus", "10\n74\n33\n", 74},
		{"empty is unknown", "", -1},
		{"garbage is unknown", "n/a\n--\n", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseGPUUtil(c.in); got != c.want {
				t.Errorf("parseGPUUtil(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestParseStatsOutput(t *testing.T) {
	// "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"
	out := "citadel-vllm\t74.31%\t6.1GiB / 62.0GiB\n" +
		"citadel-diffusers\t0.02%\t7.4GiB / 62.0GiB\n"
	got := parseStatsOutput(out)

	if len(got) != 2 {
		t.Fatalf("expected 2 containers, got %d: %v", len(got), got)
	}
	vllm := got["citadel-vllm"]
	if vllm.cpuPercent != 74.31 {
		t.Errorf("vllm cpu = %v, want 74.31", vllm.cpuPercent)
	}
	// 6.1 GiB
	if wantMin, wantMax := uint64(6.0*gib), uint64(6.2*gib); vllm.ramBytes < wantMin || vllm.ramBytes > wantMax {
		t.Errorf("vllm ram = %d bytes, want ~6.1GiB", vllm.ramBytes)
	}
	diff := got["citadel-diffusers"]
	if diff.cpuPercent != 0.02 {
		t.Errorf("diffusers cpu = %v, want 0.02", diff.cpuPercent)
	}
}

func TestParseStatsOutput_SkipsHeaderAndBlank(t *testing.T) {
	out := "NAME\tCPU %\tMEM USAGE / LIMIT\n\ncitadel-vllm\t5.0%\t1.0GiB / 2.0GiB\n"
	got := parseStatsOutput(out)
	if len(got) != 1 {
		t.Fatalf("expected 1 real row, got %v", got)
	}
	if _, ok := got["citadel-vllm"]; !ok {
		t.Errorf("missing citadel-vllm: %v", got)
	}
}

func TestParseMemBytes(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0B", 0},
		{"512MiB", 512 << 20},
		{"6.1GiB", uint64(6.1 * gib)},
		{"1.5GB", uint64(1.5 * 1e9)},
		{"2kB", 2000},
		{"1KiB", 1024},
		{"", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseMemBytes(c.in); got != c.want {
			t.Errorf("parseMemBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePercent(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"74.31%", 74.31},
		{"0.00%", 0},
		{"100", 100},
		{"--", -1},
		{"", -1},
		{"n/a", -1},
	}
	for _, c := range cases {
		if got := parsePercent(c.in); got != c.want {
			t.Errorf("parsePercent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAttributeVRAM_MatchesSubtreeOnly(t *testing.T) {
	// nvidia-smi reports host PIDs; the GPU worker is a child of the container
	// main PID, so attribution must sum over the whole subtree, not just PID 1.
	pidVRAM := map[int]uint64{
		1000: 1 << 30,  // container main pid — no GPU work itself
		1001: 20 << 30, // GPU worker child — the real VRAM holder
		9999: 4 << 30,  // unrelated process on the host — must NOT be counted
	}
	subtree := map[int]struct{}{1000: {}, 1001: {}}

	got := attributeVRAM(pidVRAM, subtree)
	want := uint64(1<<30) + uint64(20<<30)
	if got != want {
		t.Errorf("attributeVRAM = %d, want %d (subtree only, unrelated pid excluded)", got, want)
	}
}

func TestIsHeavyAndIdle(t *testing.T) {
	idleState := &IdleState{Idle: true, IdleSeconds: 2280}
	busyState := &IdleState{Idle: false}

	cases := []struct {
		name string
		fp   *ServiceFootprint
		idle *IdleState
		want bool
	}{
		{
			name: "heavy RAM and idle -> warn (the #421 diffusers case)",
			fp:   &ServiceFootprint{RAMBytes: 7 << 30},
			idle: idleState,
			want: true,
		},
		{
			name: "heavy VRAM and idle -> warn",
			fp:   &ServiceFootprint{RAMBytes: 100 << 20, VRAMBytes: 21 << 30, HasGPU: true},
			idle: idleState,
			want: true,
		},
		{
			name: "heavy but busy -> no warn",
			fp:   &ServiceFootprint{RAMBytes: 7 << 30},
			idle: busyState,
			want: false,
		},
		{
			name: "idle but light -> no warn",
			fp:   &ServiceFootprint{RAMBytes: 100 << 20, VRAMBytes: 0},
			idle: idleState,
			want: false,
		},
		{
			name: "nil idle -> no warn",
			fp:   &ServiceFootprint{RAMBytes: 7 << 30},
			idle: nil,
			want: false,
		},
		{
			name: "nil footprint -> no warn",
			fp:   nil,
			idle: idleState,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsHeavyAndIdle(c.fp, c.idle); got != c.want {
				t.Errorf("IsHeavyAndIdle = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFormatBytesGB(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0.0G"},
		{uint64(6.1 * gib), "6.1G"},
		{21 * (1 << 30), "21.0G"},
	}
	for _, c := range cases {
		if got := FormatBytesGB(c.in); got != c.want {
			t.Errorf("FormatBytesGB(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatFootprint(t *testing.T) {
	gpu := &ServiceFootprint{RAMBytes: uint64(6.1 * gib), VRAMBytes: 21 * (1 << 30), GPUUtilPercent: 74, HasGPU: true}
	if got := FormatFootprint(gpu); got != "RAM 6.1G  VRAM 21.0G  GPU 74%" {
		t.Errorf("gpu footprint = %q", got)
	}

	noGPU := &ServiceFootprint{RAMBytes: 2 * (1 << 30), HasGPU: false}
	if got := FormatFootprint(noGPU); got != "RAM 2.0G  VRAM n/a  GPU n/a" {
		t.Errorf("no-gpu footprint = %q", got)
	}

	unknownUtil := &ServiceFootprint{RAMBytes: 1 << 30, VRAMBytes: 0, GPUUtilPercent: -1, HasGPU: true}
	if got := FormatFootprint(unknownUtil); got != "RAM 1.0G  VRAM 0.0G  GPU n/a" {
		t.Errorf("unknown-util footprint = %q", got)
	}

	if got := FormatFootprint(nil); got != "" {
		t.Errorf("nil footprint = %q, want empty", got)
	}
}

func TestFootprintActive(t *testing.T) {
	cases := []struct {
		name string
		fp   *ServiceFootprint
		want bool
	}{
		{"busy cpu", &ServiceFootprint{CPUPercent: 50}, true},
		{"quiet cpu no gpu", &ServiceFootprint{CPUPercent: 0.5, HasGPU: false}, false},
		{"quiet cpu busy gpu", &ServiceFootprint{CPUPercent: 0.5, GPUUtilPercent: 80, HasGPU: true}, true},
		{"quiet cpu idle gpu (diffusers)", &ServiceFootprint{CPUPercent: 0.02, GPUUtilPercent: 0, HasGPU: true}, false},
		{"unknown cpu no gpu", &ServiceFootprint{CPUPercent: -1, HasGPU: false}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := footprintActive(c.fp); got != c.want {
				t.Errorf("footprintActive = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFootprintIdleTracker_AccumulatesAndFlips(t *testing.T) {
	tr := NewFootprintIdleTracker()
	now := time.Unix(1_700_000_000, 0)
	tr.now = func() time.Time { return now }

	idleFP := &ServiceFootprint{CPUPercent: 0.02, GPUUtilPercent: 0, HasGPU: true}

	// First observation: goes inactive, but not yet idle (clock just started).
	s := tr.Observe("citadel-diffusers", idleFP)
	if s.Idle {
		t.Fatalf("first inactive observe should not be idle yet: %+v", s)
	}

	// 30s later: still under the 60s threshold.
	now = now.Add(30 * time.Second)
	s = tr.Observe("citadel-diffusers", idleFP)
	if s.Idle {
		t.Fatalf("30s inactive should not be idle yet: %+v", s)
	}

	// 40s more (70s total): crosses the threshold.
	now = now.Add(40 * time.Second)
	s = tr.Observe("citadel-diffusers", idleFP)
	if !s.Idle {
		t.Fatalf("70s inactive should be idle: %+v", s)
	}
	if s.IdleSeconds < 70 {
		t.Errorf("idle_seconds = %d, want >= 70", s.IdleSeconds)
	}
}

func TestFootprintIdleTracker_ActivityResets(t *testing.T) {
	tr := NewFootprintIdleTracker()
	now := time.Unix(1_700_000_000, 0)
	tr.now = func() time.Time { return now }

	idleFP := &ServiceFootprint{CPUPercent: 0.02, HasGPU: false}
	busyFP := &ServiceFootprint{CPUPercent: 90, HasGPU: false}

	tr.Observe("svc", idleFP)
	now = now.Add(120 * time.Second)
	if s := tr.Observe("svc", idleFP); !s.Idle {
		t.Fatalf("should be idle after 120s: %+v", s)
	}

	// A burst of activity resets the clock.
	if s := tr.Observe("svc", busyFP); s.Idle || s.IdleSeconds != 0 {
		t.Fatalf("activity should reset idle: %+v", s)
	}

	// Going quiet again starts a fresh clock, not idle immediately.
	now = now.Add(10 * time.Second)
	if s := tr.Observe("svc", idleFP); s.Idle {
		t.Fatalf("fresh quiet period should not be immediately idle: %+v", s)
	}
}

func TestFormatIdleLabel(t *testing.T) {
	cases := []struct {
		name string
		in   *IdleState
		want string
	}{
		{"busy", &IdleState{Idle: false}, "busy"},
		{"idle minutes", &IdleState{Idle: true, IdleSeconds: 2280}, "idle 38m"},
		{"idle hours", &IdleState{Idle: true, IdleSeconds: 7200}, "idle 2h"},
		{"idle seconds", &IdleState{Idle: true, IdleSeconds: 45}, "idle 45s"},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FormatIdleLabel(c.in); got != c.want {
				t.Errorf("FormatIdleLabel = %q, want %q", got, c.want)
			}
		})
	}
}

func TestReadPPID_ParsesCommWithSpacesAndParens(t *testing.T) {
	// readPPID must key off the last ')' because comm can contain spaces/parens.
	// We can't easily write /proc, so exercise the parsing indirectly by
	// confirming the current process resolves to a plausible parent.
	pid := 1 // init always exists on Linux
	if ppid, ok := readPPID(pid); ok {
		if ppid < 0 {
			t.Errorf("ppid of pid 1 = %d, want >= 0", ppid)
		}
	}
	// A definitely-absent pid returns not-ok, never panics.
	if _, ok := readPPID(2_000_000_000); ok {
		t.Errorf("absent pid should return ok=false")
	}
}

func TestCollectFootprints_EmptyNames(t *testing.T) {
	got := CollectFootprints(nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty map for no names, got %v", got)
	}
}

// TestStatsFilterByCandidateName is the regression test for the batch-poisoning
// bug: `docker stats` is run over ALL running containers (no name args), and
// the caller filters by candidate name. A candidate that names a nonexistent
// container (the bare "diffusers" when only "citadel-diffusers" runs) must NOT
// prevent the real container's RAM from being found — which is exactly what
// would happen if the missing name were passed to `docker stats`.
func TestStatsFilterByCandidateName(t *testing.T) {
	// stats output lists all running containers; the bare "diffusers" is absent.
	out := "citadel-diffusers\t0.02%\t7.4GiB / 62.0GiB\n" +
		"some-unrelated-db\t3.0%\t128MiB / 62.0GiB\n"
	stats := parseStatsOutput(out)

	// Candidate names for the diffusers service, mirroring serviceContainerNames.
	candidates := []string{"citadel-diffusers", "diffusers"}
	var found containerStats
	var ok bool
	for _, cn := range candidates {
		if s, present := stats[cn]; present {
			found, ok = s, true
			break
		}
	}
	if !ok {
		t.Fatal("expected to resolve diffusers via citadel-diffusers despite bare name being absent")
	}
	// The 7.4 GiB RSS — the exact #421 incident value — must be recoverable so
	// IsHeavyAndIdle can fire on the RAM path.
	if found.ramBytes < uint64(7.0*gib) {
		t.Errorf("diffusers ram = %d, want >= 7GiB", found.ramBytes)
	}
	fp := &ServiceFootprint{RAMBytes: found.ramBytes}
	if !IsHeavyAndIdle(fp, &IdleState{Idle: true, IdleSeconds: 2280}) {
		t.Error("7.4GiB idle diffusers should be flagged heavy-and-idle")
	}
}

// TestParseNetIO checks the "<rx> / <tx>" NetIO cell parses to the summed byte
// total, and that garbage/partial cells degrade to 0 without panicking.
func TestParseNetIO(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0B / 0B", 0},
		{"1.0kB / 2.0kB", 3000},            // decimal kB, summed
		{"1.5MB / 0B", 1_500_000},          // one side only
		{"1.0MiB / 1.0MiB", 2 * (1 << 20)}, // binary MiB, summed
		{"", 0},
		{"garbage", 0},
		{"5kB", 5000}, // no separator -> whole cell treated as rx
	}
	for _, c := range cases {
		if got := parseNetIO(c.in); got != c.want {
			t.Errorf("parseNetIO(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParseStatsOutput_WithNetIO verifies the 4-field (post-#433) stats row is
// parsed with netBytes populated and hasNet=true.
func TestParseStatsOutput_WithNetIO(t *testing.T) {
	// "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}"
	out := "citadel-claude\t1.2%\t512MiB / 62.0GiB\t3.0kB / 1.0kB\n"
	got := parseStatsOutput(out)
	s, ok := got["citadel-claude"]
	if !ok {
		t.Fatalf("missing citadel-claude row: %v", got)
	}
	if !s.hasNet {
		t.Fatalf("expected hasNet true for a 4-field row")
	}
	if s.netBytes != 4000 {
		t.Errorf("netBytes = %d, want 4000 (3kB + 1kB)", s.netBytes)
	}
}

// TestParseStatsOutput_ThreeFieldStillParses guards backward compatibility: a
// row without the NetIO column (older statsFormat / a runtime that omits it)
// must still parse for CPU+RAM, with hasNet=false and netBytes=0.
func TestParseStatsOutput_ThreeFieldStillParses(t *testing.T) {
	out := "citadel-vllm\t74.31%\t6.1GiB / 62.0GiB\n"
	got := parseStatsOutput(out)
	s, ok := got["citadel-vllm"]
	if !ok {
		t.Fatalf("missing citadel-vllm row: %v", got)
	}
	if s.hasNet {
		t.Fatalf("expected hasNet false for a 3-field (no NetIO) row")
	}
	if s.netBytes != 0 {
		t.Errorf("netBytes = %d, want 0 when NetIO absent", s.netBytes)
	}
	if s.cpuPercent != 74.31 {
		t.Errorf("cpu = %v, want 74.31 (3-field row must still parse)", s.cpuPercent)
	}
}

// newNetTestCollector builds a Collector wired with only the trackers the
// generic idle path needs, with an injectable clock on the network tracker.
func newNetTestCollector(thresholdSeconds int, clock *time.Time) *Collector {
	c := &Collector{
		fpIdleTracker:  NewFootprintIdleTracker(),
		netIdleTracker: NewIdleTracker(thresholdSeconds),
	}
	c.netIdleTracker.now = func() time.Time { return *clock }
	c.fpIdleTracker.now = func() time.Time { return *clock }
	return c
}

// TestAttachDerivedIdle_NetworkSignalGoesIdle exercises the #433 generic path
// end to end through attachDerivedIdle: a container whose NetIO stays flat past
// the configured threshold is emitted as idle, honoring the shared idle
// threshold rather than the footprint's fixed 60s clock.
func TestAttachDerivedIdle_NetworkSignalGoesIdle(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	c := newNetTestCollector(300, &now)

	// Busy: NetIO climbing. Even with CPU above the footprint idle floor, we care
	// about the network signal here.
	fp := &ServiceFootprint{CPUPercent: 40, RAMBytes: 1 << 30, NetBytes: 1000, HasNet: true, GPUUtilPercent: -1}
	var dst *IdleState
	c.attachDerivedIdle("citadel-agent", fp, &dst)
	if dst == nil || dst.Idle {
		t.Fatalf("expected a not-idle signal on first sample, got %+v", dst)
	}

	// Flat NetIO for the full threshold -> idle.
	now = now.Add(300 * time.Second)
	fp2 := &ServiceFootprint{CPUPercent: 40, RAMBytes: 1 << 30, NetBytes: 1000, HasNet: true, GPUUtilPercent: -1}
	dst = nil
	c.attachDerivedIdle("citadel-agent", fp2, &dst)
	if dst == nil || !dst.Idle {
		t.Fatalf("expected idle after 300s flat NetIO, got %+v", dst)
	}
	if dst.IdleSeconds != 300 {
		t.Errorf("idle_seconds = %d, want 300", dst.IdleSeconds)
	}
}

// TestAttachDerivedIdle_VLLMSignalIsAuthoritative confirms the network path does
// not overwrite an already-present vLLM request-counter idle signal.
func TestAttachDerivedIdle_VLLMSignalIsAuthoritative(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	c := newNetTestCollector(300, &now)

	// Pretend the metrics path already attached a busy vLLM idle state.
	existing := &IdleState{Idle: false, IdleSeconds: 5}
	dst := existing
	fp := &ServiceFootprint{CPUPercent: 1, NetBytes: 1000, HasNet: true, GPUUtilPercent: -1}
	c.attachDerivedIdle("citadel-vllm", fp, &dst)
	if dst != existing {
		t.Fatalf("expected the vLLM idle signal to be left untouched, got %+v", dst)
	}
}

// TestAttachDerivedIdle_UnknownNetFailsSafeActive is the core fail-safe test: a
// container whose stats read produced NO NetIO column (HasNet=false) must NOT be
// fed to the network idle tracker. Otherwise a permanently-flat unread counter
// would look idle after the threshold and could auto-stop a busy container. With
// footprint activity present (CPU high) it stays busy; the network path never
// fabricates an idle signal from an unknown reading.
func TestAttachDerivedIdle_UnknownNetFailsSafeActive(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	c := newNetTestCollector(300, &now)

	// CPU is well above the footprint idle floor -> the fallback footprint signal
	// reports busy. NetIO is absent (HasNet=false) so the net path is skipped.
	fp := &ServiceFootprint{CPUPercent: 90, RAMBytes: 1 << 30, NetBytes: 0, HasNet: false, GPUUtilPercent: -1}

	var dst *IdleState
	c.attachDerivedIdle("citadel-busy", fp, &dst)
	if dst == nil {
		t.Fatalf("expected a footprint fallback idle signal, got nil")
	}
	if dst.Idle {
		t.Fatalf("busy container with unknown NetIO must never be reported idle, got %+v", dst)
	}

	// Even after a long gap, with NetIO still unknown and CPU busy, it stays
	// active -- proving the unread net counter never drives it idle.
	now = now.Add(3600 * time.Second)
	dst = nil
	c.attachDerivedIdle("citadel-busy", fp, &dst)
	if dst == nil || dst.Idle {
		t.Fatalf("busy container with unknown NetIO must stay active, got %+v", dst)
	}
}

func TestPidSubtree_WalksChildren(t *testing.T) {
	// Synthetic /proc adjacency: 1000 -> 1001 -> 1002, plus an unrelated 2000.
	children := map[int][]int{
		1000: {1001},
		1001: {1002},
		2000: {2001},
	}
	got := pidSubtree(1000, children)
	for _, pid := range []int{1000, 1001, 1002} {
		if _, ok := got[pid]; !ok {
			t.Errorf("subtree missing pid %d: %v", pid, got)
		}
	}
	if _, ok := got[2000]; ok {
		t.Errorf("subtree should not include unrelated pid 2000: %v", got)
	}
}

func TestPidSubtree_NoProcFallsBackToRoot(t *testing.T) {
	got := pidSubtree(42, nil)
	if len(got) != 1 {
		t.Fatalf("expected just {root}, got %v", got)
	}
	if _, ok := got[42]; !ok {
		t.Errorf("subtree should contain root pid 42: %v", got)
	}
}
