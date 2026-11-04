package agentquota

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates the fake policy fixtures in both local and normal CI runs.
// Production model code remains free of host-state or environment adapters.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "citadel-agentquota-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot isolate agentquota tests")
		os.Exit(1)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		path := filepath.Join(root, key)
		if err := os.MkdirAll(path, 0700); err != nil {
			fmt.Fprintln(os.Stderr, "cannot create isolated agentquota directory for", key)
			os.RemoveAll(root)
			os.Exit(1)
		}
		if err := os.Setenv(key, path); err != nil {
			fmt.Fprintln(os.Stderr, "cannot set isolated agentquota environment for", key)
			os.RemoveAll(root)
			os.Exit(1)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(root); err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}
