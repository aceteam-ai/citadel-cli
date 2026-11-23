//go:build linux || darwin

package harnessseat

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func saltFixture(t *testing.T) (*SaltStore, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSaltStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestSaltStoreReadDoesNotInitializeAndSaltIsStable(t *testing.T) {
	s, dir := saltFixture(t)
	ctx := context.Background()
	if _, err := s.LoadSalt(ctx); !errors.Is(err, ErrSaltMissing) {
		t.Fatalf("missing salt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, saltFileName)); !os.IsNotExist(err) {
		t.Fatal("read initialized salt")
	}
	first, err := s.InitializeSalt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == [32]byte{} {
		t.Fatal("zero salt")
	}
	info, err := os.Stat(filepath.Join(dir, saltFileName))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("unsafe salt: %v %v", info, err)
	}
	second, err := s.InitializeSalt(ctx)
	if err != nil || first != second {
		t.Fatal("initialization rotated an existing salt")
	}
	other, err := OpenSaltStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	third, err := other.LoadSalt(ctx)
	if err != nil || first != third {
		t.Fatal("salt did not survive reopening")
	}
}

func TestSaltStoreConcurrentInitializationDoesNotRotate(t *testing.T) {
	_, dir := saltFixture(t)
	const count = 12
	values := make([][32]byte, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range values {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := OpenSaltStore(dir)
			if err != nil {
				errs[i] = err
				return
			}
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			values[i], errs[i] = s.InitializeSalt(ctx)
		}(i)
	}
	wg.Wait()
	for i := range values {
		if errs[i] != nil || values[i] != values[0] {
			t.Fatalf("concurrent salt mismatch at %d: %v", i, errs[i])
		}
	}
}

func TestSaltStoreRefusesUnsafeStateWithoutRepairOrOverwrite(t *testing.T) {
	for _, kind := range []string{"short", "long", "public", "symlink", "hardlink", "directory", "lock-symlink", "lock-public"} {
		t.Run(kind, func(t *testing.T) {
			s, dir := saltFixture(t)
			path := filepath.Join(dir, saltFileName)
			original := bytes.Repeat([]byte{9}, 32)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "short", "long":
				n := 31
				if kind == "long" {
					n = 33
				}
				original = bytes.Repeat([]byte{9}, n)
				if err := os.WriteFile(path, original, 0o600); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else {
					target := filepath.Join(t.TempDir(), "untouched")
					if err := os.WriteFile(target, original, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(dir, "alias")); err != nil {
					t.Fatal(err)
				}
			case "lock-symlink", "lock-public":
				lock := filepath.Join(dir, saltLockName)
				if kind == "lock-symlink" {
					if err := os.Symlink(path, lock); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(lock, []byte("unchanged"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.InitializeSalt(context.Background()); err == nil {
				t.Fatal("unsafe state accepted")
			}
			if kind != "directory" {
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, original) {
					t.Fatal("unsafe state changed")
				}
			}
		})
	}
}

func TestSaltStoreRefusesUnsafeRootAndClosedStore(t *testing.T) {
	for _, kind := range []string{"public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if kind == "public" {
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(dir, alias); err != nil {
					t.Fatal(err)
				}
				dir = alias
			}
			if s, err := OpenSaltStore(dir); err == nil {
				s.Close()
				t.Fatal("unsafe root accepted")
			}
		})
	}
	s, _ := saltFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitializeSalt(context.Background()); err == nil {
		t.Fatal("closed store accepted")
	}
}

func TestSaltStoreCancellationDoesNotCreateOrRotateSalt(t *testing.T) {
	s, dir := saltFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.InitializeSalt(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled initialization: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancelled operation created state")
	}
	f, err := s.root.OpenFile(saltLockName, os.O_CREATE|os.O_RDWR|privateNoFollowFlags(), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := privateStoreLock(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = s.InitializeSalt(ctx)
	privateStoreUnlock(f)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock contention did not respect deadline: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, saltFileName)); !os.IsNotExist(err) {
		t.Fatal("timed-out operation published salt")
	}
	if _, err := s.InitializeSalt(context.Background()); err != nil {
		t.Fatalf("released lock did not recover: %v", err)
	}
}

type saltOwnerFixture struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (f saltOwnerFixture) Sys() any { return &f.stat }

func TestSaltStoreOwnershipPredicateRefusesForeignUID(t *testing.T) {
	s, dir := saltFixture(t)
	if _, err := s.InitializeSalt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, saltFileName)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := *info.Sys().(*syscall.Stat_t)
		stat.Uid++
		if privateStoreInfo(saltOwnerFixture{info, stat}, info.IsDir()) {
			t.Fatal("foreign UID accepted")
		}
	}
}

func TestSaltStoreRemainsAnchoredWhenDirectoryPathIsReplaced(t *testing.T) {
	s, dir := saltFixture(t)
	want, err := s.InitializeSalt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		if err := os.RemoveAll(moved); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(filepath.Join(dir, saltFileName), replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSalt(context.Background())
	if err != nil || got != want {
		t.Fatalf("store followed replaced directory path: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, saltFileName))
	if err != nil || !bytes.Equal(data, replacement) {
		t.Fatal("replacement directory was changed")
	}
}
