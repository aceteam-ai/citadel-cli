//go:build unix

package containerjobs

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func ownershipImportFixture(t *testing.T, headers []*tar.Header) (Request, *fakeTransport) {
	t.Helper()
	req := transferRequest()
	req.VolumeRef = newVolume
	req.ContainerRef = ""
	req.QuotaBytes = 16 << 20
	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	for _, header := range headers {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	g, err := transferCipher(req)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0x35}, 12)
	encrypted := g.Seal(bytes.Clone(nonce), nonce, plain.Bytes(), chunkAAD(req.OrgID, req.TransferID, 0))
	digest := sha256.Sum256(encrypted)
	req.Manifest = &Manifest{Version: 1, OrgID: req.OrgID, TransferID: req.TransferID, Chunks: []Chunk{{Index: 0, PlaintextBytes: int64(plain.Len()), SHA256: hex.EncodeToString(digest[:])}}}
	req.ManifestSHA256 = manifestDigest(req.Manifest)
	return req, &fakeTransport{objects: map[string][]byte{req.ChunkURLs[0]: encrypted}}
}

func TestImportHandsOffNewFilesDirectoriesAndSymlinks(t *testing.T) {
	runtime := newFake(t)
	calls := map[string]int{}
	runtime.fileHandoff = func(file *os.File) error {
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		kind := "file:"
		if info.IsDir() {
			kind = "dir:"
		} else if info.Size() != 0 {
			t.Fatal("file data written before handoff")
		}
		calls[kind+filepath.Base(file.Name())]++
		return nil
	}
	runtime.linkHandoff = func(root *os.Root, name string) error {
		info, err := root.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("link handoff must not follow the target", err)
		}
		calls["symlink:"+name]++
		return nil
	}
	req, transport := ownershipImportFixture(t, []*tar.Header{
		{Name: "implicit/deep/file", Typeflag: tar.TypeReg, Mode: 0600, Uid: 123, Gid: 456, Size: 4},
		{Name: "implicit", Typeflag: tar.TypeDir, Mode: 0777, Uid: 777, Gid: 888},
		{Name: "links/hard", Typeflag: tar.TypeLink, Linkname: "implicit/deep/file"},
		{Name: "links/sym", Typeflag: tar.TypeSymlink, Linkname: "../implicit/deep/file"},
	})
	out, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime, Transport: transport}).Run(context.Background(), Import, req)
	if err != nil || out.Manifest == nil {
		t.Fatal("import failed", err)
	}
	for _, key := range []string{"dir:implicit", "dir:deep", "dir:links", "file:file", "symlink:links/sym"} {
		if calls[key] != 1 {
			t.Fatalf("missing/duplicate ownership handoff %s: %v", key, calls)
		}
	}
	if len(calls) != 5 {
		t.Fatalf("unexpected handoff (hardlinks must inherit): %v", calls)
	}
	target := filepath.Join(runtime.root, newVolume)
	info, _ := os.Stat(filepath.Join(target, "implicit/deep/file"))
	hard, _ := os.Stat(filepath.Join(target, "links/hard"))
	if !os.SameFile(info, hard) {
		t.Fatal("hardlink did not retain its handed-off inode")
	}
	if got, err := os.ReadFile(filepath.Join(target, "links/sym")); err != nil || string(got) != "xxxx" {
		t.Fatal("symlink data changed", err)
	}
}

func TestImportRefusesEachOwnershipFailure(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			runtime := newFake(t)
			reached := false
			runtime.fileHandoff = func(file *os.File) error {
				info, err := file.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if kind == "directory" && info.IsDir() || kind == "file" && info.Mode().IsRegular() {
					reached = true
					return errors.New("fake ownership failure")
				}
				return nil
			}
			runtime.linkHandoff = func(*os.Root, string) error {
				if kind == "symlink" {
					reached = true
					return errors.New("fake ownership failure")
				}
				return nil
			}
			req, transport := ownershipImportFixture(t, []*tar.Header{
				{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0600, Size: 1},
				{Name: "sym", Typeflag: tar.TypeSymlink, Linkname: "dir/file"},
			})
			out, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime, Transport: transport}).Run(context.Background(), Import, req)
			if !reached || err == nil || out.Manifest != nil || out.Bytes != 0 {
				t.Fatal("ownership failure accepted", reached, err, out.Bytes)
			}
		})
	}
}

func TestImportCannotHandoffOrLinkPreexistingEntries(t *testing.T) {
	for _, kind := range []string{"existing-parent", "existing-link-target", "existing-header"} {
		t.Run(kind, func(t *testing.T) {
			runtime := newFake(t)
			runtime.afterCreateVolume = func(dir string) {
				if err := os.Mkdir(filepath.Join(dir, "existing"), 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "existing-link-target" {
					if err := os.WriteFile(filepath.Join(dir, "preserved"), []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			runtime.fileHandoff = func(*os.File) error { t.Fatal("preexisting entry reached ownership handoff"); return nil }
			var headers []*tar.Header
			switch kind {
			case "existing-parent":
				headers = []*tar.Header{{Name: "existing", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "existing/new", Typeflag: tar.TypeReg, Mode: 0600, Size: 1}}
			case "existing-link-target":
				headers = []*tar.Header{{Name: "alias", Typeflag: tar.TypeLink, Linkname: "preserved"}}
			case "existing-header":
				headers = []*tar.Header{{Name: "existing", Typeflag: tar.TypeDir, Mode: 0777}}
			}
			req, transport := ownershipImportFixture(t, headers)
			_, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime, Transport: transport}).Run(context.Background(), Import, req)
			if kind == "existing-header" {
				if err != nil {
					t.Fatal("empty system directory header refused", err)
				}
			} else if err == nil {
				t.Fatal("preexisting path repurposed")
			}
			info, statErr := os.Stat(filepath.Join(runtime.root, newVolume, "existing"))
			if statErr != nil || info.Mode().Perm() != 0700 {
				t.Fatal("preexisting directory changed", statErr)
			}
		})
	}
}

func TestImportKeepsNewEntryParentsPrivateUntilAuthenticatedPublication(t *testing.T) {
	for _, failure := range []string{"none", "prepare", "publish", "trailing"} {
		t.Run(failure, func(t *testing.T) {
			runtime := newFake(t)
			private, published := false, false
			runtime.prepareImport = func(*os.Root) error {
				if failure == "prepare" {
					return errors.New("fake stage refusal")
				}
				private = true
				return nil
			}
			runtime.fileHandoff = func(*os.File) error {
				if !private || published {
					t.Fatal("entry created outside private custody")
				}
				return nil
			}
			runtime.linkHandoff = func(*os.Root, string) error {
				if !private || published {
					t.Fatal("symlink handed off outside private custody")
				}
				return nil
			}
			runtime.publishImport = func(root *os.Root) error {
				if !private {
					t.Fatal("publication before staging")
				}
				if failure == "publish" {
					return errors.New("fake publish refusal")
				}
				if failure == "trailing" {
					t.Fatal("publication before final archive authentication")
				}
				if got, err := root.ReadFile("dir/file"); err != nil || string(got) != "x" {
					t.Fatal("published before file data", err)
				}
				published = true
				return nil
			}
			req, transport := ownershipImportFixture(t, []*tar.Header{{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0600, Size: 1}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "dir/file"}})
			if failure == "trailing" {
				g, err := transferCipher(req)
				if err != nil {
					t.Fatal(err)
				}
				cipher := transport.objects[req.ChunkURLs[0]]
				plain, err := g.Open(nil, cipher[:12], cipher[12:], chunkAAD(req.OrgID, req.TransferID, 0))
				if err != nil {
					t.Fatal(err)
				}
				plain = append(plain, 1)
				cipher = g.Seal(bytes.Clone(cipher[:12]), cipher[:12], plain, chunkAAD(req.OrgID, req.TransferID, 0))
				transport.objects[req.ChunkURLs[0]] = cipher
				req.Manifest.Chunks[0].PlaintextBytes = int64(len(plain))
				digest := sha256.Sum256(cipher)
				req.Manifest.Chunks[0].SHA256 = hex.EncodeToString(digest[:])
				req.ManifestSHA256 = manifestDigest(req.Manifest)
			}
			out, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime, Transport: transport}).Run(context.Background(), Import, req)
			if failure == "none" {
				if err != nil || !published || out.Manifest == nil {
					t.Fatal("publication missing", err)
				}
			} else if err == nil || published || out.Manifest != nil || out.Bytes != 0 {
				t.Fatal("failed private import reported success", failure, err)
			}
		})
	}
}
