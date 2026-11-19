package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/releasesig"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

var fixtureExecutables = struct {
	sync.Mutex
	data map[string][]byte
}{data: make(map[string][]byte)}

func fixtureExecutable(t *testing.T, tag, goos, goarch string) []byte {
	t.Helper()
	fixtureExecutables.Lock()
	defer fixtureExecutables.Unlock()
	key := tag + goos + goarch
	if data := fixtureExecutables.data[key]; data != nil {
		return data
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":              "module github.com/aceteam-ai/citadel-cli\n\ngo 1.26.0\n",
		"cmd/version.go":      "package cmd\nvar version = \"dev\"\nfunc Version() string { return version }\n",
		"cmd/citadel/main.go": "package main\nimport (\"fmt\"; \"github.com/aceteam-ai/citadel-cli/cmd\")\nfunc main() { fmt.Println(cmd.Version()) }\n",
	}
	for name, source := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	path := filepath.Join(dir, "fixture")
	command := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-ldflags", "-X github.com/aceteam-ai/citadel-cli/cmd.version="+strings.TrimPrefix(tag, "v"), "-o", path, "./cmd/citadel")
	command.Dir = dir
	command.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fixtureExecutables.data[key] = data
	return data
}

func fixtureArchive(t *testing.T, path, tag, platform string) {
	t.Helper()
	parts := strings.Split(platform, "_")
	goos := parts[0]
	goarch := strings.Split(parts[1], ".")[0]
	data := fixtureExecutable(t, tag, goos, goarch)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if goos == "windows" {
		writer := zip.NewWriter(file)
		for _, name := range []string{"citadel.exe", "citadel-start.exe"} {
			member, err := writer.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := member.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(file)
		writer := tar.NewWriter(gz)
		if err := writer.WriteHeader(&tar.Header{Name: "citadel", Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func draftFixture(t *testing.T, tag string) (string, string, []string) {
	t.Helper()
	dir := t.TempDir()
	var lines []string
	var names []string
	for _, platform := range []string{"linux_amd64.tar.gz", "linux_arm64.tar.gz", "darwin_amd64.tar.gz", "darwin_arm64.tar.gz", "windows_amd64.zip", "windows_arm64.zip"} {
		name := "citadel_" + tag + "_" + platform
		fixtureArchive(t, filepath.Join(dir, name), tag, platform)
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
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

func TestProductionBuildArgvMatchesUpdater(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "citadel")
	// Fetch the exact production argv, rather than duplicating a build recipe
	// which could stay green while the release builder reintroduces trimpath.
	code := "import importlib.util,json,sys; s=importlib.util.spec_from_file_location('r','scripts/release_draft.py'); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); print(json.dumps(m.go_build_args('v9.8.7',sys.argv[1],'./cmd/citadel')))"
	python := exec.Command("python3", "-I", "-B", "-c", code, path)
	python.Dir = root
	output, err := python.Output()
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	if err := json.Unmarshal(output, &argv); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = root
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("production build: %v: %s", err, output)
	}
	metadata, err := update.ReadExecutableVersion(path)
	if err != nil {
		t.Fatalf("updater refuses actual release argv: %v", err)
	}
	if metadata.Version != "9.8.7" || metadata.Source != update.VersionSourceLinkerX {
		t.Fatalf("wrong production stamp: %+v", metadata)
	}
}

func TestDraftExecutableMetadataRefusals(t *testing.T) {
	for _, kind := range []string{"wrong-version", "wrong-platform", "wrong-os", "foreign-main", "not-executable", "traversal", "symlink", "duplicate", "oversized-member"} {
		t.Run(kind, func(t *testing.T) {
			dir, in, names := draftFixture(t, "v9.8.7")
			path := filepath.Join(dir, names[0])
			data := fixtureExecutable(t, "v9.8.7", "linux", "amd64")
			switch kind {
			case "wrong-version":
				data = fixtureExecutable(t, "v9.8.6", "linux", "amd64")
			case "wrong-platform":
				data = fixtureExecutable(t, "v9.8.7", "linux", "arm64")
			case "wrong-os":
				data = fixtureExecutable(t, "v9.8.7", "windows", "amd64")
			case "foreign-main":
				data = bytes.ReplaceAll(data, []byte("cmd/citadel"), []byte("cmd/notadel"))
			case "not-executable":
				data = []byte("not a Go executable")
			}
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(file)
			writer := tar.NewWriter(gz)
			header := &tar.Header{Name: "citadel", Mode: 0o755, Size: int64(len(data))}
			if kind == "traversal" {
				header.Name = "../citadel"
			}
			if kind == "symlink" {
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "outside"
				header.Size = 0
				data = nil
			}
			if kind == "oversized-member" {
				header.Size = maxDraftArchiveSize + 1
				data = nil
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(data); err != nil {
				t.Fatal(err)
			}
			if kind == "duplicate" {
				if err := writer.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			err = writer.Close()
			if err != nil && kind != "oversized-member" {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			manifest, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			archive, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.SplitN(string(manifest), "\n", 2)
			updated := fmt.Sprintf("%x  %s\n%s", sha256.Sum256(archive), names[0], lines[1])
			if err := os.WriteFile(in, []byte(updated), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyDraftAssets(in, dir, "v9.8.7"); err == nil {
				t.Fatal("matching digest bypassed executable or archive validation")
			}
		})
	}
}

func TestDraftZipCountsActualDirectoryBeforeLoading(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"citadel.exe", "citadel-start.exe", "unexpected"} {
		member, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte("synthetic")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	end := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	binary.LittleEndian.PutUint16(data[end+8:], 2)
	binary.LittleEndian.PutUint16(data[end+10:], 2)
	if err := preflightDraftZip(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("forged two-entry count accepted three real directory records")
	}
	// Even a claimed two-record directory must end exactly at EOCD.
	binary.LittleEndian.PutUint32(data[end+12:], binary.LittleEndian.Uint32(data[end+12:])-1)
	if err := preflightDraftZip(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("forged directory extent accepted")
	}
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
