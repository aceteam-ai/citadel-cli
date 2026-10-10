package jobs

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// stubTEISeams points the GPU/runtime seams at fixtures so the SERVICE_START /
// APPLY_DEVICE_CONFIG compose-up wiring is testable without shelling out to the
// real GPU (this build box is a live GPU node). Restored on cleanup.
func stubTEISeams(t *testing.T, caps []string, providesGPU bool) {
	t.Helper()
	prevCaps, prevRT := teiGPUComputeCaps, teiRuntimeProvidesGPU
	teiGPUComputeCaps = func() []string { return caps }
	teiRuntimeProvidesGPU = func() bool { return providesGPU }
	t.Cleanup(func() { teiGPUComputeCaps, teiRuntimeProvidesGPU = prevCaps, prevRT })
}

func TestJobsTEIComposeEnvEntries(t *testing.T) {
	threads := func(n int) *int { return &n }

	t.Run("gpu-docker-injects-image-tag", func(t *testing.T) {
		stubTEISeams(t, []string{"8.6"}, true)
		got := teiComposeEnvEntries("tei", nil)
		want := []string{services.EnvTEIImageTag + "=86-1.6"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("cpu-threads-opt-in", func(t *testing.T) {
		stubTEISeams(t, nil, true)
		got := teiComposeEnvEntries("tei", threads(4))
		want := []string{services.EnvTEINumThreads + "=4"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("non-tei-never-probes", func(t *testing.T) {
		prevCaps, prevRT := teiGPUComputeCaps, teiRuntimeProvidesGPU
		teiGPUComputeCaps = func() []string { t.Fatal("non-tei must not probe GPU caps"); return nil }
		teiRuntimeProvidesGPU = func() bool { t.Fatal("non-tei must not probe runtime"); return false }
		t.Cleanup(func() { teiGPUComputeCaps, teiRuntimeProvidesGPU = prevCaps, prevRT })
		if got := teiComposeEnvEntries("ollama", threads(4)); got != nil {
			t.Errorf("non-tei service must inject nothing, got %v", got)
		}
	})
}

func TestJobsResolveTEIThreads(t *testing.T) {
	if got := resolveTEIThreads(nil); got != 0 {
		t.Errorf("nil => 0, got %d", got)
	}
	n := 5
	if got := resolveTEIThreads(&n); got != 5 {
		t.Errorf("explicit 5 => 5, got %d", got)
	}
	zero := 0
	if got, want := resolveTEIThreads(&zero), services.TEICPUThreads(runtime.NumCPU()); got != want {
		t.Errorf("auto (0) => %d, got %d", want, got)
	}
}

// TestManifestServiceThreadsFromFile pins the APPLY_DEVICE_CONFIG manifest read
// of the `threads:` opt-in.
func TestManifestServiceThreadsFromFile(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "citadel.yaml")
	const body = `services:
  - name: tei
    threads: 6
  - name: vllm
`
	if err := os.WriteFile(manifest, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := manifestServiceThreadsFromFile(manifest, "tei"); got == nil || *got != 6 {
		t.Errorf("tei threads = %v, want 6", got)
	}
	if got := manifestServiceThreadsFromFile(manifest, "vllm"); got != nil {
		t.Errorf("vllm threads = %v, want nil (unset)", got)
	}
	if got := manifestServiceThreadsFromFile(manifest, "absent"); got != nil {
		t.Errorf("absent service threads = %v, want nil", got)
	}
	if got := manifestServiceThreadsFromFile(filepath.Join(dir, "nope.yaml"), "tei"); got != nil {
		t.Errorf("unreadable manifest threads = %v, want nil", got)
	}
}
