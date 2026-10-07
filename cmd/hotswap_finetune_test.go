package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
)

func TestHotSwapStartRefusesRetargetedHeldNode(t *testing.T) {
	a := writeManifestWithServices(t, nil)
	b := filepath.Join(os.Getenv("HOME"), "held-b")
	if err := os.MkdirAll(finetunesafety.Dir(b), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finetunesafety.Path(b), []byte("train-job\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(a, "citadel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	controller := newSwapController(a, t.TempDir(), nil, nil)
	pointer := filepath.Join(os.Getenv("HOME"), ".citadel-cli", "config.yaml")
	if err := writeGlobalConfigFile(pointer, b); err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(context.Background(), "vllm", "some-model"); err == nil || !strings.Contains(err.Error(), "node configuration changed") {
		t.Fatalf("hotswap stale-A start = %v, want refusal", err)
	}
	if got, _ := os.ReadFile(filepath.Join(a, "citadel.yaml")); string(got) != string(before) {
		t.Fatalf("A manifest mutated: %q", got)
	}
	if _, err := os.Stat(filepath.Join(a, "services", "vllm.yml")); !os.IsNotExist(err) {
		t.Fatalf("A compose materialized: %v", err)
	}
	if got, _ := os.ReadFile(finetunesafety.Path(b)); string(got) != "train-job\n" {
		t.Fatalf("B hold mutated: %q", got)
	}
}
