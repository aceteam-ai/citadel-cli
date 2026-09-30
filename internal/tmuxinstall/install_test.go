package tmuxinstall

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/tmux"
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

	src := Source{URL: srv.URL, SHA256: sha256Hex(content), Version: "3.4", Format: formatRaw}
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
	src := Source{URL: srv.URL, SHA256: sha256Hex([]byte("something-else")), Version: "3.4", Format: formatRaw}
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
	src := Source{URL: srv.URL, SHA256: sha256Hex(archive), Version: "3.4", Format: formatTarGz}

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
	src := Source{URL: "https://example.invalid/tmux", SHA256: "", Version: "3.4", Format: formatRaw}
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
		versionSet := s.Version != ""
		if urlSet != sumSet || urlSet != versionSet {
			t.Errorf("%s: URL, SHA256, and Version must all be set or all empty (url=%v sum=%v version=%v)", key, urlSet, sumSet, versionSet)
		}
		if s.Note == "" {
			t.Errorf("%s: Note must explain provenance/gating", key)
		}
	}
}

func TestSourceVettedRejectsOldTmux(t *testing.T) {
	src := Source{
		URL:     "https://example.invalid/tmux",
		SHA256:  strings.Repeat("a", 64),
		Version: "2.5",
		Format:  formatRaw,
	}
	if src.vetted() {
		t.Fatalf("tmux %s source passed minimum %s", src.Version, tmux.MinimumVersion)
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

func TestEnsureRejectsExistingOldManagedBinary(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(dest, []byte("#!/bin/sh\necho 'tmux 2.5'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	installed, err := New(WithDestPath(dest)).Ensure()
	if installed || !errors.Is(err, tmux.ErrTmuxVersionUnsupported) {
		t.Fatalf("Ensure() = (%v, %v), want unsupported-version failure", installed, err)
	}
}
