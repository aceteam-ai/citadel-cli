package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// --- MODEL_CACHE_PULL payload parsing tests ---

func TestModelCachePull_MissingModelName(t *testing.T) {
	h := &ModelCachePullHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"engine": "ollama",
	}))
	if err == nil {
		t.Fatal("expected error for missing model_name, got nil")
	}
	if !strings.Contains(err.Error(), "model_name") {
		t.Errorf("error should mention model_name, got: %v", err)
	}
}

func TestModelCachePull_EmptyModelName(t *testing.T) {
	h := &ModelCachePullHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "",
		"engine":     "ollama",
	}))
	if err == nil {
		t.Fatal("expected error for empty model_name, got nil")
	}
}

func TestModelCachePull_MissingEngine(t *testing.T) {
	h := &ModelCachePullHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
	}))
	if err == nil {
		t.Fatal("expected error for missing engine, got nil")
	}
	if !strings.Contains(err.Error(), "engine") {
		t.Errorf("error should mention engine, got: %v", err)
	}
}

func TestModelCachePull_EmptyEngine(t *testing.T) {
	h := &ModelCachePullHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
		"engine":     "",
	}))
	if err == nil {
		t.Fatal("expected error for empty engine, got nil")
	}
}

func TestModelCachePull_UnsupportedEngine(t *testing.T) {
	h := &ModelCachePullHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
		"engine":     "tensorrt",
	}))
	if err == nil {
		t.Fatal("expected error for unsupported engine, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported engine") {
		t.Errorf("error should mention unsupported engine, got: %v", err)
	}
}

func TestModelCachePull_EngineNormalization(t *testing.T) {
	h := &ModelCachePullHandler{}
	// "OLLAMA" should be normalized to "ollama" and attempt the pull.
	// It will fail because ollama isn't installed in the test env,
	// but the error should be about the pull failing, not about
	// an unsupported engine.
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
		"engine":     "OLLAMA",
	}))
	if err == nil {
		// If ollama happens to be installed, that's fine too.
		return
	}
	if strings.Contains(err.Error(), "unsupported engine") {
		t.Errorf("OLLAMA should be normalized to ollama, got: %v", err)
	}
}

// --- MODEL_CACHE_EVICT payload parsing tests ---

func TestModelCacheEvict_MissingModelName(t *testing.T) {
	h := &ModelCacheEvictHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"engine": "ollama",
	}))
	if err == nil {
		t.Fatal("expected error for missing model_name, got nil")
	}
	if !strings.Contains(err.Error(), "model_name") {
		t.Errorf("error should mention model_name, got: %v", err)
	}
}

func TestModelCacheEvict_EmptyModelName(t *testing.T) {
	h := &ModelCacheEvictHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "",
		"engine":     "ollama",
	}))
	if err == nil {
		t.Fatal("expected error for empty model_name, got nil")
	}
}

func TestModelCacheEvict_MissingEngine(t *testing.T) {
	h := &ModelCacheEvictHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
	}))
	if err == nil {
		t.Fatal("expected error for missing engine, got nil")
	}
	if !strings.Contains(err.Error(), "engine") {
		t.Errorf("error should mention engine, got: %v", err)
	}
}

func TestModelCacheEvict_EmptyEngine(t *testing.T) {
	h := &ModelCacheEvictHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
		"engine":     "",
	}))
	if err == nil {
		t.Fatal("expected error for empty engine, got nil")
	}
}

func TestModelCacheEvict_UnsupportedEngine(t *testing.T) {
	h := &ModelCacheEvictHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
		"engine":     "tensorrt",
	}))
	if err == nil {
		t.Fatal("expected error for unsupported engine, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported engine") {
		t.Errorf("error should mention unsupported engine, got: %v", err)
	}
}

func TestModelCacheEvict_EngineNormalization(t *testing.T) {
	h := &ModelCacheEvictHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "llama3.2",
		"engine":     "OLLAMA",
	}))
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "unsupported engine") {
		t.Errorf("OLLAMA should be normalized to ollama, got: %v", err)
	}
}

func TestModelCacheEvict_HuggingFaceNotCached(t *testing.T) {
	h := &ModelCacheEvictHandler{}
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "nonexistent-org/nonexistent-model-xyz",
		"engine":     "vllm",
	}))
	if err == nil {
		t.Fatal("expected error for model not in cache, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
}

// --- Command construction tests ---

func TestBuildOllamaPullCommand(t *testing.T) {
	cmd := BuildOllamaPullCommand("llama3.2:7b")
	args := cmd.Args
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[1] != "pull" {
		t.Errorf("args[1] = %q, want 'pull'", args[1])
	}
	if args[2] != "llama3.2:7b" {
		t.Errorf("args[2] = %q, want 'llama3.2:7b'", args[2])
	}
}

func TestBuildOllamaRmCommand(t *testing.T) {
	cmd := BuildOllamaRmCommand("llama3.2:7b")
	args := cmd.Args
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[1] != "rm" {
		t.Errorf("args[1] = %q, want 'rm'", args[1])
	}
	if args[2] != "llama3.2:7b" {
		t.Errorf("args[2] = %q, want 'llama3.2:7b'", args[2])
	}
}

func TestBuildHuggingFaceDownloadCommand(t *testing.T) {
	cmd := BuildHuggingFaceDownloadCommand("meta-llama/Llama-2-7b-chat-hf")
	args := cmd.Args
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[1] != "download" {
		t.Errorf("args[1] = %q, want 'download'", args[1])
	}
	if args[2] != "meta-llama/Llama-2-7b-chat-hf" {
		t.Errorf("args[2] = %q, want 'meta-llama/Llama-2-7b-chat-hf'", args[2])
	}
}

// --- parseHumanSize tests ---

func TestParseHumanSize(t *testing.T) {
	tests := []struct {
		numStr string
		unit   string
		want   int64
	}{
		{"4.1", "GB", 4402341478},  // ~4.1 * 1024^3
		{"512", "MB", 536870912},   // 512 * 1024^2
		{"1", "TB", 1099511627776}, // 1 * 1024^4
		{"100", "KB", 102400},      // 100 * 1024
		{"42", "B", 42},
		{"bad", "GB", 0},
		{"4.1", "XB", 0},
	}
	for _, tt := range tests {
		t.Run(tt.numStr+"_"+tt.unit, func(t *testing.T) {
			got := parseHumanSize(tt.numStr, tt.unit)
			if got != tt.want {
				t.Errorf("parseHumanSize(%q, %q) = %d, want %d", tt.numStr, tt.unit, got, tt.want)
			}
		})
	}
}

// --- hfCacheDir tests ---

func TestHfCacheDir_NonexistentModel(t *testing.T) {
	dir := hfCacheDir("nonexistent-org/nonexistent-model-xyz")
	if dir != "" {
		t.Errorf("expected empty string for nonexistent model, got %q", dir)
	}
}

// --- Result JSON structure tests ---

func TestModelCachePullResult_JSONFields(t *testing.T) {
	result := modelCachePullResult{
		Status:    "cached",
		ModelName: "llama3.2",
		SizeBytes: 4402341478,
		Engine:    "ollama",
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}

	if parsed["status"] != "cached" {
		t.Errorf("status = %v, want 'cached'", parsed["status"])
	}
	if parsed["model_name"] != "llama3.2" {
		t.Errorf("model_name = %v, want 'llama3.2'", parsed["model_name"])
	}
	if parsed["engine"] != "ollama" {
		t.Errorf("engine = %v, want 'ollama'", parsed["engine"])
	}
	if _, ok := parsed["size_bytes"]; !ok {
		t.Error("missing size_bytes field")
	}
}

func TestModelCacheEvictResult_JSONFields(t *testing.T) {
	result := modelCacheEvictResult{
		Status:    "evicted",
		ModelName: "llama3.2",
		Engine:    "vllm",
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}

	if parsed["status"] != "evicted" {
		t.Errorf("status = %v, want 'evicted'", parsed["status"])
	}
	if parsed["model_name"] != "llama3.2" {
		t.Errorf("model_name = %v, want 'llama3.2'", parsed["model_name"])
	}
	if parsed["engine"] != "vllm" {
		t.Errorf("engine = %v, want 'vllm'", parsed["engine"])
	}
}

// --- Interface compliance ---

func TestModelCachePullHandler_ImplementsJobHandler(t *testing.T) {
	var _ JobHandler = (*ModelCachePullHandler)(nil)
}

func TestModelCacheEvictHandler_ImplementsJobHandler(t *testing.T) {
	var _ JobHandler = (*ModelCacheEvictHandler)(nil)
}

// makeModelCacheJob is a helper for creating model cache jobs.
func makeModelCacheJob(jobType, modelName, engine string) *nexus.Job {
	return &nexus.Job{
		ID:   "test-cache-1",
		Type: jobType,
		Payload: map[string]string{
			"model_name": modelName,
			"engine":     engine,
		},
	}
}
