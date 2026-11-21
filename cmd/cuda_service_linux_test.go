//go:build linux

package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// These wiring regressions execute in GitHub CI. Local cmd execution is held.
func TestBootSkipsCUDAEnginesWithOneClearLineEach(t *testing.T) {
	names := []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"}
	var manifest []Service
	for _, name := range names {
		manifest = append(manifest, Service{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml"})
	}
	writeManifestWithServices(t, manifest)
	t.Setenv("PATH", t.TempDir())
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = previous; reader.Close(); writer.Close() })
	probes := 0
	started := startManagedServices(context.Background(), func() int { probes++; return 0 })
	writer.Close()
	os.Stdout = previous
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 0 || probes != 1 {
		t.Fatal("GPU-less boot attempted services or repeated probe", len(started), probes)
	}
	for _, name := range names {
		if strings.Count(string(output), name+" requires an NVIDIA GPU, not starting") != 1 {
			t.Fatal("missing/duplicate clear skip", name, string(output))
		}
		if strings.Contains(string(output), "Starting "+name+"...") {
			t.Fatal("unsupported engine attempted", name)
		}
	}
}

func TestCLIStartCUDAAbsenceFailsBeforeCompose(t *testing.T) {
	writeManifestWithServices(t, nil)
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
		if err := startService(name, "", func() int { return 0 }); err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") {
			t.Fatal("no early CUDA refusal", name, err)
		}
	}
	for _, count := range []int{-1, 1} {
		if err := startService("bonsai", "", func() int { return count }); err == nil || !strings.Contains(err.Error(), "has no compose_file") {
			t.Fatal("unknown/present probe suppressed normal start", count, err)
		}
	}
}

func TestControlCenterConfiguredCUDARefusesBeforeEffects(t *testing.T) {
	for action, change := range map[string]func(string, func() int) error{"start": ccStartServiceForHost, "restart": ccRestartServiceForHost} {
		for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				dir := writeManifestWithServices(t, []Service{{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml", DesiredStatus: "stopped"}})
				t.Setenv("PATH", t.TempDir())
				before, err := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				probes := 0
				err = change(name, func() int { probes++; return 0 })
				if err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") || probes != 1 {
					t.Fatalf("missing host refusal: %v, probes=%d", err, probes)
				}
				after, err := os.ReadFile(filepath.Join(dir, "citadel.yaml"))
				if err != nil || string(after) != string(before) {
					t.Fatal("refusal changed stopped manifest", err)
				}
			})
		}
	}
}

func TestControlCenterConfiguredCPUReconcilesBeforeRuntimeEffects(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	for action, change := range map[string]func(string, func() int) error{"start": ccStartServiceForHost, "restart": ccRestartServiceForHost} {
		for _, name := range []string{"ollama", "llamacpp"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				dir := writeManifestWithServices(t, []Service{{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml", DesiredStatus: "stopped"}})
				if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "services", name+".yml")
				if err := os.WriteFile(path, []byte(services.ServiceMap[name]), 0600); err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 37\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				t.Setenv("CITADEL_CONTAINER_RUNTIME", "docker")
				probes := 0
				if err := change(name, func() int { probes++; return 0 }); err == nil {
					t.Fatal("fake engine did not refuse")
				}
				expected, _ := services.ComposeForHost(name, "linux", func() int { return 0 })
				got, err := os.ReadFile(path)
				if err != nil || string(got) != expected || probes != 1 {
					t.Fatalf("configured service kept GPU compose: %v, probes=%d", err, probes)
				}
			})
		}
	}
}
