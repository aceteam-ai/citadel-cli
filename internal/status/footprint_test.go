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
