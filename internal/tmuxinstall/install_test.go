package tmuxinstall

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// sha256Hex returns the lowercase hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// makeTarGz returns a .tar.gz containing a single regular file named entryName
// with the given content.
func makeTarGz(t *testing.T, entryName string, content []byte) []byte {
	t.Helper()
	// Build via a temp file to keep it simple.
	f, err := os.CreateTemp(t.TempDir(), "tar-*.tgz")
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: entryName, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()
	buf, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

func TestInstallFromRaw_VerifiesAndInstalls(t *testing.T) {
	content := []byte("#!/bin/sh\necho fake-tmux\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin", "tmux")
	inst := New(WithHTTPClient(srv.Client()), WithDestPath(dest))

	src := Source{URL: srv.URL, SHA256: sha256Hex(content), Format: formatRaw}
	// Override the URL scheme guard: httptest TLS server uses https already.
	if err := inst.installFrom(src); err != nil {
		t.Fatalf("installFrom: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("installed content mismatch")
	}
	if !inst.AlreadyInstalled() {
		t.Fatal("AlreadyInstalled should be true after install")
	}
}

func TestInstallFrom_ChecksumMismatchRejected(t *testing.T) {
	content := []byte("real-tmux-bytes")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin", "tmux")
	inst := New(WithHTTPClient(srv.Client()), WithDestPath(dest))

	// Wrong checksum.
	src := Source{URL: srv.URL, SHA256: sha256Hex([]byte("something-else")), Format: formatRaw}
	if err := inst.installFrom(src); err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("binary must NOT be installed on checksum mismatch (stat err=%v)", err)
	}
}

func TestInstallFrom_TarGzExtraction(t *testing.T) {
	content := []byte("tmux-binary-contents")
	// Use the platform's expected entry name so the test is valid on Windows
	// runners too (where extractTarGz looks for "tmux.exe").
	archive := makeTarGz(t, tmuxBinaryName(), content)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin", "tmux")
	inst := New(WithHTTPClient(srv.Client()), WithDestPath(dest))
	src := Source{URL: srv.URL, SHA256: sha256Hex(archive), Format: formatTarGz}

	if err := inst.installFrom(src); err != nil {
		t.Fatalf("installFrom tar.gz: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("extracted content mismatch: %q", got)
	}
}

func TestInstallFrom_GatedSourceRefused(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin", "tmux")
	inst := New(WithDestPath(dest))
	// Empty SHA256 => gated.
	src := Source{URL: "https://example.invalid/tmux", SHA256: "", Format: formatRaw}
	if err := inst.installFrom(src); err == nil {
		t.Fatal("expected refusal to install a gated (unverified) source")
	}
}

func TestSourceTable_AllEntriesValid(t *testing.T) {
	// Sanity: every table entry either is fully gated (empty URL AND empty
	// checksum) or fully vetted (both set). A URL without a checksum would be a
	// supply-chain hazard; a checksum without a URL is meaningless.
	for key, s := range sources {
		urlSet := s.URL != ""
		sumSet := s.SHA256 != ""
		if urlSet != sumSet {
			t.Errorf("%s: URL and SHA256 must both be set or both empty (url=%v sum=%v)", key, urlSet, sumSet)
		}
		if s.Note == "" {
			t.Errorf("%s: Note must explain provenance/gating", key)
		}
	}
}

func TestAvailable_DefaultGated(t *testing.T) {
	// With the shipped (fully gated) table, no platform should report available.
	for key, s := range sources {
		if s.vetted() {
			t.Errorf("%s: expected gated source in shipped table, but it is vetted", key)
		}
	}
}
