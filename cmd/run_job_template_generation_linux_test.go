//go:build linux

package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/artifact"
)

func TestCollectOutputsGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "media.bin")
	if err := os.WriteFile(path, []byte("original output"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outputs, hashes, err := (liveTemplateRunOps{}).collectOutputs(context.Background(), root, "attempt/out", []string{"media.bin"}, 1, 2, 128)
	if err != nil || len(outputs) != 1 || len(hashes) != 1 {
		t.Fatalf("outputs=%+v hashes=%v err=%v", outputs, hashes, err)
	}
	f, _ := root.Open("media.bin")
	defer f.Close()
	identity, err := artifact.Capture(f)
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("original output")))
	generation, _ := identity.Generation(hash)
	if outputs[0].Path != "attempt/out/media.bin" || outputs[0].Bytes != 15 || outputs[0].SHA256 != hash || hashes[0] != hash || outputs[0].Generation != generation {
		t.Fatalf("unbound output %+v", outputs[0])
	}
}

func TestCollectOutputsRejectsMutationDuringHash(t *testing.T) {
	for _, mutation := range []string{"same size rewrite", "append", "truncate", "wrong hash count"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "media.bin")
			if err := os.WriteFile(path, []byte("original output"), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			ops := liveTemplateRunOps{hashOutput: func(ctx context.Context, f *os.File, limit int64) (string, int64, error) {
				// Hash a prefix, then mutate before the remaining hash reads. No
				// scheduler timing is needed to land inside the producer's window.
				prefix := make([]byte, 4)
				if _, err := f.Read(prefix); err != nil {
					return "", 0, err
				}
				info, _ := f.Stat()
				switch mutation {
				case "same size rewrite":
					err = os.WriteFile(path, []byte("modified output"), 0600)
					if err == nil {
						err = os.Chtimes(path, info.ModTime().Add(-time.Hour), info.ModTime().Add(-time.Hour))
					}
				case "append":
					var writer *os.File
					writer, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					if err == nil {
						_, err = writer.WriteString("more")
						writer.Close()
					}
				case "truncate":
					err = os.Truncate(path, 6)
				}
				if err != nil {
					return "", 0, err
				}
				if _, err = f.Seek(0, 0); err != nil {
					return "", 0, err
				}
				hash, count, err := sha256OpenFileLimit(ctx, f, limit)
				if mutation == "wrong hash count" {
					count--
				}
				return hash, count, err
			}}
			outputs, hashes, err := ops.collectOutputs(context.Background(), root, "attempt/out", []string{"media.bin"}, 1, 2, 128)
			if err == nil || len(outputs) != 0 || len(hashes) != 0 {
				t.Fatalf("mutation emitted pinned output: %+v %v %v", outputs, hashes, err)
			}
		})
	}
}
