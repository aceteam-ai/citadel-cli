package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/releasesig"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// The updater's archive download limit is also 512 MiB. Pre-sign validation
// bounds disk reads independently of untrusted draft asset size metadata.
const maxDraftArchiveSize int64 = 512 << 20

// verifyDraftAssets checks unsigned draft consistency, not provenance or
// signature trust. It never loads a private key, signs, executes or publishes.
func verifyDraftAssets(checksumsPath, archiveDir, tag string) error {
	manifest, err := os.Open(checksumsPath)
	if err != nil {
		return err
	}
	defer manifest.Close()
	data, err := io.ReadAll(io.LimitReader(manifest, releasesig.MaxChecksumsFileSize+1))
	if err != nil {
		return err
	}
	// ParseChecksums is shared with the updater and binds every entry to tag.
	// Here it parses an unsigned manifest solely as a pre-sign consistency gate.
	checksums, err := releasesig.ParseChecksums(data, tag)
	if err != nil {
		return err
	}
	prefix := releasesig.ArchivePrefix(tag)
	names := []string{
		prefix + "linux_amd64.tar.gz",
		prefix + "linux_arm64.tar.gz",
		prefix + "darwin_amd64.tar.gz",
		prefix + "darwin_arm64.tar.gz",
		prefix + "windows_amd64.zip",
		prefix + "windows_arm64.zip",
	}
	if len(checksums) != len(names) {
		return fmt.Errorf("draft must list exactly %d platform archives", len(names))
	}
	root, err := os.OpenRoot(archiveDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range names {
		digest, ok := checksums[name]
		if !ok {
			return fmt.Errorf("draft is missing required platform archive %s", name)
		}
		if err := verifyDraftArchive(root, name, digest, tag); err != nil {
			return err
		}
	}
	return nil
}

func verifyDraftArchive(root *os.Root, name, digest, tag string) error {
	entry, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("read draft archive %s: %w", name, err)
	}
	if !entry.Mode().IsRegular() {
		return fmt.Errorf("draft archive %s is not a regular file", name)
	}
	// Root.Open refuses symlink escapes even if a pathname changes concurrently.
	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("read draft archive %s: %w", name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("draft archive %s is not a regular file", name)
	}
	if info.Size() > maxDraftArchiveSize {
		return fmt.Errorf("draft archive %s exceeds size bound", name)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maxDraftArchiveSize+1))
	if err != nil {
		return err
	}
	if n > maxDraftArchiveSize {
		return fmt.Errorf("draft archive %s exceeds size bound", name)
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != digest {
		return fmt.Errorf("draft archive %s digest mismatch", name)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	platform := strings.TrimPrefix(name, releasesig.ArchivePrefix(tag))
	platform = strings.TrimSuffix(strings.TrimSuffix(platform, ".tar.gz"), ".zip")
	parts := strings.Split(platform, "_")
	if len(parts) != 2 {
		return fmt.Errorf("invalid draft platform")
	}
	return verifyArchiveExecutables(file, info.Size(), parts[0], parts[1], tag)
}

// Streams only expected regular members into private temporary files. Nothing
// is unpacked by an archive pathname, and total expanded bytes are bounded.
func verifyArchiveExecutables(file *os.File, size int64, goos, goarch, tag string) error {
	seen := map[string]bool{}
	var expanded int64
	member := func(name string, regular bool, size int64, r io.Reader) error {
		want := name == "citadel"
		if goos == "windows" {
			want = name == "citadel.exe" || name == "citadel-start.exe"
		}
		if !want || !regular || seen[name] || size <= 0 || size > maxDraftArchiveSize-expanded {
			return fmt.Errorf("unexpected, duplicate or oversized draft archive member")
		}
		seen[name] = true
		expanded += size
		f, err := os.CreateTemp("", "citadel-metadata-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		n, err := io.Copy(f, io.LimitReader(r, size+1))
		if err != nil || n != size {
			return fmt.Errorf("truncated or oversized draft executable")
		}
		if name == "citadel-start.exe" {
			return nil // The updater installs the CLI, not the optional launcher.
		}
		meta, err := update.ReadExecutableVersion(f.Name())
		if err != nil {
			return fmt.Errorf("draft executable metadata refused: %w", err)
		}
		if meta.Version != strings.TrimPrefix(tag, "v") || meta.Source != update.VersionSourceLinkerX || meta.GOOS != goos || meta.GOARCH != goarch {
			return fmt.Errorf("draft executable version or platform differs from release")
		}
		return nil
	}
	if goos == "windows" {
		if err := preflightDraftZip(file, size); err != nil {
			return err
		}
		zr, err := zip.NewReader(file, size)
		if err != nil {
			return fmt.Errorf("invalid draft zip")
		}
		if len(zr.File) != 2 {
			return fmt.Errorf("draft zip must contain exactly CLI and launcher")
		}
		for _, entry := range zr.File {
			if entry.UncompressedSize64 > uint64(maxDraftArchiveSize) {
				return fmt.Errorf("draft zip expansion exceeds bound")
			}
			r, err := entry.Open()
			if err != nil {
				return err
			}
			err = member(entry.Name, entry.Mode().IsRegular(), int64(entry.UncompressedSize64), r)
			r.Close()
			if err != nil {
				return err
			}
		}
	} else {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("invalid draft gzip")
		}
		defer gz.Close()
		limited := &io.LimitedReader{R: gz, N: maxDraftArchiveSize + 1}
		tr := tar.NewReader(limited)
		for {
			header, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("invalid or oversized draft tar")
			}
			if err := member(header.Name, header.Typeflag == tar.TypeReg, header.Size, tr); err != nil {
				return err
			}
		}
		if _, err := io.Copy(io.Discard, limited); err != nil || limited.N == 0 {
			return fmt.Errorf("draft gzip expansion exceeds bound or is corrupt")
		}
	}
	want := 1
	if goos == "windows" {
		want = 2
	}
	if len(seen) != want {
		return fmt.Errorf("draft archive is missing an executable")
	}
	return nil
}

// Production packaging uses classic ZIP with two members. Count the actual
// directory records before archive/zip can allocate based on forged counts.
func preflightDraftZip(file io.ReaderAt, size int64) error {
	if size < 22 {
		return fmt.Errorf("invalid draft zip directory")
	}
	tailSize := min(size, 22+65535)
	tail := make([]byte, tailSize)
	if _, err := file.ReadAt(tail, size-tailSize); err != nil {
		return err
	}
	i := bytes.LastIndex(tail, []byte{'P', 'K', 5, 6})
	if i < 0 || i+22 > len(tail) {
		return fmt.Errorf("missing draft zip directory")
	}
	eocd := tail[i:]
	word := func(offset int) uint16 { return binary.LittleEndian.Uint16(eocd[offset:]) }
	if i+22+int(word(20)) != len(tail) || word(4) != 0 || word(6) != 0 || word(8) != 2 || word(10) != 2 {
		return fmt.Errorf("unsupported draft zip directory")
	}
	length := int64(binary.LittleEndian.Uint32(eocd[12:]))
	start := int64(binary.LittleEndian.Uint32(eocd[16:]))
	if start+length != size-tailSize+int64(i) {
		return fmt.Errorf("draft zip directory extent mismatch")
	}
	position := start
	count := 0
	for position < start+length {
		var header [46]byte
		if count == 2 || position+46 > start+length {
			return fmt.Errorf("draft zip directory count mismatch")
		}
		if _, err := file.ReadAt(header[:], position); err != nil || string(header[:4]) != "PK\x01\x02" {
			return fmt.Errorf("invalid draft zip directory record")
		}
		for _, offset := range []int{28, 30, 32} {
			position += int64(binary.LittleEndian.Uint16(header[offset:]))
		}
		position += 46
		count++
	}
	if count != 2 || position != start+length {
		return fmt.Errorf("draft zip directory count or extent mismatch")
	}
	return nil
}
