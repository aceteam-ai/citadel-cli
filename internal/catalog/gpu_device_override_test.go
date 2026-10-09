package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

const voiceCloneComposeGPU = `services:
  voice-clone:
    environment:
      - VOICE_CLONE_DEVICE=cuda
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: ${VOICE_CLONE_GPU_COUNT:-all}
              capabilities: [gpu]
`

// TestCopyGPUDeviceOverride pins the install-time copy of a module's sibling
// compose.gpu.yml to <servicesDir>/<name>.gpu.yml: copied only when the module
// requires a GPU AND ships the override; a no-op (and no file written) otherwise.
func TestCopyGPUDeviceOverride(t *testing.T) {
	t.Run("gpu module with override -> copied", func(t *testing.T) {
		srcDir := t.TempDir()
		composeSrc := filepath.Join(srcDir, "compose.yml")
		if err := os.WriteFile(composeSrc, []byte("services: {voice-clone: {image: x}}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "compose.gpu.yml"), []byte(voiceCloneComposeGPU), 0644); err != nil {
			t.Fatal(err)
		}
		servicesDir := t.TempDir()
		got, err := copyGPUDeviceOverride(composeSrc, servicesDir, "voice-clone", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := GPUDeviceOverridePath(servicesDir, "voice-clone")
		if got != want {
			t.Fatalf("returned path = %q, want %q", got, want)
		}
		data, err := os.ReadFile(want)
		if err != nil {
			t.Fatalf("override not written: %v", err)
		}
		if string(data) != voiceCloneComposeGPU {
			t.Errorf("override content was not copied verbatim")
		}
	})

	t.Run("non-gpu module with override -> no-op", func(t *testing.T) {
		srcDir := t.TempDir()
		composeSrc := filepath.Join(srcDir, "compose.yml")
		if err := os.WriteFile(composeSrc, []byte("services: {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "compose.gpu.yml"), []byte(voiceCloneComposeGPU), 0644); err != nil {
			t.Fatal(err)
		}
		servicesDir := t.TempDir()
		got, err := copyGPUDeviceOverride(composeSrc, servicesDir, "voice-clone", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("wantGPU=false must not copy an override, got %q", got)
		}
		if _, err := os.Stat(GPUDeviceOverridePath(servicesDir, "voice-clone")); err == nil {
			t.Errorf("wantGPU=false must not write a .gpu.yml file")
		}
	})

	t.Run("gpu module without override -> no-op", func(t *testing.T) {
		srcDir := t.TempDir()
		composeSrc := filepath.Join(srcDir, "compose.yml")
		if err := os.WriteFile(composeSrc, []byte("services: {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		servicesDir := t.TempDir()
		got, err := copyGPUDeviceOverride(composeSrc, servicesDir, "kokoro", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("a module shipping no compose.gpu.yml must be a no-op, got %q", got)
		}
	})
}

// TestGPUDeviceOverrideFileArg pins the docker-vs-podman layering decision: the
// override is layered as a second `-f` on docker when present, never on podman
// (the override carries a Docker reservation Podman consumes via CDI; the node's
// per-up-site rewrite handles the BASE only — see the func doc), and never when
// the module ships no override.
func TestGPUDeviceOverrideFileArg(t *testing.T) {
	servicesDir := t.TempDir()
	override := GPUDeviceOverridePath(servicesDir, "voice-clone")
	if err := os.WriteFile(override, []byte(voiceCloneComposeGPU), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("docker with override -> layered", func(t *testing.T) {
		got := GPUDeviceOverrideFileArg(servicesDir, "voice-clone", "docker")
		want := []string{"-f", override}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("docker args = %v, want %v", got, want)
		}
	})

	t.Run("podman with override -> not layered (follow-up)", func(t *testing.T) {
		if got := GPUDeviceOverrideFileArg(servicesDir, "voice-clone", "podman"); got != nil {
			t.Fatalf("podman must not layer the raw device override, got %v", got)
		}
	})

	t.Run("docker without override -> nil", func(t *testing.T) {
		if got := GPUDeviceOverrideFileArg(servicesDir, "no-such-module", "docker"); got != nil {
			t.Fatalf("a module with no .gpu.yml must yield nil, got %v", got)
		}
	})
}
