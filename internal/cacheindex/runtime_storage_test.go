package cacheindex

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRuntimeStorageRoots(t *testing.T) {
	tests := []struct {
		name, engine, home, xdg string
		want                    []string
	}{
		{"rootless podman default", "podman", "/home/citadel", "", []string{filepath.FromSlash("/home/citadel/.local/share/containers/storage")}},
		{"rootless podman XDG", "podman", "/home/citadel", "/data/citadel", []string{filepath.FromSlash("/data/citadel/containers/storage")}},
		{"relative XDG is ignored", "podman", "/home/citadel", "relative", []string{filepath.FromSlash("/home/citadel/.local/share/containers/storage")}},
		{"docker", "docker", "/home/citadel", "", []string{filepath.FromSlash("/var/lib/docker")}},
		{"unknown", "containerd", "/home/citadel", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runtimeStorageRoots(tt.engine, tt.home, tt.xdg); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("runtimeStorageRoots() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReconcileScanRecordsRuntimeStorage(t *testing.T) {
	root := t.TempDir()
	graphRoot := filepath.Join(t.TempDir(), "containers", "storage")
	if err := os.MkdirAll(filepath.Join(graphRoot, "overlay"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(graphRoot, "overlay", "layer"), []byte("layer-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(t.TempDir(), FileName)
	store := Open(indexPath, nil)
	if err := store.ReconcileScan(root, ScanOptions{RuntimeStorageRoots: []string{"/missing", graphRoot, graphRoot}}); err != nil {
		t.Fatal(err)
	}
	want := []RuntimeStorageScan{{Path: graphRoot, MeasuredBytes: int64(len("layer-data"))}}
	if got := store.Snapshot().Meta().RuntimeStorage; !reflect.DeepEqual(got, want) {
		t.Fatalf("RuntimeStorage = %+v, want %+v", got, want)
	}
	reloaded, err := Load(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Meta().RuntimeStorage; !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded RuntimeStorage = %+v, want %+v", got, want)
	}
}
