// internal/platform/meeting_browser.go
//
// Meeting-bot browser: a headed Chromium the notetaker drives over CDP to join a
// video call, launched so its audio routes into a per-meeting PulseAudio null
// sink for capture (issue #5098, epic #5097 — the sovereign auto-join notetaker).
//
// This is a deliberate SIBLING of CobrowseManager, not a reuse of it. The
// co-browse manager is a process-wide singleton owning ONE long-lived browser
// that a human logs into and the AI keeps steering. A meeting bot needs a
// short-lived browser PROCESS per meeting, isolated from the co-browse session
// so the two never fight over one Chromium — but (issue #5122) it now shares
// co-browse's other trait: a PERSISTENT profile, not a throwaway one. Google
// policy-rejects anonymous meeting participants in many orgs, so the bot needs
// a real, signed-in Google identity (notetaker@aceteam.ai) whose session
// cookies survive across meetings; a human seeds that session once by hand
// (docs/meeting-bot-profile-seeding.md — Google blocks automated login) and
// every MEETING_JOIN thereafter reuses it. Chrome still locks a
// --user-data-dir to one process, so co-browse and the meeting bot still
// cannot share a profile with EACH OTHER, and only one meeting can use the bot
// profile at a time. MeetingBrowser therefore owns its OWN Xvfb display, CDP
// debug port, and persistent profile dir, and reuses only the package-level,
// side-effect-free launch helpers (buildChromeArgs, startManagedXvfb,
// withDisplay, findChromium, pickTarget, cdpCommand) so there is no duplicated
// browser-launch logic.
package platform

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvMeetingProfileDir overrides the default persistent Chrome profile
// directory for the meeting bot's signed-in Google account (issue #5122).
// Unlike co-browse's throwaway-friendly profile, this one is deliberately
// reused across every meeting: a human seeds it ONCE with a manual, real
// sign-in to the bot's Google account (see docs/meeting-bot-profile-seeding.md
// — automated Google login is detection-blocked, so this cannot be scripted),
// and every subsequent MEETING_JOIN reuses the same cookies/session rather
// than joining as an anonymous, easily-rejected participant. Set this when a
// node's persistent state should live somewhere other than the default
// (e.g. a dedicated data volume).
const EnvMeetingProfileDir = "CITADEL_MEETING_PROFILE_DIR"

// defaultMeetingProfileDirName is the directory name under ConfigDir() that
// holds the persistent meeting-bot Chrome profile when EnvMeetingProfileDir is
// unset.
const defaultMeetingProfileDirName = "meeting-profile"

// defaultMeetingProfileDir resolves the default persistent profile path,
// following the same node-local persistent-state convention as the rest of
// citadel (ConfigDir() also backs ~/.citadel-cli/tls, /logs, /gateway).
func defaultMeetingProfileDir() string {
	return filepath.Join(ConfigDir(), defaultMeetingProfileDirName)
}

// resolveMeetingProfileDir picks the effective profile directory: an explicit
// per-browser override wins (set via NewMeetingBrowser), then
// EnvMeetingProfileDir, then the default under ConfigDir(). Pure aside from
// reading the environment, so precedence is unit-testable without touching
// the filesystem.
func resolveMeetingProfileDir(override string) string {
	if override != "" {
		return override
	}
	if v := strings.TrimSpace(os.Getenv(EnvMeetingProfileDir)); v != "" {
		return v
	}
	return defaultMeetingProfileDir()
}

// preparePersistentProfileDir resolves the effective meeting-bot profile
// directory and ensures it exists, locked down to owner-only permissions —
// it holds real Google session cookies for the bot account (issue #5122).
// Idempotent and safe to call every Start(): an already-seeded profile is
// reused as-is (its contents are untouched), and a pre-existing directory
// with looser permissions (e.g. created by an older citadel-cli build, or by
// hand during seeding with a stray umask) is tightened rather than trusted.
// Extracted from Start so the resolution + permission-lock logic is testable
// without launching a real browser.
func preparePersistentProfileDir(override string) (string, error) {
	dir := resolveMeetingProfileDir(override)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create meeting profile dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("lock down meeting profile dir permissions: %w", err)
	}
	return dir, nil
}

// IsGoogleSignInURL reports whether rawURL is a Google account authentication
// page (accounts.google.com), the reliable, deterministic signal that the
// meeting bot's persistent Chrome profile has lost its signed-in session and
// Meet has redirected it to log in. Used by the join flow to fail loudly with
// an actionable "re-seed the profile" error instead of continuing the join as
// an unauthenticated (and often policy-rejected) anonymous participant. A
// malformed URL is treated as "not a sign-in page" (returns false) so a
// transient CDP read glitch never masquerades as a signed-out profile.
func IsGoogleSignInURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "accounts.google.com")
}

// microsoftSignInHosts are the Microsoft identity-platform hosts Teams redirects
// to when a meeting requires an authenticated (non-guest) sign-in. login.live.com
// covers personal Microsoft accounts; login.microsoftonline.com covers work/school
// (Entra ID) tenants.
var microsoftSignInHosts = []string{
	"login.microsoftonline.com",
	"login.live.com",
	"login.microsoft.com",
}

// IsMicrosoftSignInURL reports whether rawURL is a Microsoft account / Entra ID
// authentication page (login.microsoftonline.com and friends) — the signal that
// the Teams meeting is refusing an anonymous/guest web join and is demanding an
// authenticated Microsoft profile the bot does not (and by design should not)
// have. The Teams join flow (issue #7000) PREFERS anonymous/guest join precisely
// to avoid needing an MS profile; when a meeting forces sign-in the flow fails
// loudly with an actionable error instead of stalling at a login wall. A
// malformed URL returns false (see IsGoogleSignInURL). Host match is exact or a
// subdomain suffix so a lookalike path cannot spoof it.
func IsMicrosoftSignInURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, base := range microsoftSignInHosts {
		if host == base || strings.HasSuffix(host, "."+base) {
			return true
		}
	}
	return false
}

// ChromiumAvailable reports whether a Chromium/Chrome binary is on PATH. Exported
// so capability detection can gate the `meeting` tag on a launchable browser
// without reaching into this package's unexported findChromium.
func ChromiumAvailable() bool {
	_, err := findChromium()
	return err == nil
}

// XvfbAvailable reports whether the Xvfb binary is on PATH. The meeting browser
// always runs on a dedicated virtual display (meeting nodes are typically
// headless), so Xvfb is a hard dependency of the `meeting` capability.
func XvfbAvailable() bool {
	return isCommandAvailable("Xvfb")
}

// AudioStackAvailable is the exported form of audioStackAvailable so capability
// detection (a different package) can gate the `meeting` tag on a working
// PulseAudio + ffmpeg + pactl stack.
func AudioStackAvailable() bool {
	return audioStackAvailable()
}

// findFreeDebugPort asks the kernel for an unused loopback TCP port so a meeting
// browser's CDP endpoint never collides with co-browse's fixed 9222 or with a
// second concurrent meeting. There is a small window between closing the probe
// listener and Chromium binding the port; acceptable because the launcher fails
// loudly (waitForCDPReady times out) rather than silently attaching to a ghost.
func findFreeDebugPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve free CDP port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForCDPReady polls the CDP HTTP endpoint until a page target appears or the
// timeout elapses. Package-level (not a MeetingBrowser method) so it is reusable
// and stays free of manager state.
func waitForCDPReady(debugPort int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := pickTarget(debugPort); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("CDP endpoint not ready after %s: %v", timeout, lastErr)
}

// clickJS builds a JS expression that clicks the first element matching selector
// and THROWS when nothing matches. The throw is load-bearing: cdpEvaluate turns a
// JS exception into a Go error, so a stale selector fails loudly during live
// tuning instead of silently reporting a successful click. selector is embedded
// via json.Marshal so any quotes/backslashes are safely escaped.
func clickJS(selector string) string {
	sel, _ := json.Marshal(selector)
	return fmt.Sprintf(
		`(function(){var el=document.querySelector(%s);`+
			`if(!el){throw new Error("selector not found: "+%s);}`+
			`el.scrollIntoView();el.click();return true;})()`,
		sel, sel)
}

// typeJS builds a JS expression that focuses the first element matching selector
// and sets its value using the native value setter (so React/Angular-controlled
// inputs, like Meet's name field, observe the change), then dispatches input and
// change events. Throws when the selector matches nothing (see clickJS). Both
// selector and text are json.Marshal-escaped.
func typeJS(selector, text string) string {
	sel, _ := json.Marshal(selector)
	val, _ := json.Marshal(text)
	return fmt.Sprintf(
		`(function(){var el=document.querySelector(%s);`+
			`if(!el){throw new Error("selector not found: "+%s);}`+
			`el.focus();`+
			`var proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;`+
			`var setter=Object.getOwnPropertyDescriptor(proto,'value').set;`+
			`setter.call(el,%s);`+
			`el.dispatchEvent(new Event('input',{bubbles:true}));`+
			`el.dispatchEvent(new Event('change',{bubbles:true}));return true;})()`,
		sel, sel, val)
}

// cdpEvaluate runs a JS expression in the page and returns its by-value result.
//
// It hardens the raw cdpCommand in two ways that matter for the join flow:
//   - returnByValue:true so the caller reads an actual JSON value (a bool, a
//     number, a string) rather than an opaque RemoteObject handle.
//   - It inspects result.exceptionDetails and returns a Go error on a JS throw.
//     cdpCommand only surfaces PROTOCOL errors (msg["error"]); a JS runtime
//     exception comes back as a normal result, so without this a throwing click
//     on a missing selector would masquerade as success — the worst outcome for
//     the human tuning selectors against a live Meet.
func cdpEvaluate(debugPort int, expression string) (any, error) {
	return cdpEvalValue(cdpCommand(debugPort, "Runtime.evaluate", runtimeEvalParams(expression)))
}

// runtimeEvalParams is the Runtime.evaluate parameter set shared by the host and
// container (published-port) evaluate paths.
func runtimeEvalParams(expression string) map[string]any {
	return map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}
}

// cdpEvalValue extracts the by-value result of a Runtime.evaluate response,
// surfacing a JS throw as a Go error (see cdpEvaluate's contract). Takes the raw
// (res, err) of a CDP command so both the host and published-port evaluate
// helpers share the exception handling verbatim.
func cdpEvalValue(res map[string]any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if exc, ok := res["exceptionDetails"]; ok && exc != nil {
		return nil, fmt.Errorf("javascript exception: %s", describeCDPException(exc))
	}
	result, ok := res["result"].(map[string]any)
	if !ok {
		return nil, nil
	}
	return result["value"], nil
}

// describeCDPException pulls a human-readable message out of a CDP
// exceptionDetails object, preferring the thrown exception's description.
func describeCDPException(exc any) string {
	m, ok := exc.(map[string]any)
	if !ok {
		return fmt.Sprint(exc)
	}
	if e, ok := m["exception"].(map[string]any); ok {
		if d, ok := e["description"].(string); ok && d != "" {
			return d
		}
		if v, ok := e["value"]; ok {
			return fmt.Sprint(v)
		}
	}
	if t, ok := m["text"].(string); ok && t != "" {
		return t
	}
	return fmt.Sprint(m)
}

// MeetingBrowser owns one disposable headed Chromium for a single meeting, its
// dedicated Xvfb display, and the notetaker's PERSISTENT, signed-in Chrome
// profile (issue #5122). Only the browser process and its Xvfb display are
// disposable now — the profile directory deliberately survives Close() so the
// bot's Google session (seeded once, by hand; see
// docs/meeting-bot-profile-seeding.md) carries over to the next meeting. Chrome
// locks a --user-data-dir to one process, so two MeetingBrowsers sharing the
// same profile cannot run concurrently: the bot account can be in at most one
// meeting at a time. Safe for concurrent use; the reaper goroutines own the
// single Wait() for each child process, mirroring CobrowseManager's process
// handling.
//
// That "cannot run concurrently" sentence used to be enforced only
// INCIDENTALLY, by whatever serialized the callers (e.g. a maxConcurrency=1
// job runner). It is now enforced HERE directly, at the resource itself (issue
// #895 review, a citadel-cli#489 follow-up): Start() claims a process-wide,
// per-profile-dir lock (see acquireMeetingProfileLock) before touching
// anything, and closeLocked() releases it. Without this, a second concurrent
// Start() against the same profile reaches Chrome's own --user-data-dir lock:
// Chrome silently FORWARDS the launch to the already-running instance and
// exits immediately, so the second caller's CDP port never comes up (a slow,
// opaque waitForCDPReady timeout) AND the forwarded launch can navigate/click
// inside the FIRST, still-live meeting's browser — disrupting an in-progress
// call. Guarding here protects against that collision from ANY caller, not
// just one dispatch path.
//
// A caller that already holds this SAME lock across a setup phase preceding
// Start() (internal/jobs.hostMedia.Start(), which marks the profile owned and
// sweeps orphaned audio sinks before the browser launches — see
// AcquireMeetingProfileSetupLock) can hand that hold in via
// WithHeldProfileLock instead of releasing it and letting Start() re-acquire
// its own. This closes the citadel-cli#927 residual window: previously that
// release-then-reacquire gap, however brief, was a real seam in which a
// second Start() against the same profile could interleave.
type MeetingBrowser struct {
	mu                 sync.Mutex
	sinkName           string
	profileDirOverride string
	debugPort          int
	profileDir         string
	display            string
	chromePath         string
	cmd                *exec.Cmd
	exited             chan struct{}
	xvfb               *exec.Cmd
	xvfbExited         chan struct{}
	// profileLockRelease releases the process-wide profile-dir guard acquired
	// BY THIS MeetingBrowser (see acquireMeetingProfileLock). nil before
	// Start() succeeds far enough to claim the resource, after it has been
	// released, OR whenever externalProfileLockHeld is true -- in that case
	// this MeetingBrowser never owns a release call at all (see
	// WithHeldProfileLock), so closeLocked's "release if non-nil" step is
	// correctly a no-op.
	profileLockRelease func()
	// externalProfileLockHeld records that the CALLER already holds the
	// process-wide profile-dir lock for this browser's profile dir (set via
	// WithHeldProfileLock before Start()). When true, Start() skips its own
	// TryLock acquisition entirely and never populates profileLockRelease, so
	// Close()/closeLocked() never attempts to release a lock this instance
	// does not own -- the caller retains sole ownership and must release it
	// itself, exactly once, whenever it is actually done with the profile
	// (which may be well after this MeetingBrowser's own Close() returns).
	//
	// Synchronization (citadel-cli#942): written under b.mu by
	// WithHeldProfileLock. Its ONE production read is inside Start(), which
	// already holds b.mu for the call's entire duration -- so that read is
	// itself inside a b.mu critical section, just not one claimProfileLock
	// opens itself. Start() reads it into a local (`held`) and passes that
	// down to claimProfileLock as a plain bool rather than letting
	// claimProfileLock re-read the field, precisely so claimProfileLock never
	// needs to (and never must not) touch b.mu itself -- see claimProfileLock's
	// doc comment for why re-locking there would deadlock.
	externalProfileLockHeld bool
}

// meetingProfileLocks guards concurrent MeetingBrowser.Start() calls against
// the SAME persistent profile directory. Keyed by the resolved, absolute
// profile directory rather than a single global lock, so two MeetingBrowsers
// deliberately configured with DIFFERENT profile dirs (only possible via
// profileDirOverride / EnvMeetingProfileDir — e.g. distinct test fixtures, or
// a deliberately multi-profile deployment) never contend with each other.
var (
	meetingProfileLocksMu sync.Mutex
	meetingProfileLocks   = map[string]*sync.Mutex{}
)

// meetingProfileLockFor returns the (lazily-created) mutex guarding dir. The
// returned *sync.Mutex is process-wide and shared by every caller that
// resolves to the same dir, mirroring the singleton pattern of
// GetCobrowseManager() but keyed rather than a single instance, since the
// meeting bot legitimately supports more than one profile directory.
func meetingProfileLockFor(dir string) *sync.Mutex {
	meetingProfileLocksMu.Lock()
	defer meetingProfileLocksMu.Unlock()
	l, ok := meetingProfileLocks[dir]
	if !ok {
		l = &sync.Mutex{}
		meetingProfileLocks[dir] = l
	}
	return l
}

// acquireMeetingProfileLock claims the process-wide guard for profileDir. It
// is a TryLock, never a blocking Lock: a second MEETING_JOIN whose browser
// wants a profile another meeting is actively using must fail FAST with a
// clear reason, not block for up to the 4h long-session deadline (a queued
// meeting would be over long before its turn came) and not silently proceed
// into Chrome's own launch-forwarding collision (see the MeetingBrowser
// doc comment).
//
// On success it returns a release func the caller must invoke exactly once
// when the profile is no longer in use. The release func is idempotent
// (sync.Once-wrapped) so a caller that both defers it on an error path AND
// hands it off to a longer-lived owner (MeetingBrowser.closeLocked) on the
// success path can never double-unlock.
//
// This is the seam TestAcquireMeetingProfileLock exercises directly, without
// starting a real browser (issue #895 review): it is the entire fast-path
// decision that would otherwise be un-unit-testable except by racing two real
// Chrome launches.
func acquireMeetingProfileLock(profileDir string) (release func(), err error) {
	lock := meetingProfileLockFor(profileDir)
	if !lock.TryLock() {
		return nil, fmt.Errorf(
			"meeting bot profile already in use — this node's bot can only be in one meeting at a time (profile: %s)",
			profileDir,
		)
	}
	var once sync.Once
	return func() { once.Do(lock.Unlock) }, nil
}

// NewMeetingBrowser creates a meeting browser whose audio will route into the
// given PulseAudio sink (from a NullSinkRecorder). The sink must be loaded before
// Start so the browser's PULSE_SINK target exists at launch.
//
// profileDirOverride pins the persistent Chrome profile directory for this
// browser, taking precedence over EnvMeetingProfileDir and the ConfigDir()
// default (see resolveMeetingProfileDir). Pass "" to use the default
// resolution — the normal case; a caller only needs this for tests or to
// point at a non-default data volume.
func NewMeetingBrowser(sinkName, profileDirOverride string) *MeetingBrowser {
	return &MeetingBrowser{sinkName: sinkName, profileDirOverride: profileDirOverride}
}

// WithHeldProfileLock tells this MeetingBrowser that the CALLER already holds
// the process-wide profile-dir lock (acquireMeetingProfileLock) for the
// profile this browser will resolve to — typically because the caller
// acquired it before Start() to guard a setup phase of its own (see
// AcquireMeetingProfileSetupLock) and wants that SAME hold to continue
// covering the browser's full launch and lifetime, rather than releasing it
// and having Start() re-acquire a fresh one (the citadel-cli#927 gap this
// closes: the brief release-then-reacquire window was a real, if narrow,
// seam for a second concurrent Start() to slip through).
//
// Must be called before Start(). When set, Start() skips its own TryLock
// acquisition entirely, and Close() never releases anything on this
// instance's behalf — the caller retains sole ownership of the lock and is
// responsible for releasing it itself, exactly once, whenever it is actually
// done with the profile (which may be after this MeetingBrowser's own
// Close() returns, since the caller may still be tearing down other
// profile-scoped state, e.g. the audio sink).
//
// Returns b for chaining, e.g.
// platform.NewMeetingBrowser(sink, dir).WithHeldProfileLock().
func (b *MeetingBrowser) WithHeldProfileLock() *MeetingBrowser {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.externalProfileLockHeld = true
	return b
}

// claimProfileLock decides how Start() obtains the process-wide profile-dir
// lock: acquire a fresh one (the normal case), or — when the caller has
// already claimed it via WithHeldProfileLock — skip acquisition entirely and
// return a nil release, since ownership (and therefore the single release
// call) belongs to the caller, not this MeetingBrowser. Extracted from
// Start() so this decision is unit-testable without launching a real
// browser (no Chrome/Xvfb needed either way).
//
// externalHeld is passed in rather than read from b.externalProfileLockHeld
// here (citadel-cli#942): Start() is this function's only production caller,
// and Start() already holds b.mu for the entire call (Lock at the top,
// deferred Unlock) — b.mu is NOT reentrant, so a b.mu.Lock() inside this
// function would self-deadlock the very call that's supposed to read the
// field safely. Start() reads the field into a local while it already holds
// b.mu (see its call site) and passes the value down instead, so the field's
// one real read stays properly synchronized without this function ever
// touching b.mu. Test callers below call this directly (no Start(), no held
// lock) and pass the field's value explicitly for the same reason.
func (b *MeetingBrowser) claimProfileLock(profileDir string, externalHeld bool) (release func(), err error) {
	if externalHeld {
		return nil, nil
	}
	return acquireMeetingProfileLock(profileDir)
}

// DebugPort returns the CDP port the browser listens on (0 before Start).
func (b *MeetingBrowser) DebugPort() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.debugPort
}

// Display returns the X display the browser renders on ("" before Start).
func (b *MeetingBrowser) Display() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.display
}

// ProfileDir returns the resolved persistent Chrome profile directory ("" before
// Start).
func (b *MeetingBrowser) ProfileDir() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.profileDir
}

// CurrentURL returns the browser's current page URL over CDP. Used by the join
// flow's signed-out detection (see IsGoogleSignInURL): a persistent profile
// whose Google session expired gets redirected to accounts.google.com instead
// of landing on the Meet pre-join page.
func (b *MeetingBrowser) CurrentURL() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return "", fmt.Errorf("meeting browser not started")
	}
	t, err := pickTarget(b.debugPort)
	if err != nil {
		return "", err
	}
	return t.URL, nil
}

// buildMeetingChromeArgs constructs the Chromium command line for a meeting-bot
// launch: the shared co-browse launch flags PLUS the meeting-only choices —
// software rendering (managed Xvfb has no GPU) and, load-bearingly,
// --password-store=basic. The basic os_crypt backend uses Chromium's fixed,
// build-independent key instead of a keyring secret tied to a specific binary
// and desktop session, so the persistent profile's cookies stay decryptable no
// matter which Chrome build seeded it or whether a keyring/dbus is present
// (issue #5122). Without it the bot reads no auth cookies and Google redirects
// to the account chooser, which the join flow correctly reports as "signed out".
// The seed procedure MUST match this flag (see docs/meeting-bot-profile-seeding.md).
//
// Split out from Start (which needs a real browser + display) so the exact flag
// set — especially the password-store choice — is unit-testable without launching.
func buildMeetingChromeArgs(debugPort int, profileDir string) []string {
	return buildChromeArgs(cobrowseLaunchOptions{
		debugPort:             debugPort,
		profileDir:            profileDir,
		stealth:               stealthEnabled(),
		userAgent:             os.Getenv(EnvCobrowseUserAgent),
		softwareGL:            true,
		passwordStoreBasic:    true,
		autoplayNoUserGesture: true,
	})
}

// Start launches the headed Chromium on a fresh Xvfb display with a throwaway
// profile, routing its audio into the meeting's null sink. It blocks until the
// CDP endpoint is ready so the first Navigate does not race the launch.
func (b *MeetingBrowser) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cmd != nil {
		return fmt.Errorf("meeting browser already started")
	}
	if b.sinkName == "" {
		return fmt.Errorf("meeting browser has no audio sink; construct with NewMeetingBrowser(sinkName)")
	}

	// Claim the process-wide guard on this profile dir BEFORE doing ANY other
	// setup work -- including findChromium and preparePersistentProfileDir's
	// mkdir/chmod, not just Xvfb/Chrome -- so a collision fails fast without
	// touching the filesystem or spinning up a process first just to tear it
	// down (issue #895 review, hardened by #896: this ordering is what makes
	// the failure path hermetically testable without a real Chrome/Xvfb
	// install -- see TestMeetingBrowser_StartAcquiresProfileLockFirst).
	// resolveMeetingProfileDir is the same pure resolution
	// preparePersistentProfileDir uses internally (override, then
	// EnvMeetingProfileDir, then the ConfigDir() default), so both calls agree
	// on the directory the lock is keyed by. claimProfileLock (see its doc
	// comment) either acquires a fresh lock here or, when the caller already
	// holds one (WithHeldProfileLock, citadel-cli#927), returns a nil release
	// and this function never becomes an owner of anything to release. When
	// it IS a fresh acquisition, release is unlocked by the deferred cleanup
	// below on every early return; ownership transfers to b.profileLockRelease
	// (released by closeLocked) only once the browser process itself has
	// actually started.
	//
	// held is read here, under b.mu (already locked above), rather than
	// inside claimProfileLock itself (citadel-cli#942): b.mu is not
	// reentrant, and this function holds it for its entire body, so
	// claimProfileLock re-locking it would deadlock. This is the field's one
	// production read, and it is synchronized by the b.mu this function
	// already holds.
	profileDirForLock := resolveMeetingProfileDir(b.profileDirOverride)
	held := b.externalProfileLockHeld
	release, err := b.claimProfileLock(profileDirForLock, held)
	if err != nil {
		return err
	}
	releasePending := release
	defer func() {
		if releasePending != nil {
			releasePending()
		}
	}()

	// Reap a Chrome + Xvfb pair leaked by a SIGKILLed/crashed prior process
	// (issue #488), now that the profile-dir lock above is held: no OTHER
	// live Start() (in this process OR another) can be mid-launch against
	// the same profile concurrently, so a same-process second meeting can
	// never reach this reap while its own chrome/xvfb are still live (see
	// meetingOwnerAlive's doc comment for why that matters -- it does NOT
	// special-case os.Getpid()). Deliberately placed here, right after the
	// lock and before findChromium/preparePersistentProfileDir (citadel#924
	// moved the lock to be the very first thing Start() does; the reap
	// follows it for the same "before touching anything else" reason). A
	// stale --user-data-dir lock or dangling Xvfb display from a genuinely
	// DEAD prior owner is cleared here, before any filesystem/process setup
	// below. profileDirForLock and preparePersistentProfileDir's profileDir
	// resolve to the same directory (see the lock-acquisition comment
	// above), so using it here is correct even though preparePersistentProfileDir
	// has not run yet.
	reapMeetingProcessOrphans(profileDirForLock)

	chrome, err := findChromium()
	if err != nil {
		return err
	}

	// Persistent, signed-in profile (issue #5122): resolve the same directory
	// every run — override, then EnvMeetingProfileDir, then the ConfigDir()
	// default — so a human's one-time manual Google sign-in (see
	// docs/meeting-bot-profile-seeding.md) survives across meetings instead of
	// being thrown away with the old MkdirTemp profile.
	profileDir, err := preparePersistentProfileDir(b.profileDirOverride)
	if err != nil {
		return err
	}

	debugPort, err := findFreeDebugPort()
	if err != nil {
		return err
	}

	// Meeting nodes are typically headless, so always run on a dedicated Xvfb
	// virtual display (no shared-desktop mode here, unlike co-browse). A managed
	// Xvfb has no GPU, so force software rendering.
	xvfb, display, err := startManagedXvfb(xvfbResolution())
	if err != nil {
		return err
	}

	args := buildMeetingChromeArgs(debugPort, profileDir)

	cmd := exec.Command(chrome, args...)
	// Compose BOTH env transforms: DISPLAY pins the virtual display, PULSE_SINK
	// routes this browser's (and only this browser's) audio into the meeting sink
	// so the recorder captures exactly the call.
	cmd.Env = withPulseSink(withDisplay(os.Environ(), display), b.sinkName)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		if xvfb.Process != nil {
			_ = xvfb.Process.Kill()
		}
		// profileDir is intentionally left in place: it is the persistent,
		// signed-in bot profile, not a throwaway launch artifact.
		return fmt.Errorf("launch meeting chromium: %w", err)
	}

	b.cmd = cmd
	b.chromePath = chrome
	b.debugPort = debugPort
	b.profileDir = profileDir
	b.display = display
	b.xvfb = xvfb

	// Transfer ownership of the profile-dir guard to b: from here on
	// closeLocked() releases it (on Close(), or on the waitForCDPReady failure
	// path below), not this function's own deferred cleanup. When
	// externalProfileLockHeld is true, release is nil here (claimProfileLock
	// never acquired one), so this assigns nil and closeLocked's own
	// "release if non-nil" check correctly stays a no-op — ownership never
	// left the caller.
	b.profileLockRelease = release
	releasePending = nil

	// Record the owner + child PIDs (issue #488) right after a successful
	// spawn, mirroring cobrowse's writeSessionPidfile placement: a future
	// process's reapMeetingProcessOrphans can then reclaim this browser +
	// Xvfb if THIS process is SIGKILLed before graceful teardown, even if
	// that happens before CDP ever comes up. Best-effort (logged, not fatal).
	writeMeetingPidfile(profileDir, os.Getpid(), pidOf(cmd), pidOf(xvfb))

	// Reap each child so a crash is observable and no zombie is left. Stop/Close
	// signal the kill and wait on these channels; they never call Wait directly.
	exited := make(chan struct{})
	b.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	xvfbExited := make(chan struct{})
	b.xvfbExited = xvfbExited
	go func() {
		_ = xvfb.Wait()
		close(xvfbExited)
	}()

	if err := waitForCDPReady(debugPort, 20*time.Second); err != nil {
		// Best-effort teardown so a browser that launched but never exposed CDP
		// does not leak a process, display, or profile dir.
		b.closeLocked()
		return fmt.Errorf("meeting browser launched but CDP not ready: %w", err)
	}
	return nil
}

// Navigate drives the browser to a URL over CDP.
func (b *MeetingBrowser) Navigate(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return fmt.Errorf("meeting browser not started")
	}
	_, err := cdpCommand(b.debugPort, "Page.navigate", map[string]any{"url": url})
	return err
}

// Click clicks the first element matching selector, erroring if none matches.
func (b *MeetingBrowser) Click(selector string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return fmt.Errorf("meeting browser not started")
	}
	_, err := cdpEvaluate(b.debugPort, clickJS(selector))
	return err
}

// Type sets the value of the first element matching selector, erroring if none
// matches.
func (b *MeetingBrowser) Type(selector, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return fmt.Errorf("meeting browser not started")
	}
	_, err := cdpEvaluate(b.debugPort, typeJS(selector, text))
	return err
}

// Evaluate runs a JS expression and returns its by-value result. A JS throw is
// returned as a Go error (see cdpEvaluate).
func (b *MeetingBrowser) Evaluate(expression string) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return nil, fmt.Errorf("meeting browser not started")
	}
	return cdpEvaluate(b.debugPort, expression)
}

// Close tears down the browser, its Xvfb, and its throwaway profile. Safe to call
// once; safe when never fully started.
func (b *MeetingBrowser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeLocked()
}

// closeLocked performs teardown; caller holds b.mu. Kills the browser first, then
// the Xvfb (so the browser is never left without a display mid-shutdown), then
// removes the profile dir. Bounded waits avoid a hung child wedging shutdown.
func (b *MeetingBrowser) closeLocked() error {
	var firstErr error
	if b.cmd != nil && b.cmd.Process != nil {
		if err := b.cmd.Process.Kill(); err != nil && !isProcessGoneErr(err) {
			firstErr = fmt.Errorf("kill meeting browser: %w", err)
		}
		if b.exited != nil {
			select {
			case <-b.exited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	b.cmd = nil
	b.exited = nil

	if b.xvfb != nil && b.xvfb.Process != nil {
		if err := b.xvfb.Process.Kill(); err != nil && !isProcessGoneErr(err) && firstErr == nil {
			firstErr = fmt.Errorf("kill meeting Xvfb: %w", err)
		}
		if b.xvfbExited != nil {
			select {
			case <-b.xvfbExited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	b.xvfb = nil
	b.xvfbExited = nil
	b.display = ""

	// Delete ONLY the pidfile (issue #488), never the profile directory
	// itself (see below): a graceful teardown must not look like an orphan to
	// the NEXT Start()'s reapMeetingProcessOrphans, which would otherwise log
	// spurious "reap: pid N ... skipping kill" noise (or, worse, chase a
	// recycled PID) for a browser that already exited cleanly right here.
	removeMeetingPidfile(b.profileDir)

	// Deliberately NOT removed (issue #5122): b.profileDir is the persistent,
	// signed-in bot profile, not a throwaway per-run artifact. Deleting it here
	// would silently wipe the human's one-time manual Google sign-in on every
	// meeting teardown, forcing a re-seed before the bot could ever join again.
	// Only clear the in-memory field; the directory on disk stays.
	b.profileDir = ""

	// Release the process-wide profile guard (issue #895 review) LAST, only
	// once the browser process is confirmed gone (or the bounded wait above
	// gave up) -- so another MeetingBrowser's TryLock never succeeds while
	// this one's Chrome could still be holding the --user-data-dir lock.
	// Idempotent: closeLocked can run more than once (Close() is documented
	// safe to call repeatedly, and the waitForCDPReady failure path in Start()
	// also routes here), and profileLockRelease itself is sync.Once-wrapped.
	// profileLockRelease is nil (this is correctly a no-op) both before
	// Start() has claimed anything AND whenever WithHeldProfileLock was used
	// (citadel-cli#927) — the caller owns that release call, not us.
	if b.profileLockRelease != nil {
		b.profileLockRelease()
		b.profileLockRelease = nil
	}
	return firstErr
}
