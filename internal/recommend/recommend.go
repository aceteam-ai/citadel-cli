// Package recommend provides hardware-based service recommendations.
package recommend

import (
	"runtime"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// ServiceRecommendation represents a recommended service with context
type ServiceRecommendation struct {
	Service     string
	Reason      string
	Recommended bool
}

// GetRecommendations returns service recommendations based on hardware
func GetRecommendations() []ServiceRecommendation {
	vramMB := platform.GetGPUMemoryMB()
	hasGPU := vramMB > 0
	isMac := runtime.GOOS == "darwin"
	isAppleSilicon := isMac && runtime.GOARCH == "arm64"

	var recommendations []ServiceRecommendation

	// Apple Silicon Mac
	if isAppleSilicon {
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "ollama",
			Reason:      "Best for Apple Silicon (native Metal acceleration)",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "llamacpp",
			Reason:      "Good Metal support, flexible model loading",
			Recommended: false,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "vllm",
			Reason:      "Requires NVIDIA GPU (Linux only)",
			Recommended: false,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "lmstudio",
			Reason:      "Use native macOS app instead",
			Recommended: false,
		})
		return recommendations
	}

	// Intel Mac
	if isMac {
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "ollama",
			Reason:      "Best option for Intel Mac (CPU inference)",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "llamacpp",
			Reason:      "Good for CPU inference with quantized models",
			Recommended: false,
		})
		return recommendations
	}

	// No GPU - CPU only
	if !hasGPU {
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "ollama",
			Reason:      "Best for CPU inference (optimized quantization)",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "llamacpp",
			Reason:      "Good for CPU with smaller quantized models",
			Recommended: false,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "vllm",
			Reason:      "Requires GPU",
			Recommended: false,
		})
		return recommendations
	}

	// Has GPU - check VRAM
	vramGB := vramMB / 1024

	switch {
	case vramGB >= 16:
		// 16GB+ VRAM - vLLM for production, can run large models
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "vllm",
			Reason:      "Best for high-throughput production (16GB+ VRAM)",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "ollama",
			Reason:      "Good for development and experimentation",
			Recommended: false,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "llamacpp",
			Reason:      "Flexible model formats, good for llama models",
			Recommended: false,
		})

	case vramGB >= 8:
		// 8-16GB VRAM - can run 13B-30B models
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "llamacpp",
			Reason:      "Good for 13B-30B models (8-16GB VRAM)",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "vllm",
			Reason:      "High throughput, may need smaller models",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "ollama",
			Reason:      "Easy to use, good model variety",
			Recommended: false,
		})

	case vramGB >= 4:
		// 4-8GB VRAM - 7B models work well
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "ollama",
			Reason:      "Best for 7B models (4-8GB VRAM)",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "llamacpp",
			Reason:      "Good with quantized 7B models",
			Recommended: false,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "vllm",
			Reason:      "Works with smaller models",
			Recommended: false,
		})

	default:
		// Less than 4GB VRAM - use CPU or quantized
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "ollama",
			Reason:      "Best for limited VRAM (uses CPU+GPU hybrid)",
			Recommended: true,
		})
		recommendations = append(recommendations, ServiceRecommendation{
			Service:     "llamacpp",
			Reason:      "Good with heavily quantized models",
			Recommended: false,
		})
	}

	return recommendations
}

// GetRecommendedService returns the top recommended service name
func GetRecommendedService() string {
	recommendations := GetRecommendations()
	for _, rec := range recommendations {
		if rec.Recommended {
			return rec.Service
		}
	}
	return "ollama" // Default fallback
}

// IsRecommended returns true if the given service is recommended for this hardware
func IsRecommended(service string) bool {
	for _, rec := range GetRecommendations() {
		if rec.Service == service {
			return rec.Recommended
		}
	}
	return false
}

// GetRecommendationReason returns the recommendation reason for a service
func GetRecommendationReason(service string) string {
	for _, rec := range GetRecommendations() {
		if rec.Service == service {
			return rec.Reason
		}
	}
	return ""
}
