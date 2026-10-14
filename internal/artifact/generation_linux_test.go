//go:build linux

package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerationGolden(t *testing.T) {
	i := Identity{Bytes: 123, Device: 2049, Inode: 42, MtimeSeconds: 1700000000, MtimeNanoseconds: 123456789, CtimeSeconds: 1700000001, CtimeNanoseconds: 987654321}
	hash := "sha256:" + strings.Repeat("a", 64)
	want := "linux-stat-v1:263fb8adc8ed098a071b9722b2d8f77a415e2aa72a784a35f30e3ccb7539b0be"
	got, err := i.Generation(hash)
	if err != nil || got != want {
		t.Fatalf("generation=%q err=%v, want %q", got, err, want)
	}
	zero, err := (Identity{MtimeSeconds: -1}).Generation("sha256:" + strings.Repeat("0", 64))
	if err != nil || zero != "linux-stat-v1:f999fdb57c6babf70a993a5b41064475a2be3174e2b89b5180484567591c1095" {
		t.Fatalf("zero/pre-epoch golden=%s err=%v", zero, err)
	}
	for name, mutate := range map[string]func(*Identity){
		"bytes": func(i *Identity) { i.Bytes++ }, "device": func(i *Identity) { i.Device++ }, "inode": func(i *Identity) { i.Inode++ },
		"mtime seconds": func(i *Identity) { i.MtimeSeconds++ }, "mtime nanoseconds": func(i *Identity) { i.MtimeNanoseconds++ },
		"ctime seconds": func(i *Identity) { i.CtimeSeconds++ }, "ctime nanoseconds": func(i *Identity) { i.CtimeNanoseconds++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := i
			mutate(&changed)
			stamp, err := changed.Generation(hash)
			if err != nil || stamp == got {
				t.Fatalf("fingerprint field ignored: %s %v", stamp, err)
			}
		})
	}
	if changed, _ := i.Generation("sha256:" + strings.Repeat("b", 64)); changed == got {
		t.Fatal("recorded hash ignored")
	}
	for _, invalid := range []string{"", strings.Repeat("a", 64), "sha256:" + strings.Repeat("A", 64), hash + "\n"} {
		if _, err := i.Generation(invalid); err == nil {
			t.Fatalf("accepted noncanonical hash %q", invalid)
		}
	}
}

func TestCaptureRequiresDescriptorObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("stable"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before, err := Capture(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	after, err := Capture(f)
	if err != nil || before != after {
		t.Fatalf("read changes identity: %v %v", after, err)
	}
	info, _ := f.Stat()
	if _, err := observe(noSystemInfo{info}); err == nil {
		t.Fatal("unsupported observations accepted")
	}
	dir, _ := os.Open(filepath.Dir(path))
	defer dir.Close()
	if _, err := Capture(dir); err == nil {
		t.Fatal("directory accepted")
	}
}

type noSystemInfo struct{ os.FileInfo }

func (noSystemInfo) Sys() any { return nil }
