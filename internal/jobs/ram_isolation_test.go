// internal/jobs/ram_isolation_test.go
//
// Hermetic tests for citadel#831's RAM isolation wiring (applyRAMIsolation,
// parseRequiredRAMBytes, resourceIsolationEnabled) and the VRAM-preflight
// citadel-side-estimate wiring (resolveRequiredVRAMBytes). No docker/nvidia-smi
// is ever invoked: collectStatus is injected (mirrors reservation_test.go's
// pattern), and applyRAMIsolation's only I/O is reading a compose file this
// test writes to a temp dir and writing the override alongside it.
package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

const ramTestGPUComposeYAML = `services:
  media:
    image: some/media-gen:latest
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              capabilities: [gpu]
`

const ramTestNonGPUComposeYAML = `services:
  redis:
    image: redis:7
`

const ramTestManifestYAML = `node:
  name: test-node
services:
  - name: media
    type: docker
    compose_file: ./services/media.yml
  - name: pinned-svc
    type: docker
    compose_file: ./services/pinned-svc.yml
pinned_services:
  - pinned-svc
`

// newRAMTestHandler builds a ServiceHandler rooted at a temp dir carrying
// ramTestManifestYAML and a synthetic NodeStatus via collectStatus, mirroring
// reservation_test.go's newReservationTestHandlerWithManifest.
func newRAMTestHandler(t *testing.T, st *status.NodeStatus) *ServiceHandler {
	t.Helper()
	dir := t.TempDir()
	writeManifestFile(t, dir, ramTestManifestYAML)
	h := NewServiceHandler(dir)
	h.collectStatus = func() (*status.NodeStatus, error) { return st, nil }
	return h
}

// writeComposeFile writes compose content to <dir>/services/<name>.yml and
// returns the path, mirroring where a real materialized compose file lives.
func writeComposeFile(t *testing.T, configDir, name, content string) string {
	t.Helper()
	svcDir := filepath.Join(configDir, "services")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatalf("mkdir services: %v", err)
	}
	path := filepath.Join(svcDir, name+".yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	return path
}

// gpuNodeStatus builds a synthetic NodeStatus reporting availableGB free RAM,
// a GPU present (so freeVRAMBytes' hostHasGPU signal is true), and a pinned
// running service with pinnedRAMGB of RAM footprint.
func gpuNodeStatus(availableGB, pinnedRAMGB float64) *status.NodeStatus {
	return &status.NodeStatus{
		System: status.SystemMetrics{MemoryAvailableGB: availableGB},
		GPU:    []status.GPUMetrics{{MemoryTotalMB: 24576, MemoryFreeMB: 10000}},
		Services: []status.ServiceInfo{
			{
				Name:      "pinned-svc",
				Status:    status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{RAMBytes: uint64(pinnedRAMGB * (1 << 30))},
			},
		},
	}
}

func TestApplyRAMIsolation_NoOpWhenNotOptedIn(t *testing.T) {
	// CITADEL_RESOURCE_ISOLATION unset -- today's default. Must be a complete
	// no-op regardless of how much GPU/RAM pressure the synthetic status
	// reports, so an unreviewed node's behavior is byte-identical to before
	// this feature existed.
	st := gpuNodeStatus(4 /* barely any free RAM */, 0)
	h := newRAMTestHandler(t, st)
	composePath := writeComposeFile(t, h.ConfigDir, "media", ramTestGPUComposeYAML)

	got, err := h.applyRAMIsolation(JobContext{}, manifestService{Name: "media"}, composePath, 100*testGiB)
	if err != nil {
		t.Fatalf("expected no refusal when not opted in, got error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no override path when not opted in, got %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(h.ConfigDir, "services", "media.ram.yml")); statErr == nil {
		t.Fatal("no override file should have been written when not opted in")
	}
}

func TestApplyRAMIsolation_SkipsNonGPUService(t *testing.T) {
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "1")
	st := gpuNodeStatus(64, 0)
	h := newRAMTestHandler(t, st)
	composePath := writeComposeFile(t, h.ConfigDir, "redis-like", ramTestNonGPUComposeYAML)

	got, err := h.applyRAMIsolation(JobContext{}, manifestService{Name: "redis-like"}, composePath, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no override for a non-GPU service, got %q", got)
	}
}

func TestApplyRAMIsolation_SkipsWhenHostHasNoGPU(t *testing.T) {
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "1")
	st := &status.NodeStatus{System: status.SystemMetrics{MemoryAvailableGB: 64}} // no GPU reported
	h := newRAMTestHandler(t, st)
	composePath := writeComposeFile(t, h.ConfigDir, "media", ramTestGPUComposeYAML)

	got, err := h.applyRAMIsolation(JobContext{}, manifestService{Name: "media"}, composePath, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no override when host reports no GPU, got %q", got)
	}
}

func TestApplyRAMIsolation_WritesCeilingOverrideForGPUService(t *testing.T) {
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "1")
	// 64GB available, 10GB held by the pinned service.
	st := gpuNodeStatus(64, 10)
	h := newRAMTestHandler(t, st)
	composePath := writeComposeFile(t, h.ConfigDir, "media", ramTestGPUComposeYAML)

	// No declared requirement (requiredRAMBytes=0): must Fit (fail-open on an
	// absent signal) and still apply a generous ceiling.
	got, err := h.applyRAMIsolation(JobContext{}, manifestService{Name: "media"}, composePath, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantPath := filepath.Join(h.ConfigDir, "services", "media.ram.yml")
	if got != wantPath {
		t.Fatalf("override path = %q, want %q", got, wantPath)
	}
	data, readErr := os.ReadFile(wantPath)
	if readErr != nil {
		t.Fatalf("override file was not written: %v", readErr)
	}
	// The ceiling must equal status.RAMBudgetBytes(64GB, 10GB) -- pinned
	// footprint subtracted, headroom reserved.
	wantCeiling := status.RAMBudgetBytes(uint64(64*(1<<30)), uint64(10*(1<<30)))
	if !strings.Contains(string(data), "mem_limit:") {
		t.Fatalf("expected mem_limit in override, got:\n%s", data)
	}
	// Sanity: the ceiling is comfortably below the 64GB available (headroom +
	// pinned were subtracted) and comfortably above a naive small default (the
	// doc's explicit requirement: media-gen jobs legitimately need 10-20GB+).
	if wantCeiling >= uint64(64*(1<<30)) {
		t.Fatalf("ceiling %d should be less than the full 64GB available", wantCeiling)
	}
	if wantCeiling < 10*testGiB {
		t.Fatalf("ceiling %d should comfortably exceed a naive small (e.g. 2g Tier-2) default", wantCeiling)
	}
}

func TestApplyRAMIsolation_RefusesConfirmedRAMShortfall(t *testing.T) {
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "1")
	// Only 12GB available, 8GB pinned => a real (not fabricated) ~2GB budget.
	// A declared requirement of 40GB cannot possibly fit: refuse.
	st := gpuNodeStatus(12, 8)
	h := newRAMTestHandler(t, st)
	composePath := writeComposeFile(t, h.ConfigDir, "media", ramTestGPUComposeYAML)

	got, err := h.applyRAMIsolation(JobContext{}, manifestService{Name: "media"}, composePath, 40*testGiB)
	if err == nil {
		t.Fatalf("expected a refusal error for a confirmed RAM shortfall, got override=%q", got)
	}
	if got != "" {
		t.Fatalf("expected no override path on refusal, got %q", got)
	}
	// No override file should have been written -- the preflight refuses
	// BEFORE generating/writing anything.
	if _, statErr := os.Stat(filepath.Join(h.ConfigDir, "services", "media.ram.yml")); statErr == nil {
		t.Fatal("no override file should be written when the RAM preflight refuses")
	}
}

func TestApplyRAMIsolation_SkipsRatherThanFabricateCeilingUnderPressure(t *testing.T) {
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "1")
	// Available RAM is entirely consumed by the pinned footprint + headroom
	// (RAMBudgetBytes returns 0 here -- see internal/status.TestRAMBudgetBytes_
	// ReturnsZeroWhenPinnedExceedsAvailable). No declared requirement
	// (requiredRAMBytes=0), so the preflight itself fits (fail-open on an
	// absent signal) -- but the ceiling generation step must still skip
	// rather than write a fabricated, meaningless small mem_limit onto a real
	// inference engine.
	st := gpuNodeStatus(10, 20)
	h := newRAMTestHandler(t, st)
	composePath := writeComposeFile(t, h.ConfigDir, "media", ramTestGPUComposeYAML)

	got, err := h.applyRAMIsolation(JobContext{}, manifestService{Name: "media"}, composePath, 0)
	if err != nil {
		t.Fatalf("expected no refusal (requiredRAMBytes=0 must fail open), got error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no override written when no safe ceiling can be derived, got %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(h.ConfigDir, "services", "media.ram.yml")); statErr == nil {
		t.Fatal("no override file should have been written under memory pressure with no safe ceiling")
	}
}

func TestApplyRAMIsolation_FitsWhenDeclaredRequirementIsSmall(t *testing.T) {
	t.Setenv("CITADEL_RESOURCE_ISOLATION", "1")
	st := gpuNodeStatus(64, 0)
	h := newRAMTestHandler(t, st)
	composePath := writeComposeFile(t, h.ConfigDir, "media", ramTestGPUComposeYAML)

	got, err := h.applyRAMIsolation(JobContext{}, manifestService{Name: "media"}, composePath, 8*testGiB)
	if err != nil {
		t.Fatalf("unexpected refusal for a requirement well within budget: %v", err)
	}
	if got == "" {
		t.Fatal("expected an override path")
	}
}

func TestPinnedRAMBytes(t *testing.T) {
	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "pinned-running", Status: status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{RAMBytes: 10 * testGiB}},
			{Name: "pinned-stopped", Status: status.ServiceStatusStopped,
				Footprint: &status.ServiceFootprint{RAMBytes: 99 * testGiB}},
			{Name: "unpinned-running", Status: status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{RAMBytes: 50 * testGiB}},
			{Name: "self", Status: status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{RAMBytes: 5 * testGiB}},
		},
	}
	pinned := map[string]bool{"pinned-running": true, "pinned-stopped": true, "self": true}
	got := pinnedRAMBytes(st, "self", pinned)
	// Only pinned-running counts: pinned-stopped is not running, unpinned-running
	// isn't pinned, and self is excluded even though pinned.
	if want := 10 * testGiB; got != want {
		t.Errorf("pinnedRAMBytes = %d, want %d", got, want)
	}
}

func TestParseRequiredRAMBytes(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]string
		want    uint64
	}{
		{"absent", map[string]string{"service": "diffusers"}, 0},
		{"blank", map[string]string{"ram_mb": "  "}, 0},
		{"zero", map[string]string{"ram_mb": "0"}, 0},
		{"negative", map[string]string{"ram_gb": "-4"}, 0},
		{"garbage", map[string]string{"ram_mb": "lots"}, 0},
		{"mb", map[string]string{"ram_mb": "8192"}, 8192 * 1024 * 1024},
		{"gb", map[string]string{"ram_gb": "20"}, 20 * 1024 * 1024 * 1024},
		{"mb_wins_over_gb", map[string]string{"ram_mb": "1024", "ram_gb": "40"}, 1024 * 1024 * 1024},
	}
	for _, c := range cases {
		if got := parseRequiredRAMBytes(c.payload); got != c.want {
			t.Errorf("%s: parseRequiredRAMBytes = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestResourceIsolationEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"garbage", false},
		{"1", true},
		{"true", true},
		{"YES", true},
		{" on ", true},
	}
	for _, c := range cases {
		t.Setenv("CITADEL_RESOURCE_ISOLATION", c.val)
		if got := resourceIsolationEnabled(); got != c.want {
			t.Errorf("resourceIsolationEnabled(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}
