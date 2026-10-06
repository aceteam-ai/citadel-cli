package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testAPIKey = "act_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSaveLoadRoundTrip_UserOnlyPerms(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		APIKey:     testAPIKey,
		APIBaseURL: "https://aceteam.ai",
		OrgID:      "org_1",
		OrgName:    "Acme",
		Scopes:     []string{"memory:read", "memory:write"},
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %o", info.Mode().Perm())
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.APIKey != testAPIKey || got.OrgName != "Acme" || len(got.Scopes) != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestRemoveSerializesWithSaveLock(t *testing.T) {
	dir := t.TempDir()
	path := ConfigPath(dir)
	if err := Save(dir, &Config{APIKey: "act_existing"}); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- withFileLock(path, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	// An arbitrarily old timestamp must not allow a contender to bypass a live
	// kernel lock. The former stale-file algorithm did exactly that.
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path+".citadel.lock", old, old); err != nil {
		t.Fatal(err)
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- Remove(dir) }()
	select {
	case err := <-removeDone:
		t.Fatalf("Remove bypassed Save lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential remains after Remove: %v", err)
	}
}

func TestFileLockReleasedWhenOwnerProcessExits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.yaml")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFileLockCrashHelper$")
	cmd.Env = append(os.Environ(), "CITADEL_TEST_CRASH_LOCK="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v: %s", err, out)
	}
	if err := withFileLock(path, func() error { return nil }); err != nil {
		t.Fatalf("kernel did not release lock after process exit: %v", err)
	}
	if _, err := os.Stat(path + ".citadel.lock"); err != nil {
		t.Fatalf("persistent sidecar missing: %v", err)
	}
}

func TestFileLockCrashHelper(t *testing.T) {
	path := os.Getenv("CITADEL_TEST_CRASH_LOCK")
	if path == "" {
		return
	}
	if err := withFileLock(path, func() error {
		os.Exit(0) // exercise crash-release: deferred unlock/close do not run
		return nil
	}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func TestFileLockRejectsSymlinkSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.yaml")
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, path+".citadel.lock"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := withFileLock(path, func() error { return nil }); err == nil {
		t.Fatal("symlink lock sidecar was accepted")
	}
	if got, _ := os.ReadFile(sentinel); string(got) != "unchanged" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestAtomicJSONWrite_DoesNotUsePredictableTemporaryPath(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	// This was the old predictable temporary name used by JSON writes. A
	// random same-directory CreateTemp name must leave it untouched.
	path := filepath.Join(dir, "claude.json")
	trap := path + ".citadel.tmp"
	if err := os.Symlink(sentinel, trap); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := writeJSONObject(path, map[string]any{"safe": true}, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "do not overwrite" {
		t.Fatalf("predictable temp trap was modified: data=%q err=%v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".claude.json.tmp-") {
			t.Fatalf("temporary file leaked after successful atomic write: %s", entry.Name())
		}
	}
}

func TestAtomicWrite_ReplacesDestinationSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	path := filepath.Join(dir, "memory.yaml")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := atomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(sentinel); string(got) != "unchanged" {
		t.Fatalf("symlink target changed: %q", got)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("replacement content = %q", got)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination remains symlink: info=%v err=%v", info, err)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil config for missing file, got %+v", got)
	}
}

func TestLoadRefusesInsecureOrLinkedCredential(t *testing.T) {
	dir := t.TempDir()
	path := ConfigPath(dir)
	content := []byte("api_key: act_not_for_other_users\nscopes:\n  - memory:read\n  - memory:write\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "insecure permissions") {
			t.Fatalf("world-readable credential accepted: %v", err)
		}
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("linked credential accepted: %v", err)
	}
}

func TestEffectiveMCPURL(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{APIBaseURL: "https://aceteam.ai"}, "https://aceteam.ai/api/mcp/aceteam/memory/mcp"},
		{Config{APIBaseURL: "https://aceteam.ai/"}, "https://aceteam.ai/api/mcp/aceteam/memory/mcp"},
		{Config{}, "https://aceteam.ai/api/mcp/aceteam/memory/mcp"},
		{Config{APIBaseURL: "http://127.0.0.1:3000/"}, "http://127.0.0.1:3000/api/mcp/aceteam/memory/mcp"},
		{Config{MCPURL: "https://aceteam.ai/api/mcp/aceteam/mcp"}, "https://aceteam.ai/api/mcp/aceteam/mcp"},
		{Config{MCPURL: "https://custom/mcp"}, "https://custom/mcp"},
	}
	for _, c := range cases {
		if got := c.cfg.EffectiveMCPURL(); got != c.want {
			t.Errorf("EffectiveMCPURL(%+v)=%q want %q", c.cfg, got, c.want)
		}
	}
}

func TestValidateScopes_ExactLeastPrivilegeSet(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		ok     bool
	}{
		{name: "canonical", scopes: []string{ScopeRead, ScopeWrite}, ok: true},
		{name: "reverse order", scopes: []string{ScopeWrite, ScopeRead}, ok: true},
		{name: "missing", scopes: []string{ScopeRead}},
		{name: "empty", scopes: nil},
		{name: "extra", scopes: []string{ScopeRead, ScopeWrite, "node:read"}},
		{name: "wildcard", scopes: []string{ScopeRead, "*"}},
		{name: "admin", scopes: []string{ScopeRead, "admin"}},
		{name: "duplicate", scopes: []string{ScopeRead, ScopeRead}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateScopes(tt.scopes)
			if tt.ok && err != nil {
				t.Fatalf("ValidateScopes(%v): %v", tt.scopes, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("ValidateScopes(%v) unexpectedly succeeded", tt.scopes)
			}
		})
	}
}

func TestValidateCredential_KeyAndEndpoint(t *testing.T) {
	base := Config{APIKey: testAPIKey, Scopes: []string{ScopeRead, ScopeWrite}}
	if err := base.ValidateCredential(); err != nil {
		t.Fatalf("valid credential rejected: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{name: "short key", mut: func(c *Config) { c.APIKey = "act_short" }},
		{name: "uppercase key", mut: func(c *Config) { c.APIKey = "act_" + strings.Repeat("A", 64) }},
		{name: "remote plaintext", mut: func(c *Config) { c.MCPURL = "http://example.test/mcp" }},
		{name: "userinfo", mut: func(c *Config) { c.MCPURL = "https://user:pass@example.test/mcp" }},
		{name: "query", mut: func(c *Config) { c.MCPURL = "https://example.test/mcp?token=x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mut(&cfg)
			if err := cfg.ValidateCredential(); err == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}

	loopback := base
	loopback.MCPURL = "http://127.0.0.1:8080/mcp"
	if err := loopback.ValidateCredential(); err != nil {
		t.Fatalf("loopback development endpoint rejected: %v", err)
	}
}
