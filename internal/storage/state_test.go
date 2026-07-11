package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreateState_CreateOnce is the load-bearing guarantee: credentials
// are minted once and persisted, then NEVER regenerated on a subsequent load.
// Regeneration would orphan stored objects and break presigned URLs.
func TestLoadOrCreateState_CreateOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// First start: mint and persist.
	first := &State{path: path}
	creds, err := generateCredentials()
	if err != nil {
		t.Fatalf("generateCredentials: %v", err)
	}
	first.Credentials = creds
	if err := first.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Every subsequent reload must return the SAME credentials.
	for i := 0; i < 3; i++ {
		got, err := loadStateFromPath(path)
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		if got.Credentials != creds {
			t.Fatalf("reload %d: credentials changed: got %+v want %+v", i, got.Credentials, creds)
		}
	}
}

// TestLoadStateFromPath_MissingIsEmpty asserts a fresh node (no state file) is
// an empty state, not an error, so the first start can proceed to mint.
func TestLoadStateFromPath_MissingIsEmpty(t *testing.T) {
	got, err := loadStateFromPath(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got.Credentials.AccessKey != "" || got.Credentials.SecretKey != "" {
		t.Fatalf("expected empty credentials, got %+v", got.Credentials)
	}
}

// TestGenerateCredentials_Random asserts minted credentials are well-formed and
// non-repeating.
func TestGenerateCredentials_Random(t *testing.T) {
	a, err := generateCredentials()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := generateCredentials()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(a.AccessKey) != accessKeyLen || len(a.SecretKey) != secretKeyLen {
		t.Fatalf("unexpected key lengths: access=%d secret=%d", len(a.AccessKey), len(a.SecretKey))
	}
	if a.AccessKey == b.AccessKey || a.SecretKey == b.SecretKey {
		t.Fatalf("credentials are not random: two mints collided")
	}
	for _, c := range a.AccessKey + a.SecretKey {
		if !strings.ContainsRune(credCharset, c) {
			t.Fatalf("credential contains char %q outside charset", c)
		}
	}
}

// TestResolveBackingDir_Bounding asserts the backing dir is bounded to
// <home>/.citadel/storage and anything outside is rejected.
func TestResolveBackingDir_Bounding(t *testing.T) {
	home := "/home/tester"
	base := filepath.Join(home, ".citadel", "storage")

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"empty defaults to data dir", "", filepath.Join(base, "data"), false},
		{"base itself", base, base, false},
		{"subdir of base", filepath.Join(base, "data"), filepath.Join(base, "data"), false},
		{"tilde-expanded inside base", "~/.citadel/storage/objects", filepath.Join(base, "objects"), false},
		{"outside: home ssh", "~/.ssh", "", true},
		{"outside: etc", "/etc", "", true},
		{"escape via ..", filepath.Join(base, "..", "..", "etc"), "", true},
		{"sibling prefix trick", base + "-evil", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBackingDir(tc.raw, home)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("resolveBackingDir(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestResolveBackingDir_NoHome asserts a missing home dir is a hard error.
func TestResolveBackingDir_NoHome(t *testing.T) {
	if _, err := resolveBackingDir("", ""); err == nil {
		t.Fatal("expected error when home dir is unknown")
	}
}
