//go:build linux

package cmd

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
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
