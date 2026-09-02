package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubHotswapPreflightPass overrides the citadel-cli#683 image/weights
// preflight package vars to always pass, for tests that exercise other
// behavior of collectInstalledEngines and don't want a real docker daemon or
// a real ~/citadel-cache to matter. The disk-headroom clause needs no stub:
// callers pass a zero-value SystemMetrics{}, which diskHeadroomBlocked
// treats as "no signal" (DiskTotalGB<=0) and never blocks.
func stubHotswapPreflightPass(t *testing.T) {
	t.Helper()
	origImg, origWeights := engineImagePresentFn, engineWeightsPresentFn
	engineImagePresentFn = func(string) bool { return true }
	engineWeightsPresentFn = func(string) bool { return true }
	t.Cleanup(func() {
		engineImagePresentFn = origImg
		engineWeightsPresentFn = origWeights
	})
}

// writeInstalledEngine materializes an engine's compose (and optional env) under
// a temp config dir so collectInstalledEngines treats it as installed.
func writeInstalledEngine(t *testing.T, dir, name, envContents string) {
	t.Helper()
	svcDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, name+".yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if envContents != "" {
		if err := os.WriteFile(filepath.Join(svcDir, name+".env"), []byte(envContents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectInstalledEngines_AdvertisesStoppedWithDefaultModel(t *testing.T) {
	stubHotswapPreflightPass(t)
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "") // no env -> compose default model

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	engines := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})

	var bonsai *ServiceInfo
	for i := range engines {
		if engines[i].Name == "bonsai" {
			bonsai = &engines[i]
		}
	}
	if bonsai == nil {
		t.Fatalf("expected bonsai advertised as installed, got %+v", engines)
	}
	if bonsai.Status != ServiceStatusStopped {
		t.Errorf("status = %q, want stopped", bonsai.Status)
	}
	if bonsai.Resident == nil || *bonsai.Resident {
		t.Errorf("Resident = %v, want non-nil false", bonsai.Resident)
	}
	if len(bonsai.Models) != 1 || bonsai.Models[0] != "Bonsai-27B-Q1_0.gguf" {
		t.Errorf("Models = %v, want [Bonsai-27B-Q1_0.gguf]", bonsai.Models)
	}
	if bonsai.VRAMEstimateMB == 0 {
		t.Errorf("expected a non-zero VRAM estimate for bonsai")
	}
	// citadel-cli#684: the disk-only branch (compose materialized, container
	// NOT running) must classify as ReadinessDown with the exact reason the
	// issue names, additive on top of the unchanged Status/Health assertions
	// above. No live probe ran here (this is a filesystem read), so ProbedAt
	// stays nil.
	if bonsai.Readiness != ReadinessDown {
		t.Errorf("readiness = %q, want %q", bonsai.Readiness, ReadinessDown)
	}
	if bonsai.Reason != "installed_not_running" {
		t.Errorf("reason = %q, want %q", bonsai.Reason, "installed_not_running")
	}
	if bonsai.ProbedAt != nil {
		t.Errorf("probed_at = %v, want nil (no live probe ran)", bonsai.ProbedAt)
	}
	if bonsai.Health != HealthStatusUnknown {
		t.Errorf("health = %q, want %q (unchanged by #684)", bonsai.Health, HealthStatusUnknown)
	}
	// citadel-cli#683: with the preflight checks stubbed to pass, this entry
	// must be advertised exactly as it was before #683 -- no SwapBlocked flag.
	if bonsai.SwapBlocked {
		t.Errorf("SwapBlocked = true, want false when all preflight checks pass")
	}
	if bonsai.SwapBlockedReason != "" {
		t.Errorf("SwapBlockedReason = %q, want empty when all preflight checks pass", bonsai.SwapBlockedReason)
	}
}

func TestResolveInstalledModel_EnvOverrideWins(t *testing.T) {
	stubHotswapPreflightPass(t)
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "vllm", "VLLM_MODEL=my-org/my-model\n# a comment\n")

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	engines := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})

	var found bool
	for _, e := range engines {
		if e.Name == "vllm" {
			found = true
			if len(e.Models) != 1 || e.Models[0] != "my-org/my-model" {
				t.Errorf("vllm Models = %v, want [my-org/my-model]", e.Models)
			}
		}
	}
	if !found {
		t.Fatalf("expected vllm advertised from persisted VLLM_MODEL")
	}
}

// TestResolveInstalledModel_LlamaCppEnvOverrideResolves pins the citadel-cli#685
// §1a fix: llamacpp was entirely absent from engineModelEnvVars, so
// resolveInstalledModel("llamacpp") always returned "" and an
// installed-but-stopped llamacpp could never be advertised as a swap
// candidate — structurally, not intermittently, regardless of what was
// persisted to llamacpp.env. With LLAMACPP_MODEL now a recognized override
// var, a persisted value must resolve and make llamacpp swappable.
func TestResolveInstalledModel_LlamaCppEnvOverrideResolves(t *testing.T) {
	stubHotswapPreflightPass(t)
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "llamacpp", "LLAMACPP_MODEL=my-model.gguf\n")

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	engines := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})

	var found bool
	for _, e := range engines {
		if e.Name == "llamacpp" {
			found = true
			if len(e.Models) != 1 || e.Models[0] != "my-model.gguf" {
				t.Errorf("llamacpp Models = %v, want [my-model.gguf]", e.Models)
			}
			if e.Resident == nil || *e.Resident {
				t.Errorf("Resident = %v, want non-nil false", e.Resident)
			}
		}
	}
	if !found {
		t.Fatalf("expected llamacpp advertised as a swap candidate from persisted LLAMACPP_MODEL, got %+v", engines)
	}
}

// TestResolveInstalledModel_LlamaCppNoDefaultWithoutOverride pins the other
// half of the same fix: unlike bonsai/unlimited-ocr, llamacpp has no compose-
// level default model (services/compose/llamacpp.yml starts in router/
// deferred-load mode when LLAMACPP_MODEL is unset), so a stopped llamacpp with
// no persisted override must stay un-advertised rather than fabricate a model
// id the engine cannot actually serve.
func TestResolveInstalledModel_LlamaCppNoDefaultWithoutOverride(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "llamacpp", "") // no env override, no compose default

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	engines := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})

	for _, e := range engines {
		if e.Name == "llamacpp" {
			t.Fatalf("expected llamacpp NOT advertised absent a persisted model, got %+v", e)
		}
	}
}

func TestCollectInstalledEngines_SkipsAlreadyReported(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "")

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	// bonsai already reported (running) => must not be duplicated as stopped.
	engines := c.collectInstalledEngines(map[string]struct{}{"bonsai": {}}, map[string]struct{}{}, SystemMetrics{})
	for _, e := range engines {
		if e.Name == "bonsai" {
			t.Fatalf("bonsai should be skipped when already reported")
		}
	}
}

func TestCollectInstalledEngines_NoConfigDirReturnsNil(t *testing.T) {
	c := NewCollector(CollectorConfig{ModelHotswap: true}) // ConfigDir empty
	if got := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{}); got != nil {
		t.Fatalf("expected nil with no configDir, got %v", got)
	}
}

func TestApplyModelHotswap_MarksRunningEngineResident(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})

	st := &NodeStatus{Services: []ServiceInfo{
		{Name: "vllm", Type: ServiceTypeLLM, Status: ServiceStatusRunning},
		{Name: "postgres", Type: ServiceTypeDatabase, Status: ServiceStatusRunning},
	}}
	reported := map[string]struct{}{"vllm": {}, "postgres": {}}

	c.applyModelHotswap(st, reported)

	byName := map[string]ServiceInfo{}
	for _, s := range st.Services {
		byName[s.Name] = s
	}
	if r := byName["vllm"].Resident; r == nil || !*r {
		t.Errorf("vllm Resident = %v, want non-nil true", byName["vllm"].Resident)
	}
	if byName["vllm"].VRAMEstimateMB == 0 {
		t.Errorf("expected a VRAM estimate on the resident vllm engine")
	}
	// A non-engine service must NOT get a residency flag.
	if byName["postgres"].Resident != nil {
		t.Errorf("postgres Resident = %v, want nil (not a serving engine)", byName["postgres"].Resident)
	}
}

// TestApplyModelHotswap_ResidentEngineUsesMeasuredFootprintOverTable is the
// citadel-cli#689 regression: a RESIDENT engine with a live measured VRAM
// footprint must advertise that measurement, not the coarse provisioning
// estimate (engineVRAMEstimateMB) -- even when the measurement is far below
// the table (the live unlimited-ocr case the issue reports: ~14GB measured
// against a 20GB budget).
func TestApplyModelHotswap_ResidentEngineUsesMeasuredFootprintOverTable(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})

	const measuredBytes = 14 << 30 // ~14GB
	st := &NodeStatus{Services: []ServiceInfo{
		{
			Name:      "unlimited-ocr",
			Type:      ServiceTypeLLM,
			Status:    ServiceStatusRunning,
			Footprint: &ServiceFootprint{VRAMBytes: measuredBytes},
		},
	}}
	reported := map[string]struct{}{"unlimited-ocr": {}}

	c.applyModelHotswap(st, reported)

	got := st.Services[0].VRAMEstimateMB
	wantMB := int(measuredBytes / (1024 * 1024))
	if got != wantMB {
		t.Fatalf("VRAMEstimateMB = %d, want the measured %d (table estimate is %d)",
			got, wantMB, engineVRAMEstimateMB["unlimited-ocr"])
	}
	if got == engineVRAMEstimateMB["unlimited-ocr"] {
		t.Fatalf("VRAMEstimateMB equals the table estimate (%d); the measured footprint was ignored", got)
	}
}

// TestApplyModelHotswap_ResidentEngineFallsBackToTableWithoutFootprint is the
// other half of #689: a running engine with NO footprint signal (no GPU,
// attribution miss) must still advertise the table estimate rather than 0 --
// the pre-existing behavior this change must not regress.
func TestApplyModelHotswap_ResidentEngineFallsBackToTableWithoutFootprint(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})

	st := &NodeStatus{Services: []ServiceInfo{
		{Name: "unlimited-ocr", Type: ServiceTypeLLM, Status: ServiceStatusRunning},
	}}
	reported := map[string]struct{}{"unlimited-ocr": {}}

	c.applyModelHotswap(st, reported)

	if got, want := st.Services[0].VRAMEstimateMB, engineVRAMEstimateMB["unlimited-ocr"]; got != want {
		t.Fatalf("VRAMEstimateMB = %d, want the table estimate %d (no footprint signal available)", got, want)
	}
}

// TestCollectInstalledEngines_StoppedEngineUsesTableEstimate pins the "cannot
// measure a model that isn't loaded" half of #689: a STOPPED (not resident)
// installed engine has no footprint to measure, so it must advertise the
// table estimate, unchanged by the measured-vram routing added for running
// engines.
func TestCollectInstalledEngines_StoppedEngineUsesTableEstimate(t *testing.T) {
	stubHotswapPreflightPass(t)
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "")
	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})

	got := c.collectInstalledEngines(map[string]struct{}{}, map[string]struct{}{}, SystemMetrics{})
	if len(got) != 1 {
		t.Fatalf("expected exactly one advertised stopped engine, got %d", len(got))
	}
	if want := engineVRAMEstimateMB["bonsai"]; got[0].VRAMEstimateMB != want {
		t.Fatalf("VRAMEstimateMB = %d, want the table estimate %d", got[0].VRAMEstimateMB, want)
	}
}

// TestServiceInfo_FlagOffOmitsResidentKey asserts the omitempty on the *bool and
// int keeps a flag-off heartbeat byte-identical: no `resident`/`vram_estimate_mb`.
func TestServiceInfo_FlagOffOmitsResidentKey(t *testing.T) {
	b, err := json.Marshal(ServiceInfo{Name: "vllm", Type: ServiceTypeLLM, Status: ServiceStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "resident") {
		t.Errorf("flag-off JSON must not contain a resident key: %s", s)
	}
	if strings.Contains(s, "vram_estimate_mb") {
		t.Errorf("flag-off JSON must not contain vram_estimate_mb: %s", s)
	}
}

// TestModelHotswapEnabled_DefaultOnDisableParsing verifies hotswap is ON by
// default (unset, empty, or any non-falsey value) and only an explicit falsey
// CITADEL_MODEL_HOTSWAP acts as the break-glass disable. A garbage value ("nope")
// stays ON so a typo can't silently kill the feature.
func TestModelHotswapEnabled_DefaultOnDisableParsing(t *testing.T) {
	cases := map[string]bool{
		// default ON: unset/empty, truthy tokens, and unknown/garbage values.
		"": true, "1": true, "true": true, "YES": true, "on": true, "On": true, "nope": true,
		// break-glass disable: explicit falsey tokens only.
		"0": false, "false": false, "no": false, "off": false, "OFF": false,
	}
	for v, want := range cases {
		t.Setenv("CITADEL_MODEL_HOTSWAP", v)
		if got := ModelHotswapEnabled(); got != want {
			t.Errorf("ModelHotswapEnabled(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestDefaultServeTier pins the citadel-cli#628 VRAM->(engine,model) tier
// boundaries exactly, including the edges (5, 6, 12, 20 GB in MB) and the
// unknown/non-positive "no tier" case.
func TestDefaultServeTier(t *testing.T) {
	cases := []struct {
		name       string
		vramMB     int
		wantEngine string
		wantModel  string
	}{
		{"zero is no tier", 0, "", ""},
		{"negative is no tier", -1, "", ""},
		{"just under 6GB", 6143, "ollama", "llama3.2:3b"},
		{"well under 6GB", 4096, "ollama", "llama3.2:3b"},
		{"exactly 6GB starts next tier", 6144, "ollama", "llama3.1:8b"},
		{"just under 12GB", 12287, "ollama", "llama3.1:8b"},
		{"exactly 12GB starts next tier", 12288, "ollama", "qwen2.5:14b"},
		{"just under 20GB", 20479, "ollama", "qwen2.5:14b"},
		{"exactly 20GB starts vllm tier", 20480, "vllm", ""},
		{"well over 20GB", 24576, "vllm", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, model := DefaultServeTier(tc.vramMB)
			if engine != tc.wantEngine || model != tc.wantModel {
				t.Errorf("DefaultServeTier(%d) = (%q, %q), want (%q, %q)", tc.vramMB, engine, model, tc.wantEngine, tc.wantModel)
			}
		})
	}
}

// TestLargestGPUTotalVRAMMB verifies the multi-GPU max-not-sum contract and
// the "no GPU with known total VRAM" (found=false) case.
func TestLargestGPUTotalVRAMMB(t *testing.T) {
	if mb, found := LargestGPUTotalVRAMMB(nil); found || mb != 0 {
		t.Errorf("empty gpus: got (%d, %v), want (0, false)", mb, found)
	}
	if mb, found := LargestGPUTotalVRAMMB([]GPUMetrics{{MemoryTotalMB: 0}}); found || mb != 0 {
		t.Errorf("single zero-total gpu: got (%d, %v), want (0, false)", mb, found)
	}
	gpus := []GPUMetrics{
		{Index: 0, MemoryTotalMB: 8192},
		{Index: 1, MemoryTotalMB: 24576},
		{Index: 2, MemoryTotalMB: 0}, // unknown -- must not win as "largest"
	}
	mb, found := LargestGPUTotalVRAMMB(gpus)
	if !found || mb != 24576 {
		t.Errorf("multi-gpu max: got (%d, %v), want (24576, true) -- must be the max, not the sum (32768)", mb, found)
	}
}
