package jobs

import (
	"strings"
	"testing"
)

// TestBuildBonsaiDownloadCommand pins the exact huggingface-cli invocation for a
// bonsai MODEL_CACHE_PULL: it must download the single Bonsai-27B-Q1_0.gguf file
// from prism-ml/Bonsai-27B-gguf (NOT the whole repo, which also carries a ~53GB
// F16 and a drafter GGUF) into the fixed --local-dir the compose mounts.
func TestBuildBonsaiDownloadCommand(t *testing.T) {
	localDir := "/home/tester/citadel-cache/bonsai"
	cmd := BuildBonsaiDownloadCommand(localDir)

	args := cmd.Args
	if len(args) < 6 {
		t.Fatalf("expected huggingface-cli download command with local-dir, got %v", args)
	}
	if !strings.Contains(args[0], "huggingface-cli") {
		t.Errorf("expected huggingface-cli, got %q", args[0])
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"download", "prism-ml/Bonsai-27B-gguf", "Bonsai-27B-Q1_0.gguf", "--local-dir", localDir} {
		if !strings.Contains(joined, want) {
			t.Errorf("bonsai download command missing %q; got: %s", want, joined)
		}
	}
	// Must be a single-file pull: the repo id must be immediately followed by the
	// specific GGUF filename (not a bare repo download).
	repoIdx, fileIdx := -1, -1
	for i, a := range args {
		switch a {
		case "prism-ml/Bonsai-27B-gguf":
			repoIdx = i
		case "Bonsai-27B-Q1_0.gguf":
			fileIdx = i
		}
	}
	if repoIdx < 0 || fileIdx != repoIdx+1 {
		t.Errorf("expected the GGUF filename to follow the repo id (single-file pull); args=%v", args)
	}
}

// TestBonsaiCacheDirMatchesComposeMount guards the pull-serve coherence
// contract: the download dir must be ~/citadel-cache/bonsai, exactly what
// services/compose/bonsai.yml mounts at /models. If these drift, the served
// path /models/Bonsai-27B-Q1_0.gguf will not exist.
func TestBonsaiCacheDirMatchesComposeMount(t *testing.T) {
	dir := bonsaiCacheDir()
	if !strings.HasSuffix(dir, "citadel-cache/bonsai") {
		t.Errorf("bonsaiCacheDir() = %q, want it to end with citadel-cache/bonsai (the compose mount)", dir)
	}
}
