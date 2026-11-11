//go:build linux

package mediainput

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func workspaceContract(t *testing.T, paths []string, data [][]byte, formats []string) Contract {
	t.Helper()
	inputs := make([]map[string]any, len(paths))
	for i, path := range paths {
		sum := sha256.Sum256(data[i])
		role := "source"
		if i == 1 {
			role = "mask"
		}
		inputs[i] = map[string]any{
			"role": role, "origin": "workspace", "path": path, "format": formats[i],
			"sha256": hex.EncodeToString(sum[:]), "size": len(data[i]),
		}
	}
	raw, err := json.Marshal(map[string]any{"schema": Schema, "inputs": inputs})
	if err != nil {
		t.Fatal(err)
	}
	task := TaskImageToImage
	if len(paths) == 2 {
		task = TaskInpaint
	}
	contract, err := Parse(task, raw)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func writeOwnedInput(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSnapshotIsPrivateAndStable(t *testing.T) {
	dir := t.TempDir()
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	writeOwnedInput(t, dir, "source.png", data)
	contract := workspaceContract(t, []string{"source.png"}, [][]byte{data}, []string{"png"})
	snapshot, err := (WorkspaceFreezer{WorkspaceDir: dir, FilesEnabled: func() bool { return true }}).Freeze(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	metadata := snapshot.Inputs()
	if len(metadata) != 1 || metadata[0].Width != 2 || metadata[0].Height != 3 || metadata[0].SHA256 != contract.Inputs()[0].SHA256 {
		t.Fatalf("wrong metadata: %+v", metadata)
	}
	metadata[0].Width = 999
	writeOwnedInput(t, dir, "source.png", []byte("changed after freeze"))
	for i := 0; i < 2; i++ {
		reader, err := snapshot.Reader("source")
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%#v", reader) != "<private media-input reader>" {
			t.Fatal("reader diagnostic reflected private buffer")
		}
		got, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(got, data) || snapshot.Inputs()[0].Width != 2 {
			t.Fatal("workspace change or returned metadata mutated snapshot")
		}
		got[0] ^= 0xff
	}
	if _, err := snapshot.Reader("mask"); err == nil {
		t.Fatal("missing role returned a reader")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(format, snapshot); got != "<private media-input snapshot>" {
			t.Fatalf("unsafe snapshot diagnostic: %q", got)
		}
	}
	reader, err := snapshot.Reader("source")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{
		struct{ snapshot Snapshot }{snapshot},
		struct{ reader io.Reader }{reader},
	} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			text := fmt.Sprintf(format, value)
			for _, forbidden := range []string{fmt.Sprint(data), fmt.Sprintf("%#v", data), string(data)} {
				if strings.Contains(text, forbidden) {
					t.Fatal("private wrapper leaked encoded image bytes")
				}
			}
		}
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || string(raw) != "{}" {
		t.Fatalf("snapshot serialization leaked: %s %v", raw, err)
	}
}

func TestWorkspaceLargeEncodedInput(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	state := uint32(1)
	for i := range img.Pix {
		state = 1664525*state + 1013904223
		img.Pix[i] = byte(state >> 24)
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	data := encodedPNG(t, img)
	if len(data) <= 64<<10 {
		t.Fatal("fixture does not exercise multiple read chunks")
	}
	dir := t.TempDir()
	writeOwnedInput(t, dir, "source.png", data)
	contract := workspaceContract(t, []string{"source.png"}, [][]byte{data}, []string{"png"})
	snapshot, err := (WorkspaceFreezer{WorkspaceDir: dir, FilesEnabled: func() bool { return true }}).Freeze(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := snapshot.Reader("source")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("multi-chunk read did not freeze exactly the expected bytes")
	}
}

func TestWorkspaceFIFORefusedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.png")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 1, 1)))
	contract := workspaceContract(t, []string{"input.png"}, [][]byte{data}, []string{"png"})
	done := make(chan error, 1)
	go func() {
		_, err := (WorkspaceFreezer{WorkspaceDir: dir, FilesEnabled: func() bool { return true }}).Freeze(context.Background(), contract)
		done <- err
	}()
	select {
	case err := <-done:
		assertReason(t, err, "invalid_input_path")
	case <-time.After(2 * time.Second):
		// Release an accidentally blocking reader before failing the test.
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if err == nil {
			defer unix.Close(fd)
		}
		t.Fatal("FIFO open blocked before regular-file validation")
	}
}

func TestWorkspaceRefusesNoPermissionOrUnsupportedInbox(t *testing.T) {
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 1, 1)))
	contract := workspaceContract(t, []string{"missing.png"}, [][]byte{data}, []string{"png"})
	for _, enabled := range []func() bool{nil, func() bool { return false }} {
		_, err := (WorkspaceFreezer{FilesEnabled: enabled}).Freeze(context.Background(), contract)
		assertReason(t, err, "files_disabled")
	}
	valid, _ := loadVectors(t)
	var vector validVector
	for _, candidate := range valid {
		if candidate.Name == "inbox" {
			vector = candidate
		}
	}
	inbox, err := Parse(vector.Task, vector.Value)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (WorkspaceFreezer{}).Freeze(context.Background(), inbox)
	assertReason(t, err, "inbox_unavailable")
	_, err = (WorkspaceFreezer{}).Freeze(context.Background(), Contract{})
	assertReason(t, err, "invalid_schema")
}

func TestWorkspaceRefusesUnsafeFiles(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	writeOwnedInput(t, dir, "source.png", data)
	writeOwnedInput(t, outside, "source.png", data)
	if err := os.Symlink("source.png", filepath.Join(dir, "leaf.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	freezer := WorkspaceFreezer{WorkspaceDir: dir, FilesEnabled: func() bool { return true }}
	for _, path := range []string{"leaf.png", "escape/source.png", "directory", "missing.png"} {
		contract := workspaceContract(t, []string{path}, [][]byte{data}, []string{"png"})
		_, err := freezer.Freeze(context.Background(), contract)
		assertReason(t, err, "invalid_input_path")
		if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), path) {
			t.Fatalf("path leaked: %v", err)
		}
	}
}

func TestWorkspaceSizeHashAndConcurrentChange(t *testing.T) {
	dir := t.TempDir()
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	contract := workspaceContract(t, []string{"source.png"}, [][]byte{data}, []string{"png"})
	freezer := WorkspaceFreezer{WorkspaceDir: dir, FilesEnabled: func() bool { return true }}
	writeOwnedInput(t, dir, "source.png", append(append([]byte(nil), data...), 0))
	_, err := freezer.Freeze(context.Background(), contract)
	assertReason(t, err, "size_mismatch")
	modified := append([]byte(nil), data...)
	modified[len(modified)-1] ^= 1
	writeOwnedInput(t, dir, "source.png", modified)
	_, err = freezer.Freeze(context.Background(), contract)
	assertReason(t, err, "sha256_mismatch")
	writeOwnedInput(t, dir, "source.png", data)
	freezer.afterRead = func() {
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(dir, "source.png"), future, future); err != nil {
			t.Fatal(err)
		}
	}
	_, err = freezer.Freeze(context.Background(), contract)
	assertReason(t, err, "input_changed")
}

func TestWorkspaceCancellationAndPermissionRecheck(t *testing.T) {
	dir := t.TempDir()
	data := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	writeOwnedInput(t, dir, "source.png", data)
	contract := workspaceContract(t, []string{"source.png"}, [][]byte{data}, []string{"png"})
	ctx, cancel := context.WithCancel(context.Background())
	freezer := WorkspaceFreezer{WorkspaceDir: dir, FilesEnabled: func() bool { return true }}
	cancel()
	_, err := freezer.Freeze(ctx, contract)
	assertReason(t, err, "cancelled_or_timeout")
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	freezer.afterRead = cancel
	_, err = freezer.Freeze(ctx, contract)
	assertReason(t, err, "cancelled_or_timeout")
	enabled := true
	freezer.FilesEnabled = func() bool { return enabled }
	freezer.afterRead = func() { enabled = false }
	_, err = freezer.Freeze(context.Background(), contract)
	assertReason(t, err, "files_disabled")
}

func TestWorkspaceMaskDimensionsAndMode(t *testing.T) {
	dir := t.TempDir()
	source := encodedPNG(t, image.NewGray(image.Rect(0, 0, 2, 3)))
	writeOwnedInput(t, dir, "source.png", source)
	freezer := WorkspaceFreezer{WorkspaceDir: dir, FilesEnabled: func() bool { return true }}
	for _, tc := range []struct {
		image image.Image
		want  string
	}{
		{image.NewGray(image.Rect(0, 0, 2, 3)), ""},
		{image.NewGray(image.Rect(0, 0, 3, 3)), "mask_invalid"},
		{image.NewGray16(image.Rect(0, 0, 2, 3)), "mask_invalid"},
	} {
		mask := encodedPNG(t, tc.image)
		writeOwnedInput(t, dir, "mask.png", mask)
		contract := workspaceContract(t, []string{"source.png", "mask.png"}, [][]byte{source, mask}, []string{"png", "png"})
		_, err := freezer.Freeze(context.Background(), contract)
		if tc.want == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			assertReason(t, err, tc.want)
		}
	}
}
