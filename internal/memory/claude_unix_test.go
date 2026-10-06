//go:build !windows

package memory

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestClaudeConfigMutation_RejectsFIFOWithoutOpening(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"mcp"}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("FIFO config was not rejected: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO was replaced: info=%v err=%v", info, err)
	}
}
