//go:build linux

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/services"
)

func TestNativeCUDAServiceStartAdmission(t *testing.T) {
	for _, kind := range []string{"native", ""} {
		for _, tc := range []struct {
			name       string
			gpu        int
			serving    bool
			wantStarts int
		}{
			{name: "absent", gpu: 0},
			{name: "unknown", gpu: -1, wantStarts: 1},
			{name: "present", gpu: 1, wantStarts: 1},
			{name: "already serving", gpu: 0, serving: true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := NewServiceHandler(t.TempDir())
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, "vllm"), []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				h.gpuCount = func() int { return tc.gpu }
				h.nativeServiceServingFn = func(string) bool { return tc.serving }
				starts := 0
				h.nativeServiceStartFn = func(string, string) (*services.NativeProcess, error) {
					starts++
					return nil, fmt.Errorf("synthetic native launch refused")
				}
				out, err := h.serviceStart(testCtx(), manifestService{Name: "vllm", Type: kind}, "", 0, 0, trustRemoteCodeUnspecified)
				if starts != tc.wantStarts {
					t.Fatalf("starts=%d want %d; %s %v", starts, tc.wantStarts, out, err)
				}
				if tc.serving {
					var result serviceResult
					if err != nil || json.Unmarshal(out, &result) != nil || !result.Running {
						t.Fatalf("serving engine refused: %s %v", out, err)
					}
				} else if tc.gpu == 0 && (err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU")) {
					t.Fatalf("missing native refusal: %s %v", out, err)
				}
				if files, err := os.ReadDir(h.ConfigDir); err != nil || len(files) != 0 {
					t.Fatal("native admission changed managed state", files, err)
				}
			})
		}
	}
}

func TestNativeCPUServiceStartDoesNotProbeGPU(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp"} {
		t.Run(name, func(t *testing.T) {
			h := NewServiceHandler(t.TempDir())
			h.gpuCount = func() int { t.Fatal("CPU engine probed for CUDA admission"); return 0 }
			h.nativeServiceServingFn = func(string) bool { return false }
			starts := 0
			h.nativeServiceStartFn = func(string, string) (*services.NativeProcess, error) {
				starts++
				return nil, fmt.Errorf("synthetic native launch refused")
			}
			_, _ = h.serviceStart(testCtx(), manifestService{Name: name, Type: "native"}, "", 0, 0, trustRemoteCodeUnspecified)
			if starts != 1 {
				t.Fatalf("CPU launch attempts=%d", starts)
			}
		})
	}
}

func TestCUDAServiceStartRefusesBeforeAnyEngineEffectOnAbsentGPU(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
		t.Run(name, func(t *testing.T) {
			h := NewServiceHandler(t.TempDir())
			h.gpuCount = func() int { return 0 }
			ownershipProbes, servingProbes := 0, 0
			h.dockerServiceRunningFn = func(string) bool { ownershipProbes++; return false }
			h.externalEngineServingFn = func(context.Context, string, int) ([]string, bool) {
				servingProbes++
				return nil, false
			}
			_, err := h.serviceStart(testCtx(), manifestService{Name: name, Type: "docker"}, "", 0, 0, trustRemoteCodeUnspecified)
			if err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") {
				t.Fatal("missing early GPU refusal", err)
			}
			wantProbes := 0
			if name == "vllm" {
				wantProbes = 1
			}
			if ownershipProbes != wantProbes || servingProbes != wantProbes {
				t.Fatal("unexpected adoption probes", ownershipProbes, servingProbes)
			}
			if files, err := os.ReadDir(h.ConfigDir); err != nil || len(files) != 0 {
				t.Fatal("refusal changed managed service files", files, err)
			}
		})
	}
}

func TestCUDAServiceStartUnknownOrPresentKeepsNormalAttempt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, count := range []int{1, -1} {
		for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
			h := NewServiceHandler(t.TempDir())
			h.gpuCount = func() int { return count }
			attempts := 0
			h.dockerServiceRunningFn = func(string) bool { attempts++; return true }
			h.externalEngineServingFn = func(context.Context, string, int) ([]string, bool) { return nil, false }
			// Template reconciliation validates the configured path before the
			// running shortcut; this control represents a valid managed service.
			out, err := h.serviceStart(testCtx(), manifestService{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml"}, "", 0, 0, trustRemoteCodeUnspecified)
			var result serviceResult
			if err != nil || json.Unmarshal(out, &result) != nil || !result.Running || attempts == 0 {
				t.Fatal("possible GPU was suppressed", name, count, err, string(out), attempts)
			}
		}
	}
}

func TestDeviceConfigCUDAAbsenceRefusesBeforeRuntimePreflight(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	h := NewConfigHandler(t.TempDir())
	h.gpuCount = func() int { return 0 }
	if err := h.startServices(h.ConfigDir, []string{"bonsai"}); err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") {
		t.Fatal("config start reached runtime instead of host refusal", err)
	}
}

func TestCUDAServiceStartAdoptsExternalDespiteAbsentGPU(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	h := serviceHandlerWithAdoptSeams(t, false, true, []string{"synthetic/model"})
	h.gpuCount = func() int { return 0 }
	out, err := h.serviceStart(testCtx(), manifestService{Name: "vllm", Type: "docker"}, "", 0, 0, trustRemoteCodeUnspecified)
	var result serviceResult
	if err != nil || json.Unmarshal(out, &result) != nil || !result.Running || !strings.Contains(result.Message, "adopted external vllm") {
		t.Fatal("external engine was refused", err, string(out))
	}
	if files, err := os.ReadDir(h.ConfigDir); err != nil || len(files) != 0 {
		t.Fatal("adoption wrote managed engine files", files, err)
	}
}
