package cacheindex

import (
	"os"
	"path/filepath"
	"strings"
)

// RuntimeStorageRoots returns the container engine graph roots whose disk use
// belongs in node cache accounting. Rootless Podman stores its graph under the
// user's data directory; Docker retains its system graph root.
func RuntimeStorageRoots(engine string) []string {
	home, _ := os.UserHomeDir()
	return runtimeStorageRoots(engine, home, os.Getenv("XDG_DATA_HOME"))
}

func runtimeStorageRoots(engine, home, xdgDataHome string) []string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "podman":
		base := strings.TrimSpace(xdgDataHome)
		if base == "" || !filepath.IsAbs(base) {
			if home == "" {
				return nil
			}
			base = filepath.Join(home, ".local", "share")
		}
		return []string{filepath.Join(base, "containers", "storage")}
	case "docker":
		return []string{"/var/lib/docker"}
	default:
		return nil
	}
}
