package resmon

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// gib is a non-constant GiB multiplier so fractional byte literals compile as
// uint64 conversions (a constant float with a fractional value cannot convert
// to uint64 in a constant expression). Mirrors internal/status/footprint_test.go.
var gib = float64(1 << 30)

func TestParseComputeApps(t *testing.T) {
	// nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader,nounits
	// used_memory is in MiB.
	out := "12345, 21504\n12346, 512\n67890, 0\n"
	got := parseComputeApps(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 pids, got %d: %v", len(got), got)
	}
	if want := uint64(21504) * (1 << 20); got[12345] != want {
		t.Errorf("pid 12345 vram = %d, want %d", got[12345], want)
	}
	if got[67890] != 0 {
		t.Errorf("pid 67890 vram = %d, want 0", got[67890])
	}
}

func TestParseComputeApps_MultiGPUSamePIDSums(t *testing.T) {
	got := parseComputeApps("999, 1024\n999, 2048\n")
	if want := uint64(1024+2048) * (1 << 20); got[999] != want {
		t.Errorf("pid 999 summed vram = %d, want %d", got[999], want)
	}
}

func TestParseGPUTotals(t *testing.T) {
	// memory.used, memory.total, utilization.gpu — MiB, MiB, %.
	// Two devices: totals sum, util is the max.
	out := "8192, 24576, 12\n1024, 24576, 47\n"
	used, total, util := parseGPUTotals(out)
	if want := uint64(8192+1024) * (1 << 20); used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
	if want := uint64(24576+24576) * (1 << 20); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if util != 47 {
		t.Errorf("util = %v, want 47 (max across devices)", util)
	}
}

func TestParseGPUTotals_Empty(t *testing.T) {
	used, total, util := parseGPUTotals("")
	if used != 0 || total != 0 {
		t.Errorf("empty totals = %d/%d, want 0/0", used, total)
	}
	if util != -1 {
		t.Errorf("empty util = %v, want -1", util)
	}
}

func TestParsePsOutput(t *testing.T) {
	out := "abc123def4560000000000000000000000000000000000000000000000000000\ttei-gte\n" +
		"fff000\tcileadel\n"
	got := parsePsOutput(out)
	full := "abc123def4560000000000000000000000000000000000000000000000000000"
	if got[full] != "tei-gte" {
		t.Errorf("full id → %q, want tei-gte", got[full])
	}
	// 12-char short id is also indexed.
	if got["abc123def456"] != "tei-gte" {
		t.Errorf("short id → %q, want tei-gte", got["abc123def456"])
	}
}

func TestContainerIDFromCgroup(t *testing.T) {
	full := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	cases := []struct {
		name   string
		cgroup string
		want   string
	}{
		{"docker scope", "0::/system.slice/docker-" + full + ".scope\n", full},
		{"docker legacy", "12:cpuset:/docker/" + full + "\n", full},
		{"podman libpod", "0::/machine.slice/libpod-" + full + ".scope\n", full},
		{"host no id", "0::/user.slice/user-1000.slice/session-2.scope\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containerIDFromCgroup(c.cgroup); got != c.want {
				t.Errorf("containerIDFromCgroup = %q, want %q", got, c.want)
			}
		})
	}
}

func TestClassifyOwner(t *testing.T) {
	full := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	dockerCgroup := "0::/system.slice/docker-" + full + ".scope\n"
	hostCgroup := "0::/user.slice/user-1000.slice/session-2.scope\n"

	managedFull := "aaaa567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	managedCgroup := "0::/system.slice/docker-" + managedFull + ".scope\n"

	idToName := map[string]string{
		full:        "tei-gte",
		managedFull: "citadel-vllm",
	}

	t.Run("unmanaged container", func(t *testing.T) {
		owner, kind := classifyOwner(dockerCgroup, idToName, "python", nil)
		if kind != OwnerContainer {
			t.Errorf("kind = %v, want container", kind)
		}
		if owner != "container:tei-gte" {
			t.Errorf("owner = %q, want container:tei-gte", owner)
		}
	})

	t.Run("citadel managed", func(t *testing.T) {
		owner, kind := classifyOwner(managedCgroup, idToName, "python", nil)
		if kind != OwnerCitadelManaged {
			t.Errorf("kind = %v, want citadel-managed", kind)
		}
		if owner != "citadel-managed" {
			t.Errorf("owner = %q, want citadel-managed", owner)
		}
	})

	t.Run("host process", func(t *testing.T) {
		owner, kind := classifyOwner(hostCgroup, idToName, "ollama", nil)
		if kind != OwnerHost {
			t.Errorf("kind = %v, want host", kind)
		}
		if owner != "host:ollama" {
			t.Errorf("owner = %q, want host:ollama", owner)
		}
	})

	t.Run("host with unknown comm", func(t *testing.T) {
		owner, _ := classifyOwner(hostCgroup, idToName, "", nil)
		if owner != "host:unknown" {
			t.Errorf("owner = %q, want host:unknown", owner)
		}
	})

	t.Run("unresolvable container id stays container", func(t *testing.T) {
		other := "deadbeef0000cafef00d1234567890abcdef1234567890abcdef1234567890ab"
		cg := "0::/system.slice/docker-" + other + ".scope\n"
		owner, kind := classifyOwner(cg, idToName, "python", nil)
		if kind != OwnerContainer {
			t.Errorf("kind = %v, want container (unresolvable id is still a container, not host)", kind)
		}
		if owner != "container:deadbeef0000" {
			t.Errorf("owner = %q, want container:deadbeef0000", owner)
		}
	})

	t.Run("manifest-declared bare name is managed", func(t *testing.T) {
		// A managed service running under its bare name (no "citadel-" prefix)
		// must still classify as managed when the manifest lists it, so it is
		// never mis-flagged reclaimable.
		bareFull := "bbbb567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
		names := map[string]string{bareFull: "diffusers"}
		cg := "0::/system.slice/docker-" + bareFull + ".scope\n"
		managed := map[string]struct{}{"diffusers": {}}
		_, kind := classifyOwner(cg, names, "python", managed)
		if kind != OwnerCitadelManaged {
			t.Errorf("kind = %v, want citadel-managed (manifest-declared bare name)", kind)
		}
		// Without the manifest set, the same bare name is an unmanaged container.
		_, kind = classifyOwner(cg, names, "python", nil)
		if kind != OwnerContainer {
			t.Errorf("kind = %v, want container when manifest names absent", kind)
		}
	})
}

func TestParseStatmRSS(t *testing.T) {
	// statm fields: size resident shared text lib data dt (in pages).
	statm := "100000 250000 5000 10 0 90000 0\n"
	got := parseStatmRSS(statm)
	want := uint64(250000) * pageSize
	if got != want {
		t.Errorf("RSS = %d, want %d", got, want)
	}
	if parseStatmRSS("") != 0 {
		t.Error("empty statm should yield 0")
	}
	if parseStatmRSS("onlyonefield") != 0 {
		t.Error("single-field statm should yield 0")
	}
}

func TestReclaimable(t *testing.T) {
	heavyVRAM := uint64(3 * gib)
	heavyRSS := uint64(5 * gib)
	light := uint64(100 * 1024 * 1024)

	t.Run("unmanaged heavy VRAM + idle => reclaimable", func(t *testing.T) {
		reason, ok := reclaimable(heavyVRAM, light, true, OwnerContainer)
		if !ok {
			t.Fatal("expected reclaimable=true")
		}
		if reason == "" {
			t.Error("expected a non-empty reason")
		}
	})

	t.Run("unmanaged heavy RSS + idle => reclaimable", func(t *testing.T) {
		_, ok := reclaimable(light, heavyRSS, true, OwnerHost)
		if !ok {
			t.Error("expected reclaimable=true for heavy-RSS idle host process")
		}
	})

	t.Run("managed heavy + idle => NOT reclaimable (issue #421 owns it)", func(t *testing.T) {
		_, ok := reclaimable(heavyVRAM, heavyRSS, true, OwnerCitadelManaged)
		if ok {
			t.Error("managed consumers must never be reclaimable here")
		}
	})

	t.Run("unmanaged heavy + ACTIVE => NOT reclaimable", func(t *testing.T) {
		_, ok := reclaimable(heavyVRAM, heavyRSS, false, OwnerContainer)
		if ok {
			t.Error("active consumers must not be reclaimable")
		}
	})

	t.Run("unmanaged light + idle => NOT reclaimable", func(t *testing.T) {
		_, ok := reclaimable(light, light, true, OwnerContainer)
		if ok {
			t.Error("light consumers must not be reclaimable")
		}
	})
}

// fakeProbe is an in-memory probe for collectWith so the snapshot path is tested
// with zero real GPU/proc dependency.
type fakeProbe struct {
	pidVRAM  map[int]uint64
	used     uint64
	total    uint64
	util     float64
	hasGPU   bool
	idToName map[string]string
	cgroups  map[int]string
	comms    map[int]string
	rss      map[int]uint64
}

func (f fakeProbe) GPU(context.Context) (map[int]uint64, uint64, uint64, float64, bool) {
	return f.pidVRAM, f.used, f.total, f.util, f.hasGPU
}
func (f fakeProbe) Containers(context.Context) map[string]string { return f.idToName }
func (f fakeProbe) Cgroup(pid int) string                        { return f.cgroups[pid] }
func (f fakeProbe) Comm(pid int) string                          { return f.comms[pid] }
func (f fakeProbe) RSS(pid int) uint64                           { return f.rss[pid] }

func TestCollectWith_SnapshotShape(t *testing.T) {
	full := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	managedFull := "aaaa567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

	p := fakeProbe{
		pidVRAM:  map[int]uint64{101: uint64(3 * gib), 202: uint64(1 * gib)},
		used:     uint64(4 * gib),
		total:    uint64(24 * gib),
		util:     3, // below GPU idle threshold → consumers can be idle
		hasGPU:   true,
		idToName: map[string]string{full: "tei-gte", managedFull: "citadel-vllm"},
		cgroups: map[int]string{
			101: "0::/system.slice/docker-" + full + ".scope\n",
			202: "0::/system.slice/docker-" + managedFull + ".scope\n",
		},
		comms: map[int]string{},
		rss:   map[int]uint64{101: uint64(1 * gib), 202: uint64(6 * gib)},
	}

	// Idle tracker whose clock is already past the threshold, so a consumer seen
	// as inactive is immediately idle (no need to wait a real minute).
	tracker := newIdleTracker()
	base := time.Now()
	// Prime both pids as inactive well in the past.
	tracker.inactiveSince[101] = base.Add(-2 * time.Minute)
	tracker.inactiveSince[202] = base.Add(-2 * time.Minute)
	tracker.now = func() time.Time { return base }

	snap := collectWith(context.Background(), p, tracker, nil)

	if !snap.HasGPU {
		t.Fatal("expected HasGPU=true")
	}
	if snap.GPU.UsedBytes != uint64(4*gib) || snap.GPU.TotalBytes != uint64(24*gib) {
		t.Errorf("GPU totals = %d/%d", snap.GPU.UsedBytes, snap.GPU.TotalBytes)
	}
	if len(snap.Consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %d", len(snap.Consumers))
	}

	byPID := map[int]Consumer{}
	for _, c := range snap.Consumers {
		byPID[c.PID] = c
	}

	// Unmanaged tei-gte with 3G VRAM, idle → reclaimable.
	unmanaged := byPID[101]
	if unmanaged.Kind != OwnerContainer {
		t.Errorf("pid 101 kind = %v, want container", unmanaged.Kind)
	}
	if !unmanaged.Reclaimable {
		t.Error("pid 101 (unmanaged, heavy VRAM, idle) should be reclaimable")
	}

	// Managed citadel-vllm with heavy RSS, idle → NOT reclaimable.
	managed := byPID[202]
	if managed.Kind != OwnerCitadelManaged {
		t.Errorf("pid 202 kind = %v, want citadel-managed", managed.Kind)
	}
	if managed.Reclaimable {
		t.Error("pid 202 (managed) must never be reclaimable")
	}

	// JSON shape: round-trips and carries the documented keys.
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"has_gpu", "gpu", "gpu_util", "consumers", "collected_at"} {
		if _, ok := round[key]; !ok {
			t.Errorf("snapshot JSON missing key %q; got keys %v", key, round)
		}
	}
	gpu, _ := round["gpu"].(map[string]any)
	if _, ok := gpu["used"]; !ok {
		t.Error("gpu object missing 'used' key")
	}
	if _, ok := gpu["total"]; !ok {
		t.Error("gpu object missing 'total' key")
	}
}

func TestCollectWith_NoGPU(t *testing.T) {
	snap := collectWith(context.Background(), fakeProbe{hasGPU: false}, newIdleTracker(), nil)
	if snap.HasGPU {
		t.Error("expected HasGPU=false")
	}
	if len(snap.Consumers) != 0 {
		t.Errorf("expected no consumers, got %d", len(snap.Consumers))
	}
	if snap.GPUUtil != -1 {
		t.Errorf("GPUUtil = %v, want -1 when no GPU", snap.GPUUtil)
	}
	// Still serializes cleanly.
	if _, err := json.Marshal(snap); err != nil {
		t.Fatalf("marshal no-GPU snapshot: %v", err)
	}
}

func TestIdleTracker_SinglePollNotIdle(t *testing.T) {
	tr := newIdleTracker()
	base := time.Now()
	tr.now = func() time.Time { return base }
	// First observation of an inactive pid: sets the clock, reports not-idle.
	if tr.observe(1, -1, 0, true) {
		t.Error("first observation of inactive pid should report idle=false")
	}
	// After the threshold elapses, a second observation flips idle=true.
	tr.now = func() time.Time { return base.Add(idleThreshold + time.Second) }
	if !tr.observe(1, -1, 0, true) {
		t.Error("inactive pid past threshold should report idle=true")
	}
}

func TestIdleTracker_ActiveResets(t *testing.T) {
	tr := newIdleTracker()
	base := time.Now()
	tr.now = func() time.Time { return base }
	tr.observe(1, -1, 0, true) // inactive, clock starts
	// A burst of activity (high GPU util) resets the clock.
	if tr.observe(1, -1, 99, true) {
		t.Error("active pid should report idle=false")
	}
	// Even long after, since the clock reset, it must re-accumulate from zero.
	tr.now = func() time.Time { return base.Add(idleThreshold + time.Second) }
	if tr.observe(1, -1, 0, true) {
		t.Error("pid should not be idle immediately after activity reset")
	}
}
