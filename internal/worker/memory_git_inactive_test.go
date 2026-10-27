package worker

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/memorygit"
)

func TestMemoryGitV1RemainsUnregistered(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{WorkspaceDir: t.TempDir(), ShellEnabled: func() bool { return enabled }})
		if anyHandles(handlers, memorygit.JobType) {
			t.Fatal("inactive source was registered or advertised")
		}
	}
}
