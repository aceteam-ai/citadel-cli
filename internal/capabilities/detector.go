package capabilities

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const detectionTimeout = 5 * time.Second

var validTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:._-]*$`)

// Capability represents a node capability tag used for queue routing.
type Capability struct {
	Tag         string
	Category    string
	Description string
}

// GPUDevice represents a single detected GPU with structured fields.
type GPUDevice struct {
	Name    string `json:"name" yaml:"name"`
	VRAMMb  int    `json:"vram_mb" yaml:"vram_mb"`
	Tag     string `json:"tag" yaml:"tag"`           // normalized tag e.g. "rtx3090"
	VRAMTag string `json:"vram_tag" yaml:"vram_tag"` // e.g. "24gb"
}

// GPUCapabilities holds the full GPU capability summary for a node.
type GPUCapabilities struct {
	Devices      []GPUDevice `json:"devices,omitempty" yaml:"devices,omitempty"`
	Count        int         `json:"count" yaml:"count"`
	DriverStatus string      `json:"driver_status,omitempty" yaml:"driver_status,omitempty"` // "ok", "not_loaded", "error", or "" (unknown)
	DriverError  string      `json:"driver_error,omitempty" yaml:"driver_error,omitempty"`   // human-readable error when drivers fail
}

// NodeCapabilities aggregates all detected capabilities for a node.
type NodeCapabilities struct {
	GPU     *GPUCapabilities `json:"gpu,omitempty" yaml:"gpu,omitempty"`
	Engines []string         `json:"engines,omitempty" yaml:"engines,omitempty"` // running inference engines
	Tags    []string         `json:"tags,omitempty" yaml:"tags,omitempty"`       // all capability tags
}

// DetectGPUCapabilities runs nvidia-smi and returns structured GPU information.
// When nvidia-smi fails but lspci detects NVIDIA hardware, a GPUCapabilities
// is still returned with the hardware name but empty Tag/VRAMTag fields so
// the GPU is visible in status displays without producing routing tags.
// Returns nil only if no NVIDIA hardware is detected at all.
func DetectGPUCapabilities() *GPUCapabilities {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,memory.total,count", "--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		// nvidia-smi failed — check if hardware is physically present via lspci
		hwName := platform.DetectNvidiaHardware()
		if hwName == "" {
			return nil // No NVIDIA hardware at all
		}
		// Hardware present but drivers not working — return display-only entry
		// with empty Tag/VRAMTag so no routing tags are generated.
		return &GPUCapabilities{
			Devices: []GPUDevice{
				{Name: hwName}, // Tag and VRAMTag intentionally empty
			},
			Count:        1,
			DriverStatus: "not_loaded",
			DriverError:  platform.NvidiaSMIErrorMessage(err),
		}
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var devices []GPUDevice
	totalCount := 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 3)
		if len(parts) < 2 {
			continue
		}

		gpuName := strings.TrimSpace(parts[0])
		memoryMBStr := strings.TrimSpace(parts[1])
		gpuTag := NormalizeGPUName(gpuName)
		vramGB := NormalizeVRAM(memoryMBStr)

		vramMB := 0
		if v, err := strconv.Atoi(memoryMBStr); err == nil {
			vramMB = v
		}

		// nvidia-smi "count" returns the total GPU count on every row
		if len(parts) >= 3 {
			if c, err := strconv.Atoi(strings.TrimSpace(parts[2])); err == nil && c > totalCount {
				totalCount = c
			}
		}

		vramTag := ""
		if vramGB != "" {
			vramTag = vramGB + "gb"
		}

		devices = append(devices, GPUDevice{
			Name:    gpuName,
			VRAMMb:  vramMB,
			Tag:     gpuTag,
			VRAMTag: vramTag,
		})
	}

	if len(devices) == 0 {
		return nil
	}

	if totalCount == 0 {
		totalCount = len(devices)
	}

	return &GPUCapabilities{
		Devices:      devices,
		Count:        totalCount,
		DriverStatus: "ok",
	}
}

// DetectNodeCapabilities returns the full node capabilities including GPU and running engines.
func DetectNodeCapabilities() *NodeCapabilities {
	caps := &NodeCapabilities{}

	// GPU detection
	gpuCaps := DetectGPUCapabilities()
	if gpuCaps != nil {
		caps.GPU = gpuCaps

		// Build tags from GPU info
		seen := make(map[string]bool)
		for i, dev := range gpuCaps.Devices {
			if dev.Tag != "" {
				tag := "gpu:" + dev.Tag
				if !seen[tag] {
					seen[tag] = true
					caps.Tags = append(caps.Tags, tag)
				}
			}
			if dev.VRAMTag != "" {
				tag := "vram:" + dev.VRAMTag
				if !seen[tag] {
					seen[tag] = true
					caps.Tags = append(caps.Tags, tag)
				}
			}
			// Indexed tag
			if dev.Tag != "" && dev.VRAMTag != "" {
				indexedTag := fmt.Sprintf("gpu:%d:%s:%s", i, dev.Tag, dev.VRAMTag)
				if ValidateTag(indexedTag) {
					caps.Tags = append(caps.Tags, indexedTag)
				}
			}
		}
	}

	// Detect running inference engines
	caps.Engines = detectRunningEngines()

	// Add engine tags
	for _, engine := range caps.Engines {
		tag := "engine:" + engine
		if ValidateTag(tag) {
			caps.Tags = append(caps.Tags, tag)
		}
	}

	// Always add cpu:general
	caps.Tags = append(caps.Tags, "cpu:general")

	// Add OS tag
	caps.Tags = append(caps.Tags, "os:"+runtime.GOOS)

	// Add architecture tag
	caps.Tags = append(caps.Tags, "arch:"+runtime.GOARCH)

	return caps
}

// detectRunningEngines checks for running inference engine processes/containers.
func detectRunningEngines() []string {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()

	// Check for running docker containers matching known engine names
	cmd := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	names := strings.Split(strings.TrimSpace(string(output)), "\n")
	return matchEngines(names)
}

// matchEngines maps container names to inference engine identifiers.
// Exported for testing. The "llama" keyword only matches when the name
// does NOT also contain "ollama", preventing false llamacpp detection.
func matchEngines(names []string) []string {
	var engines []string
	seen := make(map[string]bool)

	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}

		hasOllama := strings.Contains(name, "ollama")

		type rule struct {
			keyword string
			engine  string
		}
		rules := []rule{
			{"vllm", "vllm"},
			{"sglang", "sglang"},
			{"ollama", "ollama"},
			{"llamacpp", "llamacpp"},
			{"lmstudio", "lmstudio"},
		}

		for _, r := range rules {
			if strings.Contains(name, r.keyword) && !seen[r.engine] {
				seen[r.engine] = true
				engines = append(engines, r.engine)
			}
		}

		// "llama" is a fallback for llamacpp, but only when the name
		// does not also contain "ollama" (which is a substring match).
		if strings.Contains(name, "llama") && !hasOllama && !seen["llamacpp"] {
			seen["llamacpp"] = true
			engines = append(engines, "llamacpp")
		}
	}

	return engines
}

// Detect auto-detects hardware and software capabilities of the current node.
func Detect() []Capability {
	var caps []Capability
	caps = append(caps, detectGPU()...)
	caps = append(caps, detectOllamaModels()...)
	caps = append(caps, detectCPU()...)
	return caps
}

// ValidateTag checks whether a capability tag string is valid.
func ValidateTag(tag string) bool {
	return tag != "" && len(tag) <= 128 && validTagPattern.MatchString(tag)
}

// ParseTags splits a comma-separated string of capability tags and returns valid capabilities.
func ParseTags(tagStr string) []Capability {
	if tagStr == "" {
		return nil
	}
	var caps []Capability
	for _, raw := range strings.Split(tagStr, ",") {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if !ValidateTag(tag) {
			fmt.Printf("   - Warning: skipping invalid capability tag: %q\n", tag)
			continue
		}
		category := tag
		if idx := strings.Index(tag, ":"); idx > 0 {
			category = tag[:idx]
		}
		caps = append(caps, Capability{Tag: tag, Category: category})
	}
	return caps
}

// Tags returns just the tag strings from a slice of capabilities.
func Tags(caps []Capability) []string {
	tags := make([]string, len(caps))
	for i, c := range caps {
		tags[i] = c.Tag
	}
	return tags
}

// TagQueueName returns the Redis Streams queue name for a capability tag.
func TagQueueName(tag string) string {
	return fmt.Sprintf("jobs:v1:tag:%s", tag)
}

// ResolveQueues returns the list of queue names a node should subscribe to
// based on its capabilities. Always includes the base queue as fallback.
func ResolveQueues(caps []Capability, baseQueue string) []string {
	seen := make(map[string]bool)
	var queues []string

	for _, c := range caps {
		if c.Category == "cpu" {
			continue // cpu:general is handled by the base queue
		}
		q := TagQueueName(c.Tag)
		if !seen[q] {
			seen[q] = true
			queues = append(queues, q)
		}
	}

	// Always include the base queue as fallback
	if baseQueue != "" && !seen[baseQueue] {
		queues = append(queues, baseQueue)
	}

	return queues
}

func detectGPU() []Capability {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	var caps []Capability
	for idx, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) < 2 {
			continue
		}
		gpuName := strings.TrimSpace(parts[0])
		memoryMB := strings.TrimSpace(parts[1])
		gpuTag := NormalizeGPUName(gpuName)
		vramGB := NormalizeVRAM(memoryMB)

		// Aggregate tags (e.g., gpu:rtx4090, vram:24gb)
		if gpuTag != "" {
			caps = append(caps, Capability{Tag: fmt.Sprintf("gpu:%s", gpuTag), Category: "gpu", Description: gpuName})
		}
		if vramGB != "" {
			caps = append(caps, Capability{Tag: fmt.Sprintf("vram:%sgb", vramGB), Category: "vram", Description: fmt.Sprintf("%s MB VRAM", memoryMB)})
		}

		// Indexed tags for per-GPU targeting (e.g., gpu:0:rtx4090:24gb)
		if gpuTag != "" && vramGB != "" {
			indexedTag := fmt.Sprintf("gpu:%d:%s:%sgb", idx, gpuTag, vramGB)
			if ValidateTag(indexedTag) {
				caps = append(caps, Capability{
					Tag:         indexedTag,
					Category:    "gpu",
					Description: fmt.Sprintf("GPU %d: %s (%s MB VRAM)", idx, gpuName, memoryMB),
				})
			}
		}
	}
	return caps
}

func detectOllamaModels() []Capability {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ollama", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	var caps []Capability
	seen := make(map[string]bool)
	for i, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if i == 0 && strings.Contains(strings.ToLower(line), "name") {
			continue // skip header
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		modelFull := fields[0]
		modelName := modelFull
		if idx := strings.Index(modelFull, ":"); idx > 0 {
			modelName = modelFull[:idx]
		}
		modelTag := NormalizeModelName(modelName)
		if modelTag == "" || !ValidateTag("llm:"+modelTag) || seen[modelTag] {
			continue
		}
		seen[modelTag] = true
		caps = append(caps, Capability{Tag: fmt.Sprintf("llm:%s", modelTag), Category: "llm", Description: fmt.Sprintf("Ollama model: %s", modelFull)})
	}
	return caps
}

func detectCPU() []Capability {
	return []Capability{{Tag: "cpu:general", Category: "cpu", Description: "General CPU compute"}}
}

// NormalizeGPUName converts a full GPU name to a short tag-friendly identifier.
func NormalizeGPUName(name string) string {
	name = strings.ToLower(name)
	name = strings.TrimPrefix(name, "nvidia ")
	name = strings.TrimPrefix(name, "geforce ")
	name = strings.TrimPrefix(name, "tesla ")
	patterns := []struct {
		prefix string
		re     *regexp.Regexp
	}{
		{"rtx", regexp.MustCompile(`rtx\s*(\d{4}\s*(?:ti|super)?)`)},
		{"gtx", regexp.MustCompile(`gtx\s*(\d{3,4}\s*(?:ti|super)?)`)},
		{"", regexp.MustCompile(`\b([ahvl]\d{2,3})\b`)},
	}
	for _, p := range patterns {
		if matches := p.re.FindStringSubmatch(name); len(matches) > 1 {
			tag := p.prefix + strings.ReplaceAll(strings.TrimSpace(matches[1]), " ", "")
			return strings.Map(func(r rune) rune {
				if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
					return r
				}
				return -1
			}, tag)
		}
	}
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r == ' ' {
			return '-'
		}
		return -1
	}, name)
	for strings.Contains(cleaned, "--") {
		cleaned = strings.ReplaceAll(cleaned, "--", "-")
	}
	return strings.Trim(cleaned, "-")
}

// NormalizeVRAM converts a memory value in MB to a rounded GB string.
func NormalizeVRAM(memoryMB string) string {
	memoryMB = strings.TrimSpace(memoryMB)
	if memoryMB == "" {
		return ""
	}
	var mb float64
	if _, err := fmt.Sscanf(memoryMB, "%f", &mb); err != nil || mb <= 0 {
		return ""
	}
	rounded := int(math.Round(mb / 1024.0))
	if rounded <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", rounded)
}

// NormalizeModelName converts an Ollama model name to a tag-friendly format.
func NormalizeModelName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, " ", "-")
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			return r
		}
		return -1
	}, name)
}
