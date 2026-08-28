package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
// TestBuildMeetingChromeArgs_PasswordStoreBasic locks in the actual fix (issue
// #5122 os_crypt mismatch): the meeting bot's launch MUST pass
// --password-store=basic so its persistent, externally-seeded profile uses
// Chromium's build-independent cookie-encryption key instead of a keyring secret.
// This covers the load-bearing Start() choice, which the buildChromeArgs-level
// test cannot (that only proves the flag is emitted WHEN the option is set).
func TestBuildMeetingChromeArgs_PasswordStoreBasic(t *testing.T) {
	args := buildMeetingChromeArgs(9222, "/tmp/meeting-profile")
	if !containsArg(args, "--password-store=basic") {
		t.Errorf("meeting launch must include --password-store=basic, got %v", args)
	}
	// The persistent profile dir must still be wired through.
	if !containsArg(args, "--user-data-dir=/tmp/meeting-profile") {
		t.Errorf("meeting launch missing --user-data-dir, got %v", args)
	}
}

// TestBuildMeetingChromeArgs_AutoplayNoUserGesture locks in the audio-capture fix
// (issue #5098): an automated browser has no user gesture, so without this flag
// Chrome keeps the AudioContext suspended and blocks remote (WebRTC) audio, and
// the bot records pure silence. The meeting launch MUST relax the autoplay policy.
func TestBuildMeetingChromeArgs_AutoplayNoUserGesture(t *testing.T) {
	args := buildMeetingChromeArgs(9222, "/tmp/meeting-profile")
	if !containsArg(args, "--autoplay-policy=no-user-gesture-required") {
		t.Errorf("meeting launch must include --autoplay-policy=no-user-gesture-required, got %v", args)
	}
}

func TestMeetingBrowser_Launch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping meeting-browser launch test in -short mode")
	}
	// Launching a real headed browser under Xvfb is unreliable in CI containers
	// even when the chrome/Xvfb *binaries* are present: the DinD runners have no
	// working display/sandbox, so Start() fails rather than the binary-presence
	// guard below skipping. Binary presence is therefore not a safe trigger.
	// Require an explicit opt-in so `go test ./...` in CI always skips this; run
	// it deliberately on a real node with CITADEL_BROWSER_INTEGRATION=1.
	if os.Getenv("CITADEL_BROWSER_INTEGRATION") == "" {
		t.Skip("set CITADEL_BROWSER_INTEGRATION=1 to run the meeting-browser launch integration test")
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

// TestMeetingBrowser_IsMicrosoftSignInURL checks the Teams anonymous-join gate
// (issue #7000): a redirect to a Microsoft identity host means the meeting is
// refusing a guest web join, everything else (a real Teams URL, other MS hosts,
// unparseable input) is not.
func TestMeetingBrowser_IsMicrosoftSignInURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"entra work/school login", "https://login.microsoftonline.com/common/oauth2/authorize?x=1", true},
		{"personal MS account login", "https://login.live.com/login.srf", true},
		{"login.microsoft.com", "https://login.microsoft.com/", true},
		{"case-insensitive host", "https://LOGIN.MICROSOFTONLINE.COM/common", true},
		{"tenant subdomain of login host", "https://foo.login.microsoftonline.com/x", true},
		{"real teams meet url", "https://teams.microsoft.com/meet/1234567890?p=AbCdEf", false},
		{"real teams meetup-join url", "https://teams.microsoft.com/l/meetup-join/19%3ameeting_x", false},
		{"lookalike host is not spoofable", "https://evil-login.microsoftonline.com.attacker.test/", false},
		{"unrelated ms host", "https://outlook.office.com/mail/", false},
		{"empty string", "", false},
		{"unparseable url", "http://[::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMicrosoftSignInURL(tc.url); got != tc.want {
				t.Errorf("IsMicrosoftSignInURL(%q) = %v, want %v", tc.url, got, tc.want)
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

// TestAcquireMeetingProfileLock is the regression test for the citadel-cli#895
// review finding: two concurrent host-media Starts against the SAME
// persistent profile directory must not both proceed. Chrome's own
// --user-data-dir lock silently FORWARDS a second launch into the first,
// still-live meeting's browser instead of failing -- disrupting an
// in-progress call -- so this guard must fail the second attempt fast and
// clearly instead. This exercises the lock-acquisition seam directly
// (acquireMeetingProfileLock), not a real MeetingBrowser.Start(), so it is
// hermetic: no Chrome/Xvfb binary required, no real browser launched.
func TestAcquireMeetingProfileLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meeting-profile")

	// First caller (the "first meeting") claims the profile.
	release1, err := acquireMeetingProfileLock(dir)
	if err != nil {
		t.Fatalf("first acquire: unexpected error: %v", err)
	}
	if release1 == nil {
		t.Fatal("first acquire: expected a non-nil release func")
	}

	// A second, overlapping caller against the SAME dir must fail immediately
	// with a clear reason -- never block (a queued meeting waiting up to 4h
	// for the first to finish would already be over), and never silently
	// proceed into Chrome's launch-forwarding collision.
	release2, err := acquireMeetingProfileLock(dir)
	if err == nil {
		if release2 != nil {
			release2()
		}
		t.Fatal("second acquire against the same profile dir should have failed fast, got nil error")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("expected a clear 'already in use' error, got: %v", err)
	}
	if release2 != nil {
		t.Error("expected a nil release func on a failed acquire")
	}

	// The first caller must be entirely unaffected by the second's failed
	// attempt: it can still release normally.
	release1()

	// Once released, a third acquire against the same dir must succeed --
	// proving the failure above was contention, not a permanently poisoned lock.
	release3, err := acquireMeetingProfileLock(dir)
	if err != nil {
		t.Fatalf("acquire after release: unexpected error: %v", err)
	}
	defer release3()

	// A DIFFERENT profile dir must never contend with this one (distinct test
	// fixtures / a deliberately multi-profile deployment must not collide).
	otherDir := filepath.Join(t.TempDir(), "other-meeting-profile")
	releaseOther, err := acquireMeetingProfileLock(otherDir)
	if err != nil {
		t.Fatalf("acquire on a different profile dir should not contend, got: %v", err)
	}
	releaseOther()
}

// TestAcquireMeetingProfileLock_Concurrent races real goroutines against the
// same profile dir (rather than the deterministic sequential ordering in
// TestAcquireMeetingProfileLock above) so `go test -race` exercises the
// actual mutex under contention, matching the literal "two concurrent Starts"
// shape of the regression this guards against.
func TestAcquireMeetingProfileLock_Concurrent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meeting-profile")

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	releases := make([]func(), n)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait() // release all goroutines together to maximize contention
			release, err := acquireMeetingProfileLock(dir)
			errs[i] = err
			releases[i] = release
		}(i)
	}
	start.Done()
	wg.Wait()

	var succeeded, failed int
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			succeeded++
			if releases[i] == nil {
				t.Errorf("goroutine %d: succeeded but got a nil release func", i)
			}
		} else {
			failed++
			if releases[i] != nil {
				t.Errorf("goroutine %d: failed but got a non-nil release func", i)
			}
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent acquires to succeed, got %d", n, succeeded)
	}
	if failed != n-1 {
		t.Fatalf("expected %d failures, got %d", n-1, failed)
	}
	for _, release := range releases {
		if release != nil {
			release()
		}
	}
}

// TestAcquireMeetingProfileLock_ReleaseIsIdempotent pins that the returned
// release func can be called more than once safely. MeetingBrowser's
// closeLocked() is itself documented safe to call repeatedly (Close() ->
// closeLocked(), and the waitForCDPReady failure path inside Start() also
// routes there), so a double-release must never panic ("sync: unlock of
// unlocked mutex") or leave the lock in an inconsistent state.
func TestAcquireMeetingProfileLock_ReleaseIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meeting-profile")

	release, err := acquireMeetingProfileLock(dir)
	if err != nil {
		t.Fatalf("acquire: unexpected error: %v", err)
	}

	release()
	release() // must not panic

	// The lock must be genuinely free after release, not left held by the
	// no-op second release call.
	release2, err := acquireMeetingProfileLock(dir)
	if err != nil {
		t.Fatalf("acquire after idempotent release: unexpected error: %v", err)
	}
	release2()
}

// TestMeetingBrowser_CloseReleasesProfileLock pins the MeetingBrowser-level
// wiring (not just the standalone acquireMeetingProfileLock seam above):
// closeLocked() must release whatever lock Start() attached to
// profileLockRelease, so a second MeetingBrowser can claim the same profile
// dir once the first is closed. Constructed directly (no real Start()), same
// hermetic pattern as TestMeetingBrowser_CloseDoesNotRemoveProfileDir.
func TestMeetingBrowser_CloseReleasesProfileLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meeting-profile")

	release, err := acquireMeetingProfileLock(dir)
	if err != nil {
		t.Fatalf("acquire: unexpected error: %v", err)
	}

	br := &MeetingBrowser{profileDir: dir, profileLockRelease: release}

	// While br "holds" the profile (simulating a live meeting), a second
	// acquire must fail -- exactly the collision this guard exists to prevent.
	if _, err := acquireMeetingProfileLock(dir); err == nil {
		t.Fatal("expected the profile to be reported busy while br holds it")
	}

	if err := br.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if br.profileLockRelease != nil {
		t.Error("expected profileLockRelease to be cleared after Close")
	}

	// Now that br released it, a new acquire must succeed.
	release2, err := acquireMeetingProfileLock(dir)
	if err != nil {
		t.Fatalf("acquire after Close: unexpected error: %v", err)
	}
	release2()

	// Close() is documented safe to call more than once; must not panic on
	// the already-nil profileLockRelease.
	if err := br.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
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
