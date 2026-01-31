package capabilities

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const detectionTimeout = 5 * time.Second

var validTagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:._-]*$`)

// Capability represents a node capability tag used for queue routing.
type Capability struct {
	Tag         string
	Category    string
	Description string
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
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
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
		if gpuTag := NormalizeGPUName(gpuName); gpuTag != "" {
			caps = append(caps, Capability{Tag: fmt.Sprintf("gpu:%s", gpuTag), Category: "gpu", Description: gpuName})
		}
		if vramGB := NormalizeVRAM(memoryMB); vramGB != "" {
			caps = append(caps, Capability{Tag: fmt.Sprintf("vram:%sgb", vramGB), Category: "vram", Description: fmt.Sprintf("%s MB VRAM", memoryMB)})
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
