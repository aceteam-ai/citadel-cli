package cobrowseprofile

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveExcludesStagingDir(t *testing.T) {
	src := t.TempDir()
	for rel, content := range map[string]string{
		"Default/Cookies":        "keep",
		".citadel-stage/doc.pdf": "user file",
		".citadel-stage/sub/x":   "user file",
	} {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := buildTar(src)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(blob))
	kept := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if isStagingPath(hdr.Name) || hdr.Name == ".citadel-stage/" {
			t.Fatalf("archive contains a staging entry: %s", hdr.Name)
		}
		if hdr.Name == "Default/Cookies" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("durable profile state was dropped")
	}
}

// An older or hostile blob that carries staging entries never restores them.
func TestExtractTarSkipsStagingEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name, body string, dir bool) {
		h := &tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(body))}
		if dir {
			h = &tar.Header{Name: name, Mode: 0o700, Typeflag: tar.TypeDir}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if !dir {
			_, _ = tw.Write([]byte(body))
		}
	}
	add(".citadel-stage/", "", true)
	add(".citadel-stage/doc.pdf", "user file", false)
	// Non-canonical spellings of the same destination must be skipped too.
	add(".//.citadel-stage/a.txt", "user file", false)
	add("././.citadel-stage/b.txt", "user file", false)
	add("Default/Cookies", "keep", false)
	_ = tw.Close()
	dst := t.TempDir()
	if err := extractTar(buf.Bytes(), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".citadel-stage")); err == nil {
		t.Fatal("a staging entry was restored")
	}
	if _, err := os.Stat(filepath.Join(dst, "Default", "Cookies")); err != nil {
		t.Fatalf("durable entry missing: %v", err)
	}
}
