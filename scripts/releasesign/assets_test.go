package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/releasesig"
)

func draftFixture(t *testing.T, tag string) (string, string, []string) {
	t.Helper()
	dir := t.TempDir()
	var lines []string
	var names []string
	for _, platform := range []string{"linux_amd64.tar.gz", "linux_arm64.tar.gz", "darwin_amd64.tar.gz", "darwin_arm64.tar.gz", "windows_amd64.zip", "windows_arm64.zip"} {
		name := "citadel_" + tag + "_" + platform
		data := []byte("synthetic archive bytes for " + platform)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name))
		names = append(names, name)
	}
	in := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(in, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, in, names
}

func TestVerifyDraftAssets(t *testing.T) {
	for _, tag := range []string{"v9.8.7", "v9.8.7-rc.1"} {
		t.Run(tag, func(t *testing.T) {
			dir, in, _ := draftFixture(t, tag)
			if err := run([]string{"verify-assets", "--tag", tag, "--in", in, "--dir", dir}); err != nil {
				t.Fatalf("matching unsigned draft refused: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "checksums.txt.sig")); !os.IsNotExist(err) {
				t.Fatalf("read-only validation wrote a signature: %v", err)
			}
		})
	}
}

func TestVerifyDraftAssetsRefusesChangedBytes(t *testing.T) {
	dir, in, names := draftFixture(t, "v9.8.7")
	if err := os.WriteFile(filepath.Join(dir, names[0]), []byte("changed after CI"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"verify-assets", "--tag", "v9.8.7", "--in", in, "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("changed archive accepted or wrong refusal: %v", err)
	}
}

func TestVerifyDraftAssetsRefusals(t *testing.T) {
	for _, kind := range []string{"foreign-tag", "invalid-tag", "partial", "unexpected-platform", "extra-platform", "duplicate", "traversal", "missing", "directory", "symlink-escape", "oversize-archive", "oversize-checksums"} {
		t.Run(kind, func(t *testing.T) {
			dir, in, names := draftFixture(t, "v9.8.7")
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			tag := "v9.8.7"
			path := filepath.Join(dir, names[0])
			switch kind {
			case "foreign-tag":
				tag = "v9.8.8"
			case "invalid-tag":
				tag = "../v9.8.7"
			case "partial":
				data = []byte(strings.SplitN(string(data), "\n", 2)[0] + "\n")
			case "unexpected-platform":
				data = []byte(strings.Replace(string(data), names[0], "citadel_v9.8.7_other_amd64.tar.gz", 1))
			case "extra-platform":
				data = append(data, []byte(strings.Repeat("a", 64)+"  citadel_v9.8.7_extra_amd64.tar.gz\n")...)
			case "duplicate":
				data = append(data, []byte(strings.SplitN(string(data), "\n", 2)[0]+"\n")...)
			case "traversal":
				data = []byte(strings.Replace(string(data), names[0], "../"+names[0], 1))
			case "missing", "directory", "symlink-escape":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "symlink-escape" {
					outside := filepath.Join(t.TempDir(), "outside")
					if err := os.WriteFile(outside, []byte("synthetic archive bytes for linux_amd64.tar.gz"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
			case "oversize-archive":
				if err := os.Truncate(path, (512<<20)+1); err != nil {
					t.Fatal(err)
				}
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.New()
				_, err = io.Copy(hash, file)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.SplitN(string(data), "\n", 2)
				data = []byte(fmt.Sprintf("%x  %s\n%s", hash.Sum(nil), names[0], lines[1]))
			case "oversize-checksums":
				data = []byte(strings.Repeat("x", releasesig.MaxChecksumsFileSize+1))
			}
			if err := os.WriteFile(in, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := run([]string{"verify-assets", "--tag", tag, "--in", in, "--dir", dir}); err == nil {
				t.Fatal("invalid draft accepted")
			}
		})
	}
}

func TestDraftArchiveSizeBound(t *testing.T) {
	if maxDraftArchiveSize != 512<<20 {
		t.Fatalf("archive size bound = %d, want 512 MiB", maxDraftArchiveSize)
	}
}

func TestVerifyDraftAssetsRequiredFlags(t *testing.T) {
	for _, args := range [][]string{
		{"verify-assets"},
		{"verify-assets", "--tag", "v9.8.7", "--in", "checksums.txt"},
		{"verify-assets", "--tag", "v9.8.7", "--dir", "archives"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("incomplete arguments accepted: %v", args)
		}
	}
}
