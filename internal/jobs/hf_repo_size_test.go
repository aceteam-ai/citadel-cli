package jobs

import "testing"

func TestSumFilteredSize(t *testing.T) {
	entries := []hfTreeEntry{
		{Type: "file", Path: "transformer/model.safetensors", Size: 10 << 30},
		{Type: "file", Path: "vae/model.safetensors", Size: 2 << 30},
		{Type: "file", Path: "ltx-video-fp32.safetensors", Size: 100 << 30},
		{Type: "file", Path: "ltx-video-fp16.safetensors", Size: 50 << 30},
		{Type: "directory", Path: "transformer", Size: 0}, // directories must be skipped
	}

	t.Run("no filter sums everything", func(t *testing.T) {
		got := sumFilteredSize(entries, nil, nil)
		want := int64(10<<30 + 2<<30 + 100<<30 + 50<<30)
		if got != want {
			t.Errorf("sumFilteredSize(no filter) = %d, want %d", got, want)
		}
	})

	t.Run("allow-list restricts to the diffusers subfolders (the #828 fix)", func(t *testing.T) {
		got := sumFilteredSize(entries, []string{"transformer/*", "vae/*"}, nil)
		want := int64(10<<30 + 2<<30)
		if got != want {
			t.Errorf("sumFilteredSize(diffusers-only) = %d (%s), want %d (%s) -- the whole point is NOT summing the 150GB of sibling checkpoints",
				got, humanBytes(got), want, humanBytes(want))
		}
	})

	t.Run("LFS fallback size is used when top-level size is 0", func(t *testing.T) {
		lfsEntries := []hfTreeEntry{
			{Type: "file", Path: "model.safetensors", Size: 0, LFS: &hfLFSInfo{Size: 5 << 30}},
		}
		got := sumFilteredSize(lfsEntries, nil, nil)
		if got != 5<<30 {
			t.Errorf("sumFilteredSize with only LFS size set = %d, want %d", got, int64(5<<30))
		}
	})
}
