// services/embed.go
package services

import _ "embed"

//go:embed compose/ollama.yml
var OllamaCompose string

////go:embed compose/vllm.yml
// var VLLMCompose string

// ServiceMap provides a lookup for pre-packaged service compose files.
var ServiceMap = map[string]string{
	"ollama": OllamaCompose,
	// "vllm":   VLLMCompose,
	// Add lmstudio, llamacpp etc. here as you create their compose files.
}
