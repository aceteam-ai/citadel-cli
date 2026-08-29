package jobs

import (
	"context"
	"encoding/json"
	"errors"
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

// TestModelCachePull_EngineTokenNormalization pins citadel#545: the backend's
// provisioning templates can emit "llama.cpp" / "llama-cpp" / "llama_cpp"
// (the upstream project's own spelling) where the node's internal engine name
// is "llamacpp". A rejected pull here means the whole deploy fails even though
// the compose/service name resolves fine everywhere else.
func TestModelCachePull_EngineTokenNormalization(t *testing.T) {
	// citadel#840 review WANT: this test drives the real Execute -> pullHuggingFace
	// path, which since #828/#840 runs a disk-space preflight that fetches live
	// HF repo metadata (hfRepoTreeFn) before ever reaching the download step
	// this test actually cares about. Left un-injected, every subtest below made
	// a real network call to huggingface.co (up to hfMetadataTimeout each) just
	// to 401/404 on a fake org -- an unrelated, un-hermetic dependency this test
	// never intended to take on. Fail the metadata fetch immediately so
	// runDiskPreflight takes its documented fail-open path (proceeds unchanged)
	// and this test again only exercises the engine-token-normalization
	// assertion it's named for.
	origTree := hfRepoTreeFn
	hfRepoTreeFn = func(ctx context.Context, repo string) ([]hfTreeEntry, error) {
		return nil, errors.New("network access disabled in TestModelCachePull_EngineTokenNormalization")
	}
	t.Cleanup(func() { hfRepoTreeFn = origTree })

	// citadel#682 review: since pullHuggingFace now injects HF_HOME (when the
	// operator hasn't set one) so the subprocess writes to the canonical
	// citadel-cache/huggingface dir, a real `hf` binary on the test machine
	// would otherwise touch that machine's actual cache directory even though
	// the repo is fake and the download 404s before writing anything. Pin
	// HF_HOME to a throwaway tempdir so this test's subprocess -- and this
	// test's use of a real `hf`/`huggingface-cli` binary if one happens to be
	// on PATH -- never resolves to a real, shared cache location.
	t.Setenv("HF_HOME", t.TempDir())

	// citadel#906: llamacpp no longer routes through pullHuggingFace (and
	// therefore no longer respects HF_HOME above) -- it downloads via
	// --local-dir directly into llamaCppCacheDir(). Redirect that too, for
	// the identical reason: a real `hf`/`huggingface-cli` binary on the test
	// machine must never touch this machine's actual ~/citadel-cache/llamacpp.
	origLlamaCppCacheDirFn := llamaCppCacheDirFn
	llamaCppDir := t.TempDir()
	llamaCppCacheDirFn = func() string { return llamaCppDir }
	t.Cleanup(func() { llamaCppCacheDirFn = origLlamaCppCacheDirFn })

	tests := []struct {
		name   string
		engine string
	}{
		{"dot form", "llama.cpp"},
		{"hyphen form", "llama-cpp"},
		{"underscore form", "llama_cpp"},
		{"uppercase dot form", "LLAMA.CPP"},
		{"canonical form still works", "llamacpp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &ModelCachePullHandler{}
			_, err := h.Execute(JobContext{}, makeJob(map[string]string{
				// A deliberately nonexistent repo so a real `hf` CLI (if present
				// on the test machine) fails fast on a 404 instead of actually
				// downloading gigabytes of GGUF weights.
				"model_name": "citadel-test-org/nonexistent-model-xyz-545",
				"engine":     tt.engine,
			}))
			// The assertion is that the pull fails for a download/no-op reason,
			// not because the engine token was rejected as unsupported.
			if err == nil {
				t.Fatalf("expected the download to fail for a nonexistent repo, got success")
			}
			if strings.Contains(err.Error(), "unsupported engine") {
				t.Errorf("engine %q should normalize to llamacpp, got: %v", tt.engine, err)
			}
		})
	}
}

// TestModelCachePull_DiffusersIsSelfProvisionedNoOp pins that "diffusers" (the
// other token citadel#545 flagged as rejected) is a no-op success: the
// diffusers compose pins its model and downloads weights itself on first
// start, so MODEL_CACHE_PULL has nothing to fetch.
func TestModelCachePull_DiffusersIsSelfProvisionedNoOp(t *testing.T) {
	h := &ModelCachePullHandler{}
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"model_name": "stabilityai/stable-diffusion-3.5-medium",
		"engine":     "diffusers",
	}))
	if err != nil {
		t.Fatalf("expected diffusers pull to be a no-op success, got error: %v", err)
	}
	var result modelCachePullResult
	if jsonErr := json.Unmarshal(out, &result); jsonErr != nil {
		t.Fatalf("failed to unmarshal result: %v", jsonErr)
	}
	if result.Status != "skipped" {
		t.Errorf("status = %q, want 'skipped'", result.Status)
	}
	if result.Engine != "diffusers" {
		t.Errorf("engine = %q, want 'diffusers'", result.Engine)
	}
}

// --- normalizeEngineToken tests ---

func TestNormalizeEngineToken(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"llama.cpp", "llamacpp"},
		{"llama-cpp", "llamacpp"},
		{"llama_cpp", "llamacpp"},
		{"llamacpp", "llamacpp"},
		{"vllm", "vllm"},
		{"ollama", "ollama"},
		{"diffusers", "diffusers"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeEngineToken(tt.in); got != tt.want {
				t.Errorf("normalizeEngineToken(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
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
	cmd := BuildHuggingFaceDownloadCommand("hf", "meta-llama/Llama-2-7b-chat-hf")
	args := cmd.Args
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d: %v", len(args), args)
	}
	if args[0] != "hf" {
		t.Errorf("args[0] = %q, want 'hf'", args[0])
	}
	if args[1] != "download" {
		t.Errorf("args[1] = %q, want 'download'", args[1])
	}
	if args[2] != "meta-llama/Llama-2-7b-chat-hf" {
		t.Errorf("args[2] = %q, want 'meta-llama/Llama-2-7b-chat-hf'", args[2])
	}
	// A repo pull must NOT pass --local-dir (it lands in the HF hub cache).
	for _, a := range args {
		if a == "--local-dir" {
			t.Errorf("repo pull should not use --local-dir, got %v", args)
		}
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
