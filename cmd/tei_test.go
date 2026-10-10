package cmd

import (
	"reflect"
	"runtime"
	"testing"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// stubTEISeams points the GPU/runtime seams at fixtures so the compose-up wiring
// is testable without shelling out to the real GPU (this build box is a live GPU
// node). Restored on cleanup.
func stubTEISeams(t *testing.T, caps []string, providesGPU bool) {
	t.Helper()
	prevCaps, prevRT := teiGPUComputeCaps, teiRuntimeProvidesGPU
	teiGPUComputeCaps = func() []string { return caps }
	teiRuntimeProvidesGPU = func() bool { return providesGPU }
	t.Cleanup(func() { teiGPUComputeCaps, teiRuntimeProvidesGPU = prevCaps, prevRT })
}

func TestTEIComposeEnvEntries_InjectionAtComposeUp(t *testing.T) {
	cpuThreads := func(n int) *int { return &n }

	t.Run("gpu-docker-injects-image-tag", func(t *testing.T) {
		stubTEISeams(t, []string{"8.6"}, true)
		got := teiComposeEnvEntries("tei", nil)
		want := []string{svcports.EnvTEIImageTag + "=86-1.6"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("gpu-podman-falls-back-to-cpu-no-tag", func(t *testing.T) {
		stubTEISeams(t, []string{"8.6"}, false) // Podman can't hand the reservation-less tei.yml a GPU
		if got := teiComposeEnvEntries("tei", nil); got != nil {
			t.Errorf("Podman GPU node must not inject an image tag, got %v", got)
		}
	})

	t.Run("cpu-no-threads-opt-in-injects-nothing", func(t *testing.T) {
		stubTEISeams(t, nil, true)
		if got := teiComposeEnvEntries("tei", nil); got != nil {
			t.Errorf("CPU node with no threads opt-in must inject nothing, got %v", got)
		}
	})

	t.Run("cpu-threads-opt-in", func(t *testing.T) {
		stubTEISeams(t, nil, true)
		got := teiComposeEnvEntries("tei", cpuThreads(6))
		want := []string{svcports.EnvTEINumThreads + "=6"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("non-tei-never-injects-and-never-probes", func(t *testing.T) {
		// The seams fail the test if touched: a non-tei start must short-circuit
		// before probing the GPU.
		prevCaps, prevRT := teiGPUComputeCaps, teiRuntimeProvidesGPU
		teiGPUComputeCaps = func() []string { t.Fatal("non-tei must not probe GPU caps"); return nil }
		teiRuntimeProvidesGPU = func() bool { t.Fatal("non-tei must not probe runtime"); return false }
		t.Cleanup(func() { teiGPUComputeCaps, teiRuntimeProvidesGPU = prevCaps, prevRT })
		if got := teiComposeEnvEntries("vllm", cpuThreads(6)); got != nil {
			t.Errorf("non-tei service must inject nothing, got %v", got)
		}
	})
}

// TestTEIComposeEnvEntries_OperatorOverride verifies an operator's explicit
// CITADEL_TEI_IMAGE_TAG in the process env suppresses citadel's injection (Go's
// last-wins append would otherwise clobber it on a VRAM-contended box).
func TestTEIComposeEnvEntries_OperatorOverride(t *testing.T) {
	stubTEISeams(t, []string{"8.6"}, true)
	t.Setenv(svcports.EnvTEIImageTag, "cpu-1.6")
	if got := teiComposeEnvEntries("tei", nil); got != nil {
		t.Errorf("operator override must suppress injection, got %v", got)
	}
}

func TestResolveTEIThreads(t *testing.T) {
	if got := resolveTEIThreads(nil); got != 0 {
		t.Errorf("nil threads => 0 (no inject), got %d", got)
	}
	explicit := 7
	if got := resolveTEIThreads(&explicit); got != 7 {
		t.Errorf("explicit 7 => 7, got %d", got)
	}
	zero := 0
	if got, want := resolveTEIThreads(&zero), svcports.TEICPUThreads(runtime.NumCPU()); got != want {
		t.Errorf("auto (0) => TEICPUThreads(NumCPU)=%d, got %d", want, got)
	}
}
