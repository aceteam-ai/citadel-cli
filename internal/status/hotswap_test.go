package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "") // no env -> compose default model

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	engines := c.collectInstalledEngines(map[string]struct{}{})

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
}

func TestResolveInstalledModel_EnvOverrideWins(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "vllm", "VLLM_MODEL=my-org/my-model\n# a comment\n")

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	engines := c.collectInstalledEngines(map[string]struct{}{})

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

func TestCollectInstalledEngines_SkipsAlreadyReported(t *testing.T) {
	dir := t.TempDir()
	writeInstalledEngine(t, dir, "bonsai", "")

	c := NewCollector(CollectorConfig{ConfigDir: dir, ModelHotswap: true})
	// bonsai already reported (running) => must not be duplicated as stopped.
	engines := c.collectInstalledEngines(map[string]struct{}{"bonsai": {}})
	for _, e := range engines {
		if e.Name == "bonsai" {
			t.Fatalf("bonsai should be skipped when already reported")
		}
	}
}

func TestCollectInstalledEngines_NoConfigDirReturnsNil(t *testing.T) {
	c := NewCollector(CollectorConfig{ModelHotswap: true}) // ConfigDir empty
	if got := c.collectInstalledEngines(map[string]struct{}{}); got != nil {
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

func TestModelHotswapEnabled_TruthyParsing(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "YES": true, "on": true, "On": true,
		"": false, "0": false, "false": false, "no": false, "off": false, "nope": false,
	}
	for v, want := range cases {
		t.Setenv("CITADEL_MODEL_HOTSWAP", v)
		if got := ModelHotswapEnabled(); got != want {
			t.Errorf("ModelHotswapEnabled(%q) = %v, want %v", v, got, want)
		}
	}
}
