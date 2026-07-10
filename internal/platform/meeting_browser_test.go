package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClickJS_EscapesSelectorAndThrows(t *testing.T) {
	// A selector containing a quote must be safely embedded (not break the JS)
	// and the snippet must throw when the element is missing so a stale selector
	// surfaces as an error rather than a false success.
	sel := `button[aria-label="Leave call"]`
	js := clickJS(sel)
	marshaled, _ := json.Marshal(sel)
	if !strings.Contains(js, string(marshaled)) {
		t.Errorf("clickJS did not embed json-escaped selector; got: %s", js)
	}
	if !strings.Contains(js, "throw new Error") {
		t.Errorf("clickJS must throw on missing selector; got: %s", js)
	}
	if !strings.Contains(js, ".click()") {
		t.Errorf("clickJS must click the element; got: %s", js)
	}
}

func TestTypeJS_EscapesSelectorAndText(t *testing.T) {
	sel := `input[aria-label="Your name"]`
	text := `O'Brien "Bot" \ x`
	js := typeJS(sel, text)
	for _, want := range []string{string(mustJSON(t, sel)), string(mustJSON(t, text))} {
		if !strings.Contains(js, want) {
			t.Errorf("typeJS missing escaped %q; got: %s", want, js)
		}
	}
	if !strings.Contains(js, "throw new Error") {
		t.Errorf("typeJS must throw on missing selector; got: %s", js)
	}
	// Uses the native setter path so controlled (React) inputs observe the change.
	if !strings.Contains(js, "getOwnPropertyDescriptor") || !strings.Contains(js, "dispatchEvent") {
		t.Errorf("typeJS must set value via native setter + dispatch events; got: %s", js)
	}
}

func TestDescribeCDPException(t *testing.T) {
	cases := []struct {
		name string
		exc  any
		want string
	}{
		{
			name: "exception description",
			exc:  map[string]any{"exception": map[string]any{"description": "Error: selector not found"}},
			want: "Error: selector not found",
		},
		{
			name: "exception value fallback",
			exc:  map[string]any{"exception": map[string]any{"value": "boom"}},
			want: "boom",
		},
		{
			name: "text fallback",
			exc:  map[string]any{"text": "Uncaught"},
			want: "Uncaught",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeCDPException(tc.exc); got != tc.want {
				t.Errorf("describeCDPException = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFindFreeDebugPort(t *testing.T) {
	p, err := findFreeDebugPort()
	if err != nil {
		t.Fatalf("findFreeDebugPort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("findFreeDebugPort returned out-of-range port %d", p)
	}
}

// TestMeetingBrowser_Launch is an integration test: it launches a real Chromium
// on a real Xvfb display and drives one CDP round-trip. It skips under -short and
// wherever the browser/display deps are missing, mirroring audio_test.go's
// hardware-gated convention.
func TestMeetingBrowser_Launch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping meeting-browser launch test in -short mode")
	}
	if !ChromiumAvailable() || !XvfbAvailable() {
		t.Skip("no Chromium or Xvfb on this host; skipping meeting-browser launch test")
	}
	// Isolate this run to a throwaway profile dir so the integration test never
	// touches (or depends on) a real seeded bot profile under ConfigDir().
	br := NewMeetingBrowser("citadel_meeting_gotest", filepath.Join(t.TempDir(), "meeting-profile"))
	if err := br.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer br.Close()
	if br.DebugPort() <= 0 {
		t.Fatalf("expected a CDP debug port after Start, got %d", br.DebugPort())
	}
	v, err := br.Evaluate("1+1")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if n, ok := v.(float64); !ok || n != 2 {
		t.Fatalf("Evaluate(1+1) = %v (%T), want 2", v, v)
	}
	// A throwing expression must surface as a Go error, not a silent success.
	if _, err := br.Evaluate(`throw new Error("nope")`); err == nil {
		t.Fatal("expected error from throwing JS expression, got nil")
	}
}

// TestResolveMeetingProfileDir_Precedence checks the persistent-profile-dir
// override chain (issue #5122): an explicit per-browser override beats
// EnvMeetingProfileDir, which beats the ConfigDir()-rooted default. Pure
// (no filesystem I/O), so it does not need t.TempDir().
func TestResolveMeetingProfileDir_Precedence(t *testing.T) {
	t.Setenv(EnvMeetingProfileDir, "/env/profile")

	if got := resolveMeetingProfileDir("/override/profile"); got != "/override/profile" {
		t.Errorf("override should win over env var; got %q", got)
	}
	if got := resolveMeetingProfileDir(""); got != "/env/profile" {
		t.Errorf("env var should win over default when override is unset; got %q", got)
	}

	t.Setenv(EnvMeetingProfileDir, "")
	if got := resolveMeetingProfileDir(""); got != defaultMeetingProfileDir() {
		t.Errorf("expected ConfigDir()-rooted default when override and env are both unset; got %q, want %q",
			got, defaultMeetingProfileDir())
	}
}

// TestMeetingBrowser_IsGoogleSignInURL checks the deterministic signed-out signal: any URL
// hosted on accounts.google.com is a sign-in redirect, everything else
// (including a real Meet URL, and unparseable input) is not.
func TestMeetingBrowser_IsGoogleSignInURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"signin identifier page", "https://accounts.google.com/signin/v2/identifier?service=meet", true},
		{"bare accounts host", "https://accounts.google.com/", true},
		{"case-insensitive host", "https://ACCOUNTS.GOOGLE.COM/signin", true},
		{"real meet url", "https://meet.google.com/abc-defg-hij", false},
		{"unrelated google host", "https://mail.google.com/mail/u/0/", false},
		{"empty string", "", false},
		{"unparseable url", "http://[::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGoogleSignInURL(tc.url); got != tc.want {
				t.Errorf("IsGoogleSignInURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

// TestMeetingProfileDir_CreatesWithOwnerOnlyPerms verifies a
// freshly-created meeting profile dir is locked to 0700 — it will hold real
// Google session cookies for the bot account (issue #5122).
func TestMeetingProfileDir_CreatesWithOwnerOnlyPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits not meaningful on windows")
	}
	target := filepath.Join(t.TempDir(), "nested", "meeting-profile")

	got, err := preparePersistentProfileDir(target)
	if err != nil {
		t.Fatalf("preparePersistentProfileDir: %v", err)
	}
	if got != target {
		t.Fatalf("preparePersistentProfileDir returned %q, want %q", got, target)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat profile dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("expected owner-only 0700 perms, got %o", perm)
	}
}

// TestMeetingProfileDir_ReusesExistingDirAndTightensLoosePerms
// covers the "reuse across runs" contract: a pre-existing, already-seeded
// profile directory keeps its contents (the seeded Google session), and
// looser-than-0700 permissions on an existing dir (e.g. left over from manual
// seeding under a permissive umask) are tightened rather than trusted.
func TestMeetingProfileDir_ReusesExistingDirAndTightensLoosePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits not meaningful on windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod setup: %v", err)
	}
	// Simulate seeded profile content (Chrome's cookie DB lives under Default/).
	seeded := filepath.Join(dir, "Default", "Cookies")
	if err := os.MkdirAll(filepath.Dir(seeded), 0o700); err != nil {
		t.Fatalf("seed setup: %v", err)
	}
	if err := os.WriteFile(seeded, []byte("fake-session-cookie"), 0o600); err != nil {
		t.Fatalf("seed setup: %v", err)
	}

	got, err := preparePersistentProfileDir(dir)
	if err != nil {
		t.Fatalf("preparePersistentProfileDir: %v", err)
	}
	if got != dir {
		t.Fatalf("preparePersistentProfileDir returned %q, want %q (existing dir must be reused, not replaced)", got, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat profile dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("expected loose 0755 perms tightened to 0700, got %o", perm)
	}
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("expected pre-seeded profile content to survive reuse, got: %v", err)
	}
}

// TestMeetingBrowser_CloseDoesNotRemoveProfileDir is the regression test for
// the orphan-profile leak fix (issue #5122): the old MkdirTemp-based
// closeLocked unconditionally os.RemoveAll'd the profile. Now that the
// profile is the persistent, human-seeded bot session, Close() must leave it
// on disk untouched — only the in-memory handle is cleared.
func TestMeetingBrowser_CloseDoesNotRemoveProfileDir(t *testing.T) {
	dir := t.TempDir()
	seeded := filepath.Join(dir, "Default", "Cookies")
	if err := os.MkdirAll(filepath.Dir(seeded), 0o700); err != nil {
		t.Fatalf("seed setup: %v", err)
	}
	if err := os.WriteFile(seeded, []byte("fake-session-cookie"), 0o600); err != nil {
		t.Fatalf("seed setup: %v", err)
	}

	// Construct directly (no real Start()) so this stays a pure filesystem
	// test; closeLocked's process/Xvfb teardown paths are all nil-safe.
	br := &MeetingBrowser{profileDir: dir}
	if err := br.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("expected persistent profile content to survive Close(), got: %v", err)
	}
	if got := br.ProfileDir(); got != "" {
		t.Errorf("expected in-memory ProfileDir cleared after Close, got %q", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%v): %v", v, err)
	}
	return b
}
