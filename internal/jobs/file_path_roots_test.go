package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateWithinRootsEmptyAllowlistRejects(t *testing.T) {
	// An empty allowlist must fail closed: even an existing absolute path is
	// rejected when nothing is authorized (the fresh-node default).
	existing := t.TempDir()
	if _, err := ValidateWithinRoots(nil, existing); err == nil {
		t.Fatal("empty allowlist must reject every path, even an existing one")
	}
}

func TestValidateWithinRootsInsideAndOutside(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	outside := t.TempDir()

	// A file under rootB is valid (it validates under the second root).
	inB := filepath.Join(rootB, "notes", "file.md")
	if err := os.MkdirAll(filepath.Dir(inB), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inB, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := ValidateWithinRoots([]string{rootA, rootB}, inB)
	if err != nil {
		t.Fatalf("path under an authorized root should validate: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("validated path %q should be absolute", got)
	}

	// A path under no authorized root is rejected.
	out := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(out, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWithinRoots([]string{rootA, rootB}, out); err == nil {
		t.Fatal("path outside every authorized root must be rejected")
	}
}

func TestValidateWithinRootsSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is unreliable without privileges on Windows")
	}
	root := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0644); err != nil {
		t.Fatal(err)
	}

	// A symlink INSIDE the authorized root pointing OUTSIDE it must not grant
	// access to the target: EvalSymlinks resolves the escape and the boundary
	// check rejects it. This is the core security property.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(secretDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	escaped := filepath.Join(link, "secret.txt") // /root/escape/secret.txt -> /secretDir/secret.txt
	if _, err := ValidateWithinRoots([]string{root}, escaped); err == nil {
		t.Fatal("symlink escaping the authorized root must be rejected")
	}
}
