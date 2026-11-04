//go:build linux

package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCUDAServiceStartRefusesBeforeAnyEngineEffectOnAbsentGPU(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"} {
		t.Run(name, func(t *testing.T) {
			h := NewServiceHandler(t.TempDir())
			h.gpuCount = func() int { return 0 }
			h.dockerServiceRunningFn = func(string) bool { t.Fatal("absence reached runtime probe"); return false }
			h.externalEngineServingFn = func(context.Context, string, int) ([]string, bool) {
				t.Fatal("absence reached external probe")
				return nil, false
			}
			_, err := h.serviceStart(testCtx(), manifestService{Name: name, Type: "docker"}, "", 0, 0, trustRemoteCodeUnspecified)
			if err == nil || !strings.Contains(err.Error(), "requires an NVIDIA GPU, not starting") {
				t.Fatal("missing early GPU refusal", err)
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
