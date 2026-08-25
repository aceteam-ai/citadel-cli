package jobs

import "testing"

func TestParseModelCachePullPatterns(t *testing.T) {
	t.Run("absent fields return nil, nil (backward compat)", func(t *testing.T) {
		allow, ignore := parseModelCachePullPatterns(map[string]string{})
		if allow != nil || ignore != nil {
			t.Fatalf("got allow=%v ignore=%v, want nil, nil for an absent-field payload", allow, ignore)
		}
	})

	t.Run("JSON array form", func(t *testing.T) {
		payload := map[string]string{
			"allow_patterns":  `["transformer/*","vae/*"]`,
			"ignore_patterns": `["*.bin"]`,
		}
		allow, ignore := parseModelCachePullPatterns(payload)
		wantAllow := []string{"transformer/*", "vae/*"}
		wantIgnore := []string{"*.bin"}
		if !equalStrs(allow, wantAllow) {
			t.Errorf("allow = %v, want %v", allow, wantAllow)
		}
		if !equalStrs(ignore, wantIgnore) {
			t.Errorf("ignore = %v, want %v", ignore, wantIgnore)
		}
	})

	t.Run("comma-separated fallback", func(t *testing.T) {
		payload := map[string]string{
			"allow_patterns": "transformer/*, vae/* ,text_encoder/*",
		}
		allow, _ := parseModelCachePullPatterns(payload)
		want := []string{"transformer/*", "vae/*", "text_encoder/*"}
		if !equalStrs(allow, want) {
			t.Errorf("allow = %v, want %v", allow, want)
		}
	})

	t.Run("empty string field returns nil", func(t *testing.T) {
		allow, ignore := parseModelCachePullPatterns(map[string]string{
			"allow_patterns":  "",
			"ignore_patterns": "   ",
		})
		if allow != nil || ignore != nil {
			t.Errorf("got allow=%v ignore=%v, want nil, nil for blank fields", allow, ignore)
		}
	})
}

func TestMatchGlobFnmatchSemantics(t *testing.T) {
	// * must match across "/" (Python fnmatch semantics, NOT filepath.Match
	// shell-glob semantics) so our size estimate agrees with what
	// huggingface_hub's own --include/--exclude actually download.
	if !matchGlob("transformer/sub/model.safetensors", "transformer/*") {
		t.Error("expected transformer/* to match a nested path under transformer/ (fnmatch semantics)")
	}
	if !matchGlob("vae/config.json", "vae/*") {
		t.Error("expected vae/* to match vae/config.json")
	}
	if matchGlob("text_encoder/model.bin", "vae/*") {
		t.Error("vae/* must not match a file under text_encoder/")
	}
	if !matchGlob("model_index.json", "*.json") {
		t.Error("expected *.json to match a root-level json file")
	}
	if !matchGlob("a.safetensors", "?.safetensors") {
		t.Error("expected ? to match a single character")
	}
}

func TestPatternsInclude(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		allow  []string
		ignore []string
		want   bool
	}{
		{"no patterns includes everything", "root.safetensors", nil, nil, true},
		{"ignore wins over no allow list", "root.safetensors", nil, []string{"*.safetensors"}, false},
		{"allow list requires a positive match", "transformer/config.json", []string{"transformer/*"}, nil, true},
		{"allow list excludes non-matching files", "ltx-video-13b-fp32.safetensors", []string{"transformer/*", "vae/*"}, nil, false},
		{"ignore overrides an allow match", "transformer/big.bin", []string{"transformer/*"}, []string{"*.bin"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := patternsInclude(tt.path, tt.allow, tt.ignore); got != tt.want {
				t.Errorf("patternsInclude(%q, %v, %v) = %v, want %v", tt.path, tt.allow, tt.ignore, got, tt.want)
			}
		})
	}
}

// TestDeriveDiffusersAllowPatterns pins #828 part 3: a repo carrying the
// diffusers pipeline subfolders alongside MULTIPLE sibling top-level
// checkpoints (the exact LTX-Video shape) gets a default allow-list; anything
// short of that shape is left alone (nil), falling back to an unfiltered pull.
func TestDeriveDiffusersAllowPatterns(t *testing.T) {
	ltxVideoShape := []hfTreeEntry{
		{Type: "file", Path: "ltx-video-2b-v0.9.safetensors", Size: 1},
		{Type: "file", Path: "ltx-video-2b-v0.9.1.safetensors", Size: 1},
		{Type: "file", Path: "ltx-video-2b-v0.9.5.safetensors", Size: 1},
		{Type: "file", Path: "model_index.json", Size: 1},
		{Type: "file", Path: "transformer/config.json", Size: 1},
		{Type: "file", Path: "transformer/diffusion_pytorch_model.safetensors", Size: 1},
		{Type: "file", Path: "vae/config.json", Size: 1},
		{Type: "file", Path: "vae/diffusion_pytorch_model.safetensors", Size: 1},
		{Type: "file", Path: "text_encoder/config.json", Size: 1},
		{Type: "file", Path: "text_encoder/model.safetensors", Size: 1},
		{Type: "file", Path: "tokenizer/tokenizer_config.json", Size: 1},
		{Type: "file", Path: "scheduler/scheduler_config.json", Size: 1},
	}
	got := deriveDiffusersAllowPatterns(ltxVideoShape)
	if got == nil {
		t.Fatal("expected a non-nil default allow list for the LTX-Video-shaped repo")
	}
	want := map[string]bool{
		"transformer/*": true, "vae/*": true, "text_encoder/*": true,
		"tokenizer/*": true, "scheduler/*": true, "*.json": true, "*.txt": true,
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected pattern %q in derived allow list", p)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Errorf("missing expected patterns: %v", want)
	}

	t.Run("single checkpoint, no siblings -> nil (ordinary small diffusers repo)", func(t *testing.T) {
		entries := []hfTreeEntry{
			{Type: "file", Path: "model.safetensors", Size: 1},
			{Type: "file", Path: "transformer/config.json", Size: 1},
			{Type: "file", Path: "vae/config.json", Size: 1},
			{Type: "file", Path: "text_encoder/config.json", Size: 1},
		}
		if got := deriveDiffusersAllowPatterns(entries); got != nil {
			t.Errorf("expected nil for a single-checkpoint repo, got %v", got)
		}
	})

	t.Run("multiple root checkpoints but not diffusers-shaped -> nil", func(t *testing.T) {
		entries := []hfTreeEntry{
			{Type: "file", Path: "model-fp16.safetensors", Size: 1},
			{Type: "file", Path: "model-fp32.safetensors", Size: 1},
			{Type: "file", Path: "README.md", Size: 1},
		}
		if got := deriveDiffusersAllowPatterns(entries); got != nil {
			t.Errorf("expected nil when there is no diffusers subfolder layout, got %v", got)
		}
	})

	t.Run("only two of five diffusers dirs present -> nil (not enough evidence)", func(t *testing.T) {
		entries := []hfTreeEntry{
			{Type: "file", Path: "a.safetensors", Size: 1},
			{Type: "file", Path: "b.safetensors", Size: 1},
			{Type: "file", Path: "transformer/config.json", Size: 1},
			{Type: "file", Path: "vae/config.json", Size: 1},
		}
		if got := deriveDiffusersAllowPatterns(entries); got != nil {
			t.Errorf("expected nil with only 2 of 5 diffusers dirs present, got %v", got)
		}
	})
}
