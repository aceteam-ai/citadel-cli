// internal/cobrowseprofile/archive.go
//
// Directory <-> single-blob serialization for the encrypted profile. The whole
// profile directory becomes one tar blob so it can be sealed by a single
// nodevault Seal call; the blob is the plaintext that never touches disk.
//
// Two deliberate constraints:
//
//   - We tar only regular files and directories. A live Chromium user-data-dir
//     also holds runtime-only artifacts (SingletonLock/Socket/Cookie, sockets,
//     symlinks) that are meaningless outside the launch that made them and, in
//     the socket/symlink case, are not representable as portable tar content.
//     Skipping them keeps the blob to durable state and avoids re-materializing a
//     stale lock into a fresh session. We intentionally KEEP sqlite -wal/-shm
//     sidecars: they carry not-yet-checkpointed cookie/login writes.
//
//   - Extraction rejects any entry whose path escapes the destination (absolute
//     path or a ".." component) and skips symlink entries. The blob is
//     AEAD-authenticated so a tampered archive cannot reach here, but path
//     traversal defense is cheap and keeps the extractor safe regardless of how
//     the blob was produced.
package cobrowseprofile

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runtimeOnlyNames are basenames that are per-launch runtime artifacts, not
// durable profile state; they are skipped when archiving so a re-materialized
// profile does not carry a stale singleton lock into a new browser launch.
var runtimeOnlyNames = map[string]bool{
	"SingletonLock":   true,
	"SingletonSocket": true,
	"SingletonCookie": true,
}

// cacheDirNames are regenerable cache directories excluded from the archive to
// keep the sealed blob (and the in-memory plaintext) bounded — a full Chromium
// cache can be hundreds of MB, none of it needed to persist a login.
var cacheDirNames = map[string]bool{
	"Cache":               true,
	"Code Cache":          true,
	"GPUCache":            true,
	"ShaderCache":         true,
	"GrShaderCache":       true,
	"DawnCache":           true,
	"DawnGraphiteCache":   true,
	"DawnWebGPUCache":     true,
	"component_crx_cache": true,
}

// buildTar walks dir and returns a tar blob of its durable regular files and
// directories, with relative paths. Runtime-only files and cache directories are
// skipped. An empty or absent dir yields a valid empty archive.
func buildTar(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil // the root itself is implied by the destination dir
		}
		base := filepath.Base(rel)
		if info.IsDir() {
			if cacheDirNames[base] {
				return filepath.SkipDir
			}
			return writeTarDir(tw, rel)
		}
		if runtimeOnlyNames[base] {
			return nil
		}
		if !info.Mode().IsRegular() {
			// Sockets, symlinks, pipes: not durable profile state, skip.
			return nil
		}
		return writeTarFile(tw, path, rel, info)
	})
	if err != nil {
		_ = tw.Close()
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeTarDir writes a directory header with a trailing slash and 0700 perms
// (the profile is private).
func writeTarDir(tw *tar.Writer, rel string) error {
	return tw.WriteHeader(&tar.Header{
		Name:     filepath.ToSlash(rel) + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o700,
	})
}

// writeTarFile streams one regular file into the archive with 0600 perms.
func writeTarFile(tw *tar.Writer, path, rel string, info os.FileInfo) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{
		Name:     filepath.ToSlash(rel),
		Typeflag: tar.TypeReg,
		Mode:     0o600,
		Size:     info.Size(),
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// extractTar writes the archive's entries under dstDir. It rejects path-traversal
// entries and skips anything that is not a regular file or directory. dstDir is
// assumed to already exist and be private (0700).
func extractTar(blob []byte, dstDir string) error {
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dstDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := writeExtractedFile(target, tr); err != nil {
				return err
			}
		default:
			// Symlinks and other types are never written (defense in depth).
			continue
		}
	}
}

// writeExtractedFile creates one file (0600) and copies its content from the tar
// reader.
func writeExtractedFile(target string, tr *tar.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, tr); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// safeJoin joins name onto dst, REJECTING (not silently sanitizing) an absolute
// path or any ".." component so a malicious archive cannot escape the destination
// directory (zip-slip). It also re-checks the joined result stays within dst as a
// belt-and-suspenders guard.
func safeJoin(dst, name string) (string, error) {
	slashed := filepath.ToSlash(name)
	if slashed == "" || strings.HasPrefix(slashed, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("cobrowseprofile: unsafe absolute archive path %q", name)
	}
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return "", fmt.Errorf("cobrowseprofile: unsafe archive path %q", name)
		}
	}
	joined := filepath.Join(dst, filepath.FromSlash(slashed))
	if joined != dst && !strings.HasPrefix(joined, dst+string(os.PathSeparator)) {
		return "", fmt.Errorf("cobrowseprofile: unsafe archive path %q", name)
	}
	return joined, nil
}

// writeFileAtomic writes data to a temp file in the same directory, fsyncs it, and
// renames it over path, so a crash mid-write never leaves a truncated or empty
// profile blob (the previous good blob survives until the rename completes). The
// parent directory is created 0700. Mirrors nodevault's own atomic writer.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
