package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/releasesig"
)

// The updater's archive download limit is also 512 MiB. Pre-sign validation
// bounds disk reads independently of untrusted draft asset size metadata.
const maxDraftArchiveSize int64 = 512 << 20

// verifyDraftAssets checks unsigned draft consistency, not provenance or
// signature trust. It never loads a private key, signs, extracts or publishes.
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
		if err := verifyDraftArchive(root, name, digest); err != nil {
			return err
		}
	}
	return nil
}

func verifyDraftArchive(root *os.Root, name, digest string) error {
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
	return nil
}
