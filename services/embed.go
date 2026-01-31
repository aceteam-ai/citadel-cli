// services/embed.go
package services

import (
	_ "embed"
	"sort"
)

//go:embed compose/ollama.yml
var OllamaCompose string

//go:embed compose/vllm.yml
var VLLMCompose string

//go:embed compose/llamacpp.yml
var LlamacppCompose string

//go:embed compose/lmstudio.yml
var LMStudioCompose string

//go:embed compose/gliner2.yml
var GlinerCompose string

// ServiceMap provides a lookup for pre-packaged service compose files.
var ServiceMap = map[string]string{
	"ollama":   OllamaCompose,
	"vllm":     VLLMCompose,
	"llamacpp": LlamacppCompose,
	"lmstudio": LMStudioCompose,
	"gliner2":  GlinerCompose,
}

// GetAvailableServices returns a sorted list of service names.
func GetAvailableServices() []string {
	keys := make([]string, 0, len(ServiceMap))
	for k := range ServiceMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
