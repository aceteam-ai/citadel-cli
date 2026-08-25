package cobrowseprofile

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestArchiveExcludesCacheAndRuntimeKeepsData verifies buildTar keeps durable
// profile state (cookies, sqlite -wal/-shm sidecars) and drops regenerable cache
// dirs and per-launch runtime artifacts. Mutation check: dropping the -wal exclude
// guard, or excluding Cookies, flips one of these assertions.
func TestArchiveExcludesCacheAndRuntimeKeepsData(t *testing.T) {
	src := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Default/Cookies", "keep-cookie")
	write("Default/Cookies-wal", "keep-wal")
	write("Default/Cookies-shm", "keep-shm")
	write("Default/Cache/blob", "drop-cache")
	write("Default/Code Cache/js/blob", "drop-code-cache")
	write("GPUCache/data", "drop-gpucache")
	write("SingletonLock", "drop-singleton")

	blob, err := buildTar(src)
	if err != nil {
		t.Fatalf("buildTar: %v", err)
	}

	dst := t.TempDir()
	if err := extractTar(blob, dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	mustExist := []string{"Default/Cookies", "Default/Cookies-wal", "Default/Cookies-shm"}
	for _, rel := range mustExist {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("durable file %q missing from archive: %v", rel, err)
		}
	}
	mustNotExist := []string{"Default/Cache/blob", "Default/Code Cache/js/blob", "GPUCache/data", "SingletonLock"}
	for _, rel := range mustNotExist {
		if _, err := os.Stat(filepath.Join(dst, rel)); !os.IsNotExist(err) {
			t.Errorf("ephemeral entry %q should have been excluded (err=%v)", rel, err)
		}
	}
}

// TestSafeJoinRejectsTraversal is the unit guard against zip-slip: an absolute path
// or a ".." escape must be rejected, a normal path accepted.
func TestSafeJoinRejectsTraversal(t *testing.T) {
	dst := "/tmp/dst"
	bad := []string{"../etc/passwd", "../../x", "/etc/passwd", "a/../../b"}
	for _, name := range bad {
		if _, err := safeJoin(dst, name); err == nil {
			t.Errorf("safeJoin accepted traversal path %q", name)
		}
	}
	good, err := safeJoin(dst, "Default/Cookies")
	if err != nil {
		t.Fatalf("safeJoin rejected a safe path: %v", err)
	}
	if good != filepath.Join(dst, "Default/Cookies") {
		t.Fatalf("safeJoin = %q, want %q", good, filepath.Join(dst, "Default/Cookies"))
	}
}

// TestExtractTarRejectsMaliciousEntry crafts a tar whose entry escapes the
// destination and confirms extraction refuses it rather than writing outside dst.
// Mutation check: removing the safeJoin call in extractTar lets this write escape
// and the test (which asserts an error AND no escaped file) fails.
func TestExtractTarRejectsMaliciousEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "../escaped.txt",
		Typeflag: tar.TypeReg,
		Mode:     0o600,
		Size:     3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("pwn")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	dst := filepath.Join(parent, "profile")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(buf.Bytes(), dst); err == nil {
		t.Fatal("extractTar accepted a path-traversal entry")
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal entry escaped the destination dir (err=%v)", err)
	}
}

// TestExtractTarSkipsSymlinks confirms a symlink entry is never written (defense in
// depth beyond safeJoin).
func TestExtractTarSkipsSymlinks(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := extractTar(buf.Bytes(), dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Fatal("symlink entry was written on extraction")
	}
}
