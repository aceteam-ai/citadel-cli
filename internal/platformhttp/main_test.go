package platformhttp

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "citadel-platform-http-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot isolate platform HTTP tests")
		os.Exit(1)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		path := filepath.Join(root, key)
		if err := os.MkdirAll(path, 0700); err != nil {
			os.RemoveAll(root)
			os.Exit(1)
		}
		if err := os.Setenv(key, path); err != nil {
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
