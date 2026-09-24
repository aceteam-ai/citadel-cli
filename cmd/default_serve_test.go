package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// TestResolveDefaultServe pins the citadel-cli#628 opt-in precedence: env var
// > manifest default_serve key > persisted APPLY_DEVICE_CONFIG value > off.
func TestResolveDefaultServe(t *testing.T) {
	writePersisted := func(t *testing.T, enabled bool) {
		t.Helper()
		if err := config.SaveDefaultServe(platform.ConfigDir(), &config.DefaultServe{Enabled: enabled}); err != nil {
			t.Fatalf("SaveDefaultServe: %v", err)
		}
	}

	cases := []struct {
		name      string
		env       string // "" means unset
		manifest  bool
		persisted *bool // nil means no persisted file
		expected  bool
	}{
		{name: "default off when nothing set", expected: false},
		{name: "manifest key enables", manifest: true, expected: true},
		{name: "persisted value enables", persisted: boolPtr(true), expected: true},
		{name: "persisted value stays off", persisted: boolPtr(false), expected: false},
		{name: "env true overrides everything off", env: "true", expected: true},
		{name: "env off overrides manifest and persisted on", env: "off", manifest: true, persisted: boolPtr(true), expected: false},
		{name: "manifest wins over persisted off", manifest: true, persisted: boolPtr(false), expected: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // isolate platform.ConfigDir()
			t.Setenv("CITADEL_DEFAULT_SERVE", tc.env)
			if tc.persisted != nil {
				writePersisted(t, *tc.persisted)
			}
			manifest := &CitadelManifest{DefaultServe: tc.manifest}
			if got := resolveDefaultServe(manifest); got != tc.expected {
				t.Errorf("resolveDefaultServe() = %v, want %v", got, tc.expected)
			}
		})
	}
}

// TestResolveDefaultServe_NilManifest ensures a nil manifest (e.g. a node
// with no citadel.yaml yet) does not panic and falls through to the
// persisted/env precedence.
func TestResolveDefaultServe_NilManifest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "")
	if got := resolveDefaultServe(nil); got != false {
		t.Errorf("resolveDefaultServe(nil) = %v, want false", got)
	}
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	if got := resolveDefaultServe(nil); got != true {
		t.Errorf("resolveDefaultServe(nil) with env=1 = %v, want true", got)
	}
}

func TestRealDefaultServeCallbackRefusesRetargetedHeldNode(t *testing.T) {
	a := writeManifestWithServices(t, nil)
	b := filepath.Join(os.Getenv("HOME"), "held-default-serve")
	if err := os.MkdirAll(finetunesafety.Dir(b), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finetunesafety.Path(b), []byte("train-job\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeGlobalConfigFile(filepath.Join(platform.ConfigDir(), "config.yaml"), b); err != nil {
		t.Fatal(err)
	}
	deps := realDefaultServeDeps(jobs.NewServiceHandler(a))
	if err := deps.executeServiceStart("vllm", "some-model"); err == nil {
		t.Fatal("production default-serve callback followed stale node A after pointer moved to held B")
	}
	if _, err := os.Stat(filepath.Join(a, "services", "vllm.yml")); !os.IsNotExist(err) {
		t.Fatalf("stale node A compose materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b, "citadel.yaml")); !os.IsNotExist(err) {
		t.Fatalf("held node B manifest materialized: %v", err)
	}
}

// TestBlankNodeCheckForDefaultServe_ManifestServiceBlocks verifies a manifest
// entry naming any candidate serving engine is treated as "not blank",
// regardless of the on-disk cache state.
func TestBlankNodeCheckForDefaultServe_ManifestServiceBlocks(t *testing.T) {
	cacheRoot := t.TempDir() // empty: nothing cached on disk
	manifest := &CitadelManifest{Services: []Service{{Name: "ollama", ComposeFile: "services/ollama.yml"}}}

	blank, reason := blankNodeCheckForDefaultServe(manifest, cacheRoot)
	if blank {
		t.Fatalf("expected not-blank (manifest already has %q), got blank", "ollama")
	}
	if reason == "" {
		t.Error("expected a non-empty reason naming the blocking service")
	}
}

// TestBlankNodeCheckForDefaultServe_CachedModelBlocks verifies a non-empty
// engine cache directory is treated as "not blank" even with an empty
// manifest.
func TestBlankNodeCheckForDefaultServe_CachedModelBlocks(t *testing.T) {
	cacheRoot := t.TempDir()
	// "ollama"'s cache dir per services.EngineCacheDirs.
	ollamaDir := filepath.Join(cacheRoot, "ollama")
	if err := os.MkdirAll(ollamaDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ollamaDir, "some-blob"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := &CitadelManifest{} // no services

	blank, reason := blankNodeCheckForDefaultServe(manifest, cacheRoot)
	if blank {
		t.Fatalf("expected not-blank (cache dir has content), got blank; reason=%q", reason)
	}
}

// TestBlankNodeCheckForDefaultServe_TrulyBlank verifies an empty manifest and
// an empty (or missing) cache root reports blank.
func TestBlankNodeCheckForDefaultServe_TrulyBlank(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "does-not-exist")
	manifest := &CitadelManifest{Services: []Service{{Name: "not-an-engine", ComposeFile: "services/not-an-engine.yml"}}}

	blank, reason := blankNodeCheckForDefaultServe(manifest, cacheRoot)
	if !blank {
		t.Errorf("expected blank, got not-blank; reason=%q", reason)
	}
}

// TestRunDefaultServeReconcile_OptInOffIsNoOp verifies the reconcile touches
// nothing -- no marker written, no side-effecting call made -- when
// default-serve is not opted in.
func TestRunDefaultServeReconcile_OptInOffIsNoOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "")
	configDir := t.TempDir()

	called := false
	deps := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { t.Fatal("should not probe GPU when opt-in is off"); return 0, false },
		executeServiceStart:   func(string, string) error { called = true; return nil },
		log:                   func(string, ...any) {},
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)

	if called {
		t.Error("executeServiceStart should not be called when opt-in is off")
	}
	if _, ok := loadDefaultServeMarker(configDir); ok {
		t.Error("no marker should be written when opt-in is off")
	}
}

// TestRunDefaultServeReconcile_OnceEverMarkerSkipsSecondRun verifies a
// pre-existing marker short-circuits the reconcile before any GPU probe or
// service-start call, even when opted in.
func TestRunDefaultServeReconcile_OnceEverMarkerSkipsSecondRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	configDir := t.TempDir()
	if err := saveDefaultServeMarker(configDir, "applied", "ollama", "llama3.1:8b", 8192); err != nil {
		t.Fatalf("saveDefaultServeMarker: %v", err)
	}

	deps := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { t.Fatal("should not probe GPU when marker already present"); return 0, false },
		executeServiceStart: func(string, string) error {
			t.Fatal("should not start a service when marker already present")
			return nil
		},
		log: func(string, ...any) {},
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)
}

// TestRunDefaultServeReconcile_NoGPUSkipsAndWritesMarker verifies a
// not-found GPU signal is a skip (not a failure), writes the once-ever
// marker (no retry), and never calls executeServiceStart.
func TestRunDefaultServeReconcile_NoGPUSkipsAndWritesMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	configDir := t.TempDir()

	deps := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { return 0, false },
		executeServiceStart:   func(string, string) error { t.Fatal("should not start a service with no GPU"); return nil },
		log:                   func(string, ...any) {},
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)

	m, ok := loadDefaultServeMarker(configDir)
	if !ok {
		t.Fatal("expected a marker to be written")
	}
	if m.Status != "skipped:no-gpu" {
		t.Errorf("marker status = %q, want %q", m.Status, "skipped:no-gpu")
	}
}

// TestRunDefaultServeReconcile_NotBlankSkipsAndWritesMarker verifies a
// non-blank node is skipped (never calls executeServiceStart) and the
// once-ever marker records why.
func TestRunDefaultServeReconcile_NotBlankSkipsAndWritesMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	configDir := t.TempDir()
	manifest := &CitadelManifest{Services: []Service{{Name: "vllm", ComposeFile: "services/vllm.yml"}}}

	deps := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { return 24576, true },
		executeServiceStart:   func(string, string) error { t.Fatal("should not start a service on a non-blank node"); return nil },
		log:                   func(string, ...any) {},
	}
	runDefaultServeReconcile(manifest, configDir, deps)

	m, ok := loadDefaultServeMarker(configDir)
	if !ok {
		t.Fatal("expected a marker to be written")
	}
	if got := m.Status; len(got) < len("skipped:not-blank") || got[:len("skipped:not-blank")] != "skipped:not-blank" {
		t.Errorf("marker status = %q, want prefix %q", got, "skipped:not-blank")
	}
}

// TestRunDefaultServeReconcile_AppliedCallsServiceStartAndWritesMarker is the
// full happy path: opted in, GPU present, blank node -> executeServiceStart
// is called with the tier's (engine, model) and the marker records "applied".
func TestRunDefaultServeReconcile_AppliedCallsServiceStartAndWritesMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	configDir := t.TempDir()

	var gotEngine, gotModel string
	calls := 0
	deps := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { return 8192, true }, // 6-12GB tier
		executeServiceStart: func(engine, model string) error {
			calls++
			gotEngine, gotModel = engine, model
			return nil
		},
		log: func(string, ...any) {},
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)

	if calls != 1 {
		t.Fatalf("executeServiceStart called %d times, want 1", calls)
	}
	if gotEngine != "ollama" || gotModel != "llama3.1:8b" {
		t.Errorf("executeServiceStart(%q, %q), want (\"ollama\", \"llama3.1:8b\")", gotEngine, gotModel)
	}
	m, ok := loadDefaultServeMarker(configDir)
	if !ok {
		t.Fatal("expected a marker to be written")
	}
	if m.Status != "applied" || m.Engine != "ollama" || m.Model != "llama3.1:8b" {
		t.Errorf("marker = %+v, want applied/ollama/llama3.1:8b", m)
	}
}

// TestRunDefaultServeReconcile_FailureWritesMarkerAndNeverRetries verifies a
// failed executeServiceStart still writes the once-ever marker (so the node
// never crash-loops retrying a doomed pull) with a "failed:" status.
func TestRunDefaultServeReconcile_FailureWritesMarkerAndNeverRetries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	configDir := t.TempDir()

	deps := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { return 24576, true }, // vllm tier
		executeServiceStart:   func(string, string) error { return errBoom },
		log:                   func(string, ...any) {},
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)

	m, ok := loadDefaultServeMarker(configDir)
	if !ok {
		t.Fatal("expected a marker to be written even on failure")
	}
	if len(m.Status) < len("failed:") || m.Status[:len("failed:")] != "failed:" {
		t.Errorf("marker status = %q, want prefix %q", m.Status, "failed:")
	}

	// A second run must NOT retry, even though it failed.
	deps2 := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { t.Fatal("should not probe GPU after a failed once-ever attempt"); return 0, false },
		executeServiceStart:   func(string, string) error { t.Fatal("should not retry a failed once-ever attempt"); return nil },
		log:                   func(string, ...any) {},
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps2)
}

type staticError string

func (e staticError) Error() string { return string(e) }

var errBoom = staticError("boom")

func TestDefaultServeHeldBlank24GBDefersWithoutMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	configDir := t.TempDir()
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	original := []byte("services: []\n")
	if err := os.WriteFile(manifestPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(finetunesafety.Dir(configDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finetunesafety.Path(configDir), []byte("train-job"), 0600); err != nil {
		t.Fatal(err)
	}
	probe, execute := 0, 0
	deps := defaultServeDeps{
		largestGPUTotalVRAMMB: func() (int, bool) { probe++; return 24576, true },
		executeServiceStart:   func(string, string) error { execute++; return nil },
		log:                   func(string, ...any) {},
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)
	if probe != 0 || execute != 0 {
		t.Fatalf("held default-serve probed/started: %d/%d", probe, execute)
	}
	if _, ok := loadDefaultServeMarker(configDir); ok {
		t.Fatal("hold consumed once-ever marker")
	}
	if got, err := os.ReadFile(manifestPath); err != nil || string(got) != string(original) {
		t.Fatalf("manifest mutated: %q %v", got, err)
	}
	if err := finetunesafety.RequireOwned(configDir, "train-job"); err != nil {
		t.Fatalf("hold lost: %v", err)
	}
	if err := os.Remove(finetunesafety.Path(configDir)); err != nil {
		t.Fatal(err)
	}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)
	if probe != 1 || execute != 1 {
		t.Fatalf("default-serve did not resume: %d/%d", probe, execute)
	}
	if marker, ok := loadDefaultServeMarker(configDir); !ok || marker.Status != "applied" || marker.Engine == "" {
		t.Fatalf("marker after clear: %+v %t", marker, ok)
	}
}

func TestRealDefaultServeDepsRefusesHeldCalleeBeforeExecute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_DEFAULT_SERVE", "1")
	configDir := t.TempDir()
	manifestPath := filepath.Join(configDir, "citadel.yaml")
	original := []byte("services: []\n")
	if err := os.WriteFile(manifestPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(finetunesafety.Dir(configDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finetunesafety.Path(configDir), []byte("train-job"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := realDefaultServeDeps(jobs.NewServiceHandler(configDir))
	deps.largestGPUTotalVRAMMB = func() (int, bool) { t.Fatal("GPU probe under hold"); return 0, false }
	deps.log = func(string, ...any) {}
	runDefaultServeReconcile(&CitadelManifest{}, configDir, deps)
	if err := deps.executeServiceStart("vllm", "some-model"); err == nil {
		t.Fatal("production callback admitted held SERVICE_START")
	}
	if _, ok := loadDefaultServeMarker(configDir); ok {
		t.Fatal("production path consumed marker")
	}
	if got, err := os.ReadFile(manifestPath); err != nil || string(got) != string(original) {
		t.Fatalf("production path mutated manifest: %q %v", got, err)
	}
}
