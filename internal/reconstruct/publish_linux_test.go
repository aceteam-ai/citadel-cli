//go:build linux

package reconstruct

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunPreservesDestinationCreatedDuringTraining(t *testing.T) {
	for _, kind := range []string{"empty-directory", "nonempty-directory", "file", "symlink", "dangling-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			frames, output := filepath.Join(root, "frames"), filepath.Join(root, "result")
			if err := os.Mkdir(frames, 0o700); err != nil {
				t.Fatal(err)
			}
			var before os.FileInfo
			fake := successfulFake(t, []byte("losing-synthetic-ply"))
			original := fake.run
			fake.run = func(ctx context.Context, invocation Invocation, index int) error {
				if err := original(ctx, invocation, index); err != nil {
					return err
				}
				if invocation.Args[0] != "train" {
					return nil
				}
				switch kind {
				case "empty-directory", "nonempty-directory":
					if err := os.Mkdir(output, 0o700); err != nil {
						return err
					}
					if kind == "nonempty-directory" {
						if err := os.WriteFile(filepath.Join(output, "sentinel"), []byte("keep"), 0o600); err != nil {
							return err
						}
					}
				case "file":
					if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
						return err
					}
				case "symlink", "dangling-symlink":
					target := filepath.Join(root, "target")
					if kind == "symlink" {
						if err := os.Mkdir(target, 0o700); err != nil {
							return err
						}
					}
					if err := os.Symlink(target, output); err != nil {
						return err
					}
				}
				var err error
				before, err = os.Lstat(output)
				return err
			}
			result, err := (Runner{Executor: fake}).Run(context.Background(), Request{
				InputPath: frames, OutputDir: output, Iterations: 3,
			})
			if !errors.Is(err, os.ErrExist) || result.Artifact != nil {
				t.Fatalf("collision returned artifact=%v err=%v", result.Artifact, err)
			}
			after, err := os.Lstat(output)
			if err != nil || before == nil || !os.SameFile(before, after) {
				t.Fatal("pre-existing destination inode was replaced")
			}
			switch kind {
			case "empty-directory", "nonempty-directory":
				entries, err := os.ReadDir(output)
				want := 0
				if kind == "nonempty-directory" {
					want = 1
					raw, readErr := os.ReadFile(filepath.Join(output, "sentinel"))
					if readErr != nil || string(raw) != "keep" {
						t.Fatal("existing directory contents changed")
					}
				}
				if err != nil || len(entries) != want {
					t.Fatal("private bundle was published into a collision")
				}
			case "file":
				if raw, err := os.ReadFile(output); err != nil || string(raw) != "keep" {
					t.Fatal("existing file contents changed")
				}
			case "symlink", "dangling-symlink":
				if target, err := os.Readlink(output); err != nil || target != filepath.Join(root, "target") {
					t.Fatal("existing symlink changed")
				}
			}
			assertNoPrivatePublicationDirs(t, output)
		})
	}
}

func TestConcurrentRunsPublishOnlyOneWholeBundle(t *testing.T) {
	root := t.TempDir()
	frames, output := filepath.Join(root, "frames"), filepath.Join(root, "result")
	if err := os.Mkdir(frames, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	type outcome struct {
		result Result
		err    error
		raw    []byte
	}
	done := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		raw := []byte(fmt.Sprintf("synthetic-ply-%d", i))
		fake := successfulFake(t, raw)
		original := fake.run
		fake.run = func(ctx context.Context, invocation Invocation, index int) error {
			if err := original(ctx, invocation, index); err != nil {
				return err
			}
			if invocation.Args[0] == "train" {
				ready <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		go func() {
			result, err := (Runner{Executor: fake}).Run(ctx, Request{
				InputPath: frames, OutputDir: output, Iterations: 3,
			})
			done <- outcome{result: result, err: err, raw: raw}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case early := <-done:
			t.Fatalf("run failed before the two-writer barrier: %v", early.err)
		case <-ctx.Done():
			t.Fatal("two-writer barrier did not complete")
		}
	}
	close(release)
	var winner *outcome
	for i := 0; i < 2; i++ {
		var result outcome
		select {
		case result = <-done:
		case <-ctx.Done():
			t.Fatal("competing publications did not finish")
		}
		if result.err == nil {
			if winner != nil || result.result.Artifact == nil {
				t.Fatal("publication did not have exactly one winner")
			}
			winner = &result
		} else if !errors.Is(result.err, os.ErrExist) || result.result.Artifact != nil {
			t.Fatalf("loser returned artifact=%v err=%v", result.result.Artifact, result.err)
		}
	}
	if winner == nil {
		t.Fatal("neither complete bundle was published")
	}
	raw, err := os.ReadFile(filepath.Join(output, "splat.ply"))
	if err != nil || string(raw) != string(winner.raw) ||
		winner.result.Artifact.SHA256 != fmt.Sprintf("sha256:%x", sha256.Sum256(raw)) ||
		winner.result.Artifact.Size != int64(len(raw)) {
		t.Fatal("published bytes/hash/length do not match the winning bundle")
	}
	assertNoPrivatePublicationDirs(t, output)
}

func TestPublishDirectoryRefusesCancelledContextAndSymlinkParent(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output := filepath.Join(root, "result")
	if err := publishDirectory(ctx, source, output); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled publication: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{
		{filepath.Join(alias, "source"), output},
		{source, filepath.Join(alias, "result")},
	} {
		if err := publishDirectory(context.Background(), paths[0], paths[1]); err == nil {
			t.Fatal("publication followed a final-component parent symlink")
		}
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("refused publication removed the owned source")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused publication exposed output")
	}
}

func assertNoPrivatePublicationDirs(t *testing.T, output string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(output), "."+filepath.Base(output)+".*-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("private pipeline directories remain: %v err=%v", matches, err)
	}
}
