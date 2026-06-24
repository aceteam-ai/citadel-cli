package tmuxinstall

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/tmux"
)

// maxArtifactBytes caps how much we will download for a single tmux artifact. A
// static tmux is a few MB; this guards against a misconfigured URL streaming an
// unbounded body into memory/disk.
const maxArtifactBytes = 64 << 20 // 64 MiB

// defaultTimeout bounds the whole download. Provisioning is best-effort and must
// never hang a node's startup.
const defaultTimeout = 60 * time.Second

// Installer provisions the managed tmux binary. The zero value is not usable;
// construct it with New.
type Installer struct {
	httpClient *http.Client
	// destPath is where the managed binary is written. Defaults to
	// tmux.ManagedBinaryPath(); overridable for tests.
	destPath string
	// now is injectable for tests; unused in production beyond defaults.
}

// Option configures an Installer.
type Option func(*Installer)

// WithHTTPClient overrides the HTTP client (e.g. in tests).
func WithHTTPClient(c *http.Client) Option {
	return func(i *Installer) { i.httpClient = c }
}

// WithDestPath overrides the install destination (e.g. in tests). Production
// callers should leave this unset so tmux.ManagedBinaryPath() is honored.
func WithDestPath(p string) Option {
	return func(i *Installer) { i.destPath = p }
}

// New constructs an Installer with sensible defaults.
func New(opts ...Option) *Installer {
	i := &Installer{
		httpClient: &http.Client{Timeout: defaultTimeout},
		destPath:   tmux.ManagedBinaryPath(),
	}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// DestPath returns where the managed tmux binary will be / is installed.
func (i *Installer) DestPath() string { return i.destPath }

// AlreadyInstalled reports whether a managed tmux binary already exists at the
// destination (a regular, non-directory file). It does not validate or execute
// it.
func (i *Installer) AlreadyInstalled() bool {
	info, err := os.Stat(i.destPath)
	return err == nil && !info.IsDir()
}

// Ensure installs the managed tmux binary for the current platform if it is not
// already present and a vetted artifact exists. It returns:
//   - (true, nil)  when a binary is now present (installed or already there)
//   - (false, nil) when the platform is gated/unsupported (no error: callers
//     should fall back to a bare shell)
//   - (false, err) when an install was attempted and failed
//
// Ensure never executes the downloaded binary.
func (i *Installer) Ensure() (installed bool, err error) {
	if i.AlreadyInstalled() {
		return true, nil
	}
	if !Available() {
		// Gated/unsupported platform: not an error condition for best-effort
		// callers; the terminal server falls back to a bare shell.
		return false, nil
	}
	if err := i.Install(); err != nil {
		return false, err
	}
	return true, nil
}

// Install downloads, checksum-verifies, and installs the managed tmux binary for
// the current platform, overwriting any existing managed binary. It returns a
// descriptive error for gated/unsupported platforms (use Ensure for best-effort
// semantics). The downloaded binary is never executed as part of verification.
func (i *Installer) Install() error {
	src, ok := CurrentSource()
	if !ok || !src.vetted() {
		return gatedError(runtime.GOOS, runtime.GOARCH)
	}
	return i.installFrom(src)
}

// installFrom performs the download -> verify -> extract -> place pipeline for a
// concrete (vetted) source. Split out for testability.
func (i *Installer) installFrom(src Source) error {
	if !src.vetted() {
		return fmt.Errorf("refusing to install tmux: source is not checksum-verified (gated)")
	}

	destDir := filepath.Dir(i.destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create managed bin dir %s: %w", destDir, err)
	}

	// Download the artifact to a temp file in the destination directory (same
	// filesystem, so the final rename is atomic).
	tmpArtifact, err := os.CreateTemp(destDir, ".tmux-artifact-*")
	if err != nil {
		return fmt.Errorf("create temp artifact: %w", err)
	}
	artifactPath := tmpArtifact.Name()
	tmpArtifact.Close()
	defer os.Remove(artifactPath)

	if err := i.download(src.URL, artifactPath); err != nil {
		return err
	}

	// Verify the checksum of the DOWNLOADED artifact BEFORE extracting or
	// installing anything. This is the trust boundary.
	if err := verifyChecksum(artifactPath, src.SHA256); err != nil {
		return err
	}

	// Extract the tmux binary into a temp file next to the destination, then
	// atomically rename it into place. We never run the binary to "verify" it.
	tmpBin, err := os.CreateTemp(destDir, ".tmux-bin-*")
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	tmpBinPath := tmpBin.Name()
	tmpBin.Close()
	defer os.Remove(tmpBinPath)

	if err := extractBinary(artifactPath, tmpBinPath, src.Format); err != nil {
		return err
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpBinPath, 0o755); err != nil {
			return fmt.Errorf("set executable bit: %w", err)
		}
	}

	if err := os.Rename(tmpBinPath, i.destPath); err != nil {
		return fmt.Errorf("install tmux to %s: %w", i.destPath, err)
	}
	return nil
}

// download streams url to destPath over HTTPS, enforcing a size cap.
func (i *Installer) download(url, destPath string) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("refusing to download tmux from non-HTTPS URL: %q", url)
	}
	resp, err := i.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("download tmux artifact: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download tmux artifact: unexpected status %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create artifact file: %w", err)
	}
	defer out.Close()

	// LimitReader guards against an unbounded body. +1 lets us detect overflow.
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	if n > maxArtifactBytes {
		return fmt.Errorf("tmux artifact exceeds size limit of %d bytes", maxArtifactBytes)
	}
	return nil
}

// verifyChecksum confirms the SHA-256 of the file at path equals expected
// (lowercase hex). It returns a clear error on mismatch.
func verifyChecksum(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open artifact for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash artifact: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("tmux artifact checksum mismatch: expected %s, got %s", expected, got)
	}
	return nil
}

// tmuxBinaryName is the name of the tmux executable inside an archive for the
// current platform.
func tmuxBinaryName() string {
	if runtime.GOOS == "windows" {
		return "tmux.exe"
	}
	return "tmux"
}

// extractBinary writes the tmux executable from the artifact at srcPath to
// dstPath, according to format.
func extractBinary(srcPath, dstPath string, format archiveFormat) error {
	switch format {
	case formatRaw:
		return copyFile(srcPath, dstPath)
	case formatGzip:
		return extractGzip(srcPath, dstPath)
	case formatTarGz:
		return extractTarGz(srcPath, dstPath)
	case formatZip:
		return extractZip(srcPath, dstPath)
	default:
		return fmt.Errorf("unknown tmux artifact format: %q", format)
	}
}

func copyFile(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

func extractGzip(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	gzr, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gzr.Close()

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(gzr, maxArtifactBytes+1)); err != nil {
		return fmt.Errorf("gunzip tmux: %w", err)
	}
	return nil
}

func extractTarGz(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	gzr, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gzr.Close()

	want := tmuxBinaryName()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != want {
			continue
		}
		out, err := os.Create(dstPath)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxArtifactBytes+1)); err != nil {
			out.Close()
			return fmt.Errorf("extract tmux from tar: %w", err)
		}
		out.Close()
		return nil
	}
	return fmt.Errorf("%q not found in tar.gz artifact", want)
}

func extractZip(srcPath, dstPath string) error {
	r, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	want := tmuxBinaryName()
	for _, f := range r.File {
		if filepath.Base(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dstPath)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(rc, maxArtifactBytes+1)); err != nil {
			rc.Close()
			out.Close()
			return fmt.Errorf("extract tmux from zip: %w", err)
		}
		rc.Close()
		out.Close()
		return nil
	}
	return fmt.Errorf("%q not found in zip artifact", want)
}
