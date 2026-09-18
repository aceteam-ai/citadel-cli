//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeEphemeralSystemExecCopiesToDurablePath(t *testing.T) {
	sourceFile, err := os.CreateTemp("/tmp", "citadel-test-*")
	if err != nil {
		t.Fatal(err)
	}
	source := sourceFile.Name()
	if _, err := sourceFile.Write([]byte("binary bytes")); err != nil {
		t.Fatal(err)
	}
	if err := sourceFile.Chmod(0o755); err != nil {
		t.Fatal(err)
	}
	if err := sourceFile.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(source) })
	original := durableSystemExecPath
	durableSystemExecPath = filepath.Join(t.TempDir(), "usr", "local", "bin", "citadel")
	t.Cleanup(func() { durableSystemExecPath = original })
	cfg, err := materializeEphemeralSystemExec(ServiceConfig{ExecPath: source})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecPath != durableSystemExecPath {
		t.Fatalf("ExecPath = %q, want %q", cfg.ExecPath, durableSystemExecPath)
	}
	got, err := os.ReadFile(cfg.ExecPath)
	if err != nil || string(got) != "binary bytes" {
		t.Fatalf("durable executable = %q, %v", got, err)
	}
}

func TestCopyExecutableAndEphemeralExecStart(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	destination := filepath.Join(dir, "nested", "citadel")
	if err := os.WriteFile(source, []byte("binary bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyExecutable(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "binary bytes" {
		t.Fatalf("copied executable = %q, %v", got, err)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("destination must be executable: %v", err)
	}
	path, location, ok := ephemeralExecStart("[Service]\nExecStart=/tmp/citadel work\n")
	if !ok || path != "/tmp/citadel" || location != "/tmp" {
		t.Fatalf("ephemeral ExecStart = %q, %q, %v", path, location, ok)
	}
	if _, _, ok := ephemeralExecStart("[Service]\nExecStart=/usr/local/bin/citadel work\n"); ok {
		t.Fatal("durable ExecStart must not be flagged")
	}
	if unsafe, _ := ephemeralExecPath("/var/tmp/citadel"); !unsafe {
		t.Fatal("/var/tmp must be ephemeral")
	}
	if unsafe, _ := ephemeralExecPath("/dev/shm/citadel"); !unsafe {
		t.Fatal("/dev/shm must be ephemeral")
	}
	if strings.Contains(destination, "/tmp/citadel") {
		t.Fatal("test destination must be independent of the ephemeral source")
	}
}
