//go:build linux

package jobs

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

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
			out, err := h.serviceStart(testCtx(), manifestService{Name: name, Type: "docker"}, "", 0, 0, trustRemoteCodeUnspecified)
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
