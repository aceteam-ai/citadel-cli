//go:build !linux

package reconstruct

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsupportedPublicationRefusesWithoutChangingDirectories(t *testing.T) {
	t.Parallel()
	for _, existingDestination := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existingDestination], func(t *testing.T) {
			root := t.TempDir()
			source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			const sourceBytes = "synthetic-new-artifact"
			if err := os.WriteFile(filepath.Join(source, "splat.ply"), []byte(sourceBytes), 0o600); err != nil {
				t.Fatal(err)
			}
			if existingDestination {
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(destination, "prior"), []byte("synthetic-prior"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := publishDirectory(context.Background(), source, destination)
			if err == nil || !strings.Contains(err.Error(), "create-only bundle publication requires the supported Linux GPU container") {
				t.Fatalf("publication error = %v, want unsupported-platform refusal", err)
			}
			data, err := os.ReadFile(filepath.Join(source, "splat.ply"))
			if err != nil || string(data) != sourceBytes {
				t.Fatalf("source changed after refusal: %q, %v", data, err)
			}
			if existingDestination {
				data, err := os.ReadFile(filepath.Join(destination, "prior"))
				if err != nil || string(data) != "synthetic-prior" {
					t.Fatalf("prior destination changed after refusal: %q, %v", data, err)
				}
				entries, err := os.ReadDir(destination)
				if err != nil || len(entries) != 1 {
					t.Fatalf("destination gained publication entries: %v, %v", entries, err)
				}
			} else if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported publication created destination: %v", err)
			}
		})
	}
}

func TestUnsupportedPublicationPreservesCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := publishDirectory(ctx, filepath.Join(root, "source"), filepath.Join(root, "destination")); !errors.Is(err, context.Canceled) {
		t.Fatalf("publication error = %v, want context cancellation", err)
	}
}
