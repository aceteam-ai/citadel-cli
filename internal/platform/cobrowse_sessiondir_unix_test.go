//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePrivateSessionDir(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSessionDir(good); err != nil {
		t.Errorf("0700 own dir: %v", err)
	}
	loose := filepath.Join(root, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSessionDir(loose); err == nil {
		t.Error("group-readable dir must be refused")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateSessionDir(link); err == nil {
		t.Error("symlink must be refused")
	}
	if err := validatePrivateSessionDir(filepath.Join(root, "missing")); err == nil {
		t.Error("missing dir must be refused")
	}
}
