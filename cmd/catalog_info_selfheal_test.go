package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// `catalog info` on a cold cache refreshes once and then resolves (citadel-cli#1758).
func TestCatalogInfoSelfHealsColdCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := 0
	restore := catalog.StubRefreshForTest(func() error {
		calls++
		dir := filepath.Join(catalog.GetCatalogPath(), "services", "meeting")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "service.yaml"), []byte("name: meeting\nversion: 1.0.0\n"), 0o644)
	})
	t.Cleanup(restore)
	_ = platform.ConfigDir

	if err := runCatalogInfo(nil, []string{"meeting"}); err != nil {
		t.Fatalf("runCatalogInfo: %v", err)
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}
