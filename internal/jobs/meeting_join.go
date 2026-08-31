// internal/jobs/meeting_join.go
//
// MEETING_JOIN handler (issue #5098, epic #5097 — the sovereign auto-join Google
// Meet notetaker). Orchestrates the deterministic scaffolding around the one
// hardware-dependent piece (audio capture, already shipped in
// platform/audio.go): create a per-meeting null sink, launch a sibling meeting
// browser routed into that sink, run the Meet join flow, record the call, detect
// the end, transcribe node-locally, and return a structured result. Every byte
// (audio + transcript) stays on the user's machine.
//
// IMPORTANT (partially verified): the Google Meet join flow and end-detection
// below are mostly BEST-GUESS DOM interactions. The mic/camera interstitial and
// its dismiss-button text were confirmed against a live signed-in session on
// 2026-07-11; the join-button labels and everything downstream are NOT verified
// end-to-end — a human must run this against a live meet.google.com call and
// confirm/swap the selectors and heuristics in the LIVE-TUNING block before this
// can be trusted. Everything is isolated so tuning is a single-file edit.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// defaultBotDisplayName is the name the bot enters in Meet's pre-join name field
// when the job does not specify one.
const defaultBotDisplayName = "AceTeam Notetaker"

// Timeouts and cadence for the join/record/end lifecycle. Generous defaults;
// max_duration_seconds (when supplied) is a HARD cap layered on top.
const (
	// meetPageSettle waits for the Meet pre-join page to render its controls
	// after navigation before we start poking at the DOM.
	meetPageSettle = 5 * time.Second
	// admitTimeout bounds how long we wait in the "asking to join" lobby for a
	// host to admit the bot before giving up.
	admitTimeout = 3 * time.Minute
	// joinButtonTimeout bounds the dismiss-interstitial → join-button poll loop.
	// Observed live (2026-07-11): the mic/camera interstitial renders ~9s after
	// navigation, and the "Join now"/"Ask to join" pre-join page only appears a
	// few seconds after the interstitial is dismissed — a single-shot click
	// races the page load, so we poll well past both renders.
	joinButtonTimeout = 45 * time.Second
	// meetingPollInterval is how often we re-check admission / meeting-end state.
	meetingPollInterval = 5 * time.Second
	// defaultMeetingMaxDuration is the absolute safety cap when a job omits
	// max_duration_seconds, so a bot can never sit in a call forever.
	defaultMeetingMaxDuration = 4 * time.Hour
)

// ---------------------------------------------------------------------------
// LIVE-TUNING REQUIRED (partially verified)
//
// Most of this block is a best-guess against Google Meet's DOM. Confirmed live
// on 2026-07-11 (signed-in session at meet.google.com/new):
//
//   - ~9s after navigation Meet shows a mic/camera interstitial ("Do you want
//     people to see and hear you in the meeting?") with exactly two visible
//     buttons: "Continue without microphone and camera" (no aria-label) and an
//     expand_more "More options" button. meetDismissButtonLabels substring-matches
//     it via "Continue without microphone".
//   - The "Join now"/"Ask to join" pre-join page renders only AFTER that
//     interstitial is dismissed (plus a few more seconds) — hence the
//     joinButtonTimeout poll loop in runJoinFlow.
//   - HOST AUTO-ADMIT: when the bot creates its OWN meeting via
//     meet.google.com/new (signed in), Google redirects to meet.google.com/<code>
//     and drops it straight into the call ~8s after navigation — the in-call
//     toolbar is present (buttons "Leave call", "Chat with everyone", "Meeting
//     details", "Host controls", participant count 1) and NO "Join now"/"Ask to
//     join"/"Join" button EVER renders. meetIsAdmittedJS's "Leave call" selector
//     is therefore CONFIRMED for the host path, and pollForJoinClick treats
//     admission as join success.
//
// Everything else (guest-join button labels, name input, end heuristics)
// remains UNVERIFIED — we've only exercised the host/auto-admit path live. A
// human must confirm/replace those during the next live-Meet guest session.
// Kept together so that tuning is a one-place edit.
//
//	verified against real Google Meet on: 2026-07-11 (interstitial + host
//	auto-admit / in-call toolbar)
//
// ---------------------------------------------------------------------------
const (
	// meetNameInputSelector: the pre-join "Your name" text field shown to
	// not-signed-in participants. Best-guess aria-label match.
	meetNameInputSelector = `input[type="text"][aria-label*="name" i]`
	// meetIsAdmittedJS returns true once the in-call toolbar is present and the
	// pre-join / lobby UI is gone. The "Leave call" aria-label selector is
	// CONFIRMED live 2026-07-11 on the host (auto-admit) path.
	meetIsAdmittedJS = `(function(){` +
		`return !!document.querySelector('button[aria-label*="Leave call" i],button[aria-label*="Leave" i][data-tooltip*="Leave" i]');` +
		`})()`
	// meetIsEndedJS returns true when the call has ended or the bot was removed:
	// Meet swaps to a "You've left the meeting" / "Return to home screen" state.
	// Best-guess text scan.
	meetIsEndedJS = `(function(){` +
		`var t=(document.body&&document.body.innerText||"");` +
		`return /you (?:left|.?ve left) the meeting|return to home screen|you.?ve been removed|call ended/i.test(t);` +
		`})()`
	// meetParticipantCountJS returns the current participant count if Meet exposes
	// it in the toolbar, else -1. Used as a secondary end signal (bot alone).
	// Best-guess: the people-count pill's numeric text.
	meetParticipantCountJS = `(function(){` +
		`var el=document.querySelector('[aria-label*="participant" i] , [data-participant-count]');` +
		`if(!el)return -1;var m=(el.getAttribute('data-participant-count')||el.innerText||"").match(/\d+/);` +
		`return m?parseInt(m[0],10):-1;})()`
	// meetAccountChipPresentJS returns true if a signed-in Google account chip
	// (the avatar/initial in the top-right corner) is present on the pre-join
	// page. Secondary, best-effort signed-out signal alongside the deterministic
	// accounts.google.com URL redirect (platform.IsGoogleSignInURL) — the URL
	// check is what actually fails the join; this is logged only, since a
	// missing chip while ON the correct meet.google.com URL could just mean the
	// selector is stale. TODO(live-tuning): confirm selector against a real,
	// signed-in Meet pre-join page and consider promoting to fatal once trusted.
	meetAccountChipPresentJS = `(function(){` +
		`return !!document.querySelector('[aria-label*="Google Account" i],a[aria-label*="account" i] img,header img[alt*="account" i]');` +
		`})()`
	// meetLeaveCallJS clicks the in-call "Leave call" button so the bot exits the
	// meeting gracefully in response to an `/ace leave` command (issue #5435). It
	// reuses the SAME "Leave call" aria-label selector as meetIsAdmittedJS, which
	// is CONFIRMED live 2026-07-11 (host auto-admit path) — so this is the LEAST
	// uncertain interactive selector. Returns true if a button was clicked, false
	// if none matched (best-effort: the browser Close() teardown is the backstop,
	// so a missed click only means a slightly less graceful exit, never a stuck
	// bot). Intentionally does NOT throw on no-match, unlike platform.clickJS.
	meetLeaveCallJS = `(function(){` +
		`var b=document.querySelector('button[aria-label*="Leave call" i],button[aria-label*="Leave" i][data-tooltip*="Leave" i]');` +
		`if(!b)return false;b.click();return true;})()`
)

// ErrMeetingBotSignedOut is a sentinel wrapped into the runJoinFlow error when
// the persistent bot profile's Google session has expired (issue #5122). A
// distinct sentinel (rather than a bare fmt.Errorf) lets a caller
// errors.Is-detect "needs re-seed" specifically, e.g. to raise a
// higher-urgency alert than a generic join failure (stale selector, host never
// admitted the bot, etc.).
var ErrMeetingBotSignedOut = fmt.Errorf("meeting bot Chrome profile is signed out of its Google account")

// meetJoinButtonLabels are the visible button texts Meet uses for the join
// action, in priority order. "Ask to join" appears when the bot needs host
// admission; "Join now" appears when it can enter directly. LIVE-TUNING: confirm
// exact casing/locale against a real call.
var meetJoinButtonLabels = []string{"Ask to join", "Join now", "Join"}

// meetDismissButtonLabels are labels for the permission / "continue without
// microphone|camera" prompts Meet shows before the join button. Clicking them is
// best-effort (non-fatal). CONFIRMED live 2026-07-11: the interstitial's button
// text is "Continue without microphone and camera", which "Continue without
// microphone" substring-matches (the matcher uses indexOf).
var meetDismissButtonLabels = []string{
	"Continue without microphone",
	"Continue without camera",
	"Continue without microphone and camera",
	"Got it",
	"Dismiss",
}

// clickButtonByTextOptionalJS builds a JS expression that clicks the FIRST
// visible button/[role=button] whose trimmed text matches (case-insensitively,
// substring via indexOf) any of the given labels, returning the matched label or
// "" when nothing matches. The empty-string miss (instead of a throw) lets the
// poll loop in runJoinFlow retry without treating "not rendered yet" as an
// error. labels are json.Marshal-escaped.
func clickButtonByTextOptionalJS(labels []string) string {
	arr, _ := json.Marshal(labels)
	return `(function(){var labels=` + string(arr) + `.map(function(s){return s.toLowerCase();});` +
		`var btns=Array.prototype.slice.call(document.querySelectorAll('button,[role="button"]'));` +
		`for(var i=0;i<btns.length;i++){var b=btns[i];` +
		`var txt=(b.innerText||b.textContent||"").trim().toLowerCase();` +
		`if(!txt)continue;` +
		`for(var j=0;j<labels.length;j++){if(txt===labels[j]||txt.indexOf(labels[j])!==-1){b.click();return labels[j];}}}` +
		`return "";})()`
}

// meetingPlatform is the conferencing platform a meeting_url targets. It selects
// which pre-join flow runJoinFlow drives (Meet vs Teams). Kept as a small string
// enum so it can ride the result/logs legibly and so the aceteam backend's
// meeting_platform column (issue #6997 owns the enum + gates) maps 1:1.
type meetingPlatform string

const (
	// platformMeet is a meet.google.com meeting (the original, shipped flow).
	platformMeet meetingPlatform = "meet"
	// platformTeams is a Microsoft Teams web meeting (teams.microsoft.com /
	// teams.live.com), both the /meet/<id>?p=<passcode> and /l/meetup-join/…
	// link shapes. Handled by the Teams flow in meeting_join_teams.go.
	platformTeams meetingPlatform = "teams"
	// platformUnknown is any URL we do not recognize; parseMeetingJoinParams
	// rejects it so an unsupported link fails fast with a clear error rather
	// than launching a browser at a flow that cannot possibly work.
	platformUnknown meetingPlatform = "unknown"
)

// parsedMeetingURL is the pure, statically-verifiable result of inspecting a
// meeting_url: which platform it targets and (Teams /meet/<id>?p= links only)
// the pre-join passcode. This is the main unit-tested deliverable of the Teams
// scaffold — no browser, no network, just URL shape.
type parsedMeetingURL struct {
	Platform meetingPlatform
	// Passcode is the Teams pre-join passcode extracted from the `p` query
	// parameter of a teams.microsoft.com/meet/<id>?p=<passcode> link. Empty for
	// Meet, for /l/meetup-join/… links (which embed auth in the path/query and
	// need no separate passcode field), and when no `p` param is present.
	Passcode string
}

// detectMeetingPlatform maps a meeting_url's host to a meetingPlatform. Pure and
// host-based (not substring-based) so a lookalike path segment cannot spoof it:
// it parses the URL and matches the hostname exactly or as a subdomain suffix.
// A malformed URL or an unrecognized host returns platformUnknown. Case- and
// scheme-insensitive; a bare host with no scheme is tolerated by prepending
// https:// so operator-pasted "teams.microsoft.com/meet/…" still classifies.
func detectMeetingPlatform(rawURL string) meetingPlatform {
	host := meetingURLHost(rawURL)
	if host == "" {
		return platformUnknown
	}
	switch {
	case hostMatches(host, "meet.google.com"):
		return platformMeet
	case hostMatches(host, "teams.microsoft.com"),
		hostMatches(host, "teams.live.com"),
		hostMatches(host, "teams.microsoft.us"): // GCC High / DoD cloud
		return platformTeams
	default:
		return platformUnknown
	}
}

// meetingURLHost extracts the lowercased hostname from rawURL, tolerating a
// scheme-less host (prepends https://) so "teams.microsoft.com/meet/x" parses.
// Returns "" for input that has no usable host.
func meetingURLHost(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// hostMatches reports whether host equals base or is a subdomain of base
// (e.g. "gov.teams.microsoft.com" matches base "teams.microsoft.com"). Suffix
// matching is anchored on a dot so "evilteams.microsoft.com" does NOT match
// "teams.microsoft.com".
func hostMatches(host, base string) bool {
	return host == base || strings.HasSuffix(host, "."+base)
}

// parseTeamsPasscode returns the pre-join passcode from a Teams
// /meet/<id>?p=<passcode> link (the `p` query parameter), or "" when the URL is
// malformed or carries no `p` param. Pure and unit-tested.
func parseTeamsPasscode(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("p"))
}

// parseMeetingURL is the pure classifier used by parseMeetingJoinParams: it
// detects the platform and (Teams only) extracts the pre-join passcode. Split
// out so the whole URL-shape contract is unit-testable without a job payload.
func parseMeetingURL(rawURL string) parsedMeetingURL {
	plat := detectMeetingPlatform(rawURL)
	out := parsedMeetingURL{Platform: plat}
	if plat == platformTeams {
		out.Passcode = parseTeamsPasscode(rawURL)
	}
	return out
}

// meetingJoinParams is the typed, validated job payload.
type meetingJoinParams struct {
	MeetingURL         string
	MeetingID          string
	BotDisplayName     string
	MaxDurationSeconds int // 0 means "unset"; the handler applies defaultMeetingMaxDuration
	// Platform is the conferencing platform detected from MeetingURL. Selects
	// the pre-join flow in runJoinFlow.
	Platform meetingPlatform
	// Passcode is the Teams pre-join passcode (from a /meet/<id>?p=<passcode>
	// link); empty for Meet and for passcode-less Teams links.
	Passcode string
}

// parseMeetingJoinParams validates and normalizes the raw string payload. Payload
// values arrive as strings (the worker adapter coerces JSON numbers via
// fmt.Sprint), so numeric fields are parsed tolerantly.
func parseMeetingJoinParams(payload map[string]string) (meetingJoinParams, error) {
	p := meetingJoinParams{
		MeetingURL:     strings.TrimSpace(payload["meeting_url"]),
		MeetingID:      strings.TrimSpace(payload["meeting_id"]),
		BotDisplayName: strings.TrimSpace(payload["bot_display_name"]),
	}
	if p.MeetingURL == "" {
		return meetingJoinParams{}, fmt.Errorf("job payload missing required 'meeting_url' field")
	}
	if p.MeetingID == "" {
		return meetingJoinParams{}, fmt.Errorf("job payload missing required 'meeting_id' field")
	}
	// Detect the platform from the URL shape and reject anything we cannot
	// drive, so an unsupported link fails fast here rather than at a stale
	// selector after the browser is up.
	parsed := parseMeetingURL(p.MeetingURL)
	if parsed.Platform == platformUnknown {
		return meetingJoinParams{}, fmt.Errorf("unsupported meeting_url %q: only Google Meet (meet.google.com) and Microsoft Teams (teams.microsoft.com) links are supported", p.MeetingURL)
	}
	p.Platform = parsed.Platform
	p.Passcode = parsed.Passcode
	if p.BotDisplayName == "" {
		p.BotDisplayName = defaultBotDisplayName
	}
	if raw := strings.TrimSpace(payload["max_duration_seconds"]); raw != "" {
		secs, err := parsePositiveSeconds(raw)
		if err != nil {
			return meetingJoinParams{}, fmt.Errorf("invalid 'max_duration_seconds': %w", err)
		}
		p.MaxDurationSeconds = secs
	}
	return p, nil
}

// parsePositiveSeconds parses a duration-in-seconds field that may arrive as an
// int string ("300") or a float string ("300.0", from a JSON number coerced by
// fmt.Sprint). Rejects non-positive values.
func parsePositiveSeconds(raw string) (int, error) {
	if n, err := strconv.Atoi(raw); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("must be positive, got %d", n)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", raw)
	}
	if f <= 0 {
		return 0, fmt.Errorf("must be positive, got %v", f)
	}
	return int(f), nil
}

// maxDuration resolves the effective hard cap for a run.
func (p meetingJoinParams) maxDuration() time.Duration {
	if p.MaxDurationSeconds > 0 {
		return time.Duration(p.MaxDurationSeconds) * time.Second
	}
	return defaultMeetingMaxDuration
}

// sanitizeMeetingFilename keeps only filename-safe characters so a meeting id
// cannot traverse out of the meetings dir or produce an odd path. Mirrors the
// spirit of platform.sanitizeSinkSuffix but for a filesystem name.
func sanitizeMeetingFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "meeting"
	}
	return b.String()
}

// meetingWavPath builds the absolute path the recording is written to under the
// workspace, namespaced in a meetings/ subdir keyed by the sanitized meeting id.
func meetingWavPath(workspaceDir, meetingID string) string {
	return filepath.Join(workspaceDir, "meetings", sanitizeMeetingFilename(meetingID)+".wav")
}

// meetingWavRelPath is the workspace-RELATIVE form of meetingWavPath: the path
// the container's meetingd writes under its /workspace mount, which is bound to
// ${CITADEL_WORKSPACE} == the handler's WorkspaceDir (the SAME mount the
// whisper/transcribe sidecar reads). filepath.Join(WorkspaceDir, this) equals
// meetingWavPath, so the container's WAV lands exactly where the transcriber
// looks — no new hand-off plumbing. Kept adjacent to meetingWavPath so the two
// can never drift (guarded by a test).
func meetingWavRelPath(meetingID string) string {
	return "meetings/" + sanitizeMeetingFilename(meetingID) + ".wav"
}

// selectMedia returns the media backend for a run, honoring an injected override
// (tests) and otherwise delegating to defaultSelectMedia.
func (h *MeetingJoinHandler) selectMedia(p meetingJoinParams) MeetingMedia {
	if h.newMedia != nil {
		return h.newMedia(p)
	}
	return h.defaultSelectMedia(p)
}

// defaultSelectMedia picks the containerized meeting module when it is healthy on
// this node, else the in-process host stack (backwards compat: pre-provisioned
// host-stack nodes keep working). The container is preferred because its /health
// is a stronger signal (it proves non-silent audio capture via the canary tone)
// and it needs no host chrome/pulse/Xvfb/ffmpeg.
func (h *MeetingJoinHandler) defaultSelectMedia(p meetingJoinParams) MeetingMedia {
	if h.containerMediaHealthy() {
		return newContainerMedia(
			p.MeetingID,
			meetingWavRelPath(p.MeetingID),
			meetingWavPath(h.WorkspaceDir, p.MeetingID),
			p.maxDuration(),
		)
	}
	return newHostMedia(p.MeetingID, h.ProfileDir, meetingWavPath(h.WorkspaceDir, p.MeetingID))
}

// containerMediaHealthy reports whether the containerized meeting module is up
// and healthy on this node, honoring an injected probe (tests) and otherwise
// hitting meetingd's /health on the loopback.
func (h *MeetingJoinHandler) containerMediaHealthy() bool {
	if h.containerHealthProbe != nil {
		return h.containerHealthProbe()
	}
	return meetingdHealthy(&http.Client{Timeout: meetingContainerHealthTimeout}, meetingdBaseURL())
}

// MeetingJoinHandler handles MEETING_JOIN jobs. It reuses the transcribe handler
// (same workspace) to turn the recording into a transcript node-locally.
type MeetingJoinHandler struct {
	WorkspaceDir string
	// ProfileDir overrides the persistent, signed-in bot Chrome profile
	// directory (issue #5122). Empty means "use platform's default
	// resolution" — EnvMeetingProfileDir, then ConfigDir()/meeting-profile.
	// Exposed here (rather than only via the env var) so the worker's
	// startup config can pin it explicitly, e.g. to a dedicated data volume.
	ProfileDir  string
	transcriber *TranscribeAudioHandler

	// StreamingEnabled turns on the DURING-call interactive layer (issue #5435):
	// rolling transcription, the in-call `/ace` command monitor, chat capture,
	// and the self-announcement. Default false (zero value) so the plain
	// NewMeetingJoinHandler constructor and the existing batch pipeline are
	// unchanged; the worker sets it from the persisted meeting config
	// (config.Meeting.StreamingEnabled, default-on). The whole interactive layer
	// degrades gracefully (log + continue), so a stale live selector can never
	// regress the batch record→transcribe path.
	StreamingEnabled bool
	// StreamingInterval / StreamingWindow are the rolling-transcription cadence
	// and trailing stability margin (see meeting_transcribe_rolling.go). Zero
	// values fall back to the package defaults at use.
	StreamingInterval time.Duration
	StreamingWindow   time.Duration
	// StreamingMaxWindow caps how much trailing audio each rolling pass
	// re-transcribes (see meeting_transcribe_window.go), keeping per-pass cost
	// bounded regardless of call length. Zero falls back to the package default.
	StreamingMaxWindow time.Duration

	// transcribeMu serializes whisper-sidecar access so a during-call rolling
	// pass (meeting_interactive.go) never overlaps the end-of-call batch
	// transcribe — they would otherwise hit the sidecar and read the growing WAV
	// concurrently.
	transcribeMu sync.Mutex

	// newMedia selects the media backend for a run (#514). Non-nil overrides the
	// default selector (container when the meeting module is healthy on this node,
	// else the in-process host stack); tests inject a fake here. Leaving it nil —
	// the normal case — uses defaultSelectMedia.
	newMedia func(p meetingJoinParams) MeetingMedia
	// containerHealthProbe overrides how the default selector decides the meeting
	// module is healthy (#514). Non-nil is used by tests to force a backend
	// without a live meetingd; nil probes meetingd's /health on the loopback.
	containerHealthProbe func() bool

	// AudioBackupEnabled gates the sovereign audio-backup path (aceteam#5097):
	// after the recording is finalized, transcode the WAV to Opus in the meeting
	// container and upload a default-on compressed copy to the AceTeam backend
	// (the lossless WAV stays node-local). Default-on, opt-out — the worker sets
	// it from config.Meeting.AudioBackupEnabled. The whole path is best-effort so
	// a failure never fails the meeting job (the transcript is already stored).
	AudioBackupEnabled bool
	// AudioRetentionAge is the local-recording prune window; the worker sets it
	// from config.Meeting.RetentionAge(). Zero falls back to a safe default at
	// use (see backupAndPrune).
	AudioRetentionAge time.Duration
	// backupCreds returns the backend base URL + device bearer token, read FRESH
	// at upload time so a token rotated by the worker's in-place reauth is
	// honored. nil (or an empty token) disables the upload leg.
	backupCreds func() (baseURL, token string)
	// runDockerExec / backupHTTPClient / diskPressureFn are test seams; nil uses
	// the real docker-exec, HTTP, and gopsutil implementations respectively.
	// runDockerExec receives the full `docker` argv (i.e. args[0] == "exec").
	runDockerExec    func(ctx context.Context, args ...string) ([]byte, error)
	backupHTTPClient *http.Client
	diskPressureFn   func(dir string) bool
}

// NewMeetingJoinHandler constructs a handler rooted at the node workspace.
func NewMeetingJoinHandler(workspace string) *MeetingJoinHandler {
	return &MeetingJoinHandler{
		WorkspaceDir: workspace,
		transcriber:  NewTranscribeAudioHandler(workspace),
	}
}

// SetConfigDir wires citadel#891's readiness-failure diagnosis and VRAM
// preflight (meeting_vram_diagnosis.go) to a live status.NodeStatus
// collection rooted at configDir. It forwards to the internal transcriber
// (transcriber is unexported so both the top-of-Execute meeting preflight
// and every rolling/batch transcribe pass share the identical collector),
// so both handlers this package exposes agree on one status source. A no-op
// on a handler with no transcriber (a hand-built test struct); leaving it
// unset keeps diagnosis/preflight inert, matching the pre-#891 behavior.
func (h *MeetingJoinHandler) SetConfigDir(configDir string) {
	if h.transcriber != nil {
		h.transcriber.ConfigDir = configDir
	}
}

// ConfigDir reports the value SetConfigDir last wired (empty if never
// called, or if the handler has no transcriber). Exists primarily so
// construction-site tests (internal/worker) can assert the wiring reached
// the internal transcriber without depending on the unexported field.
func (h *MeetingJoinHandler) ConfigDir() string {
	if h.transcriber == nil {
		return ""
	}
	return h.transcriber.ConfigDir
}

// Execute runs the full join → record → transcribe lifecycle and returns a JSON
// document (transcript + wav path + status). The browser and null sink are torn
// down on every exit path, including errors.
func (h *MeetingJoinHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if h.WorkspaceDir == "" {
		return nil, fmt.Errorf("MEETING_JOIN requires a configured workspace directory")
	}
	p, err := parseMeetingJoinParams(job.Payload)
	if err != nil {
		return nil, err
	}

	// citadel#891: refuse fast, BEFORE joining the call, if the payload
	// declares a VRAM budget this node's transcription stack cannot fit --
	// joining, recording, and only then discovering transcription cannot run
	// wastes the whole call. Inert today (the backend does not send
	// vram_mb/vram_gb on MEETING_JOIN yet); see meeting_vram_diagnosis.go.
	// h.transcriber is nil only for a hand-built test struct that never
	// reaches this far (every real construction goes through
	// NewMeetingJoinHandler).
	if h.transcriber != nil {
		if err := checkVRAMPreflight(ctx, job.Payload, h.transcriber.collectStatusForDiagnosis); err != nil {
			return nil, err
		}
	}

	ctx.Log("info", "     - [Job %s] MEETING_JOIN %s (id=%s, bot=%q)", job.ID, p.MeetingURL, p.MeetingID, p.BotDisplayName)

	// Pick the media backend for this run: the containerized meeting module when
	// it is installed and healthy on this node, else the in-process host stack
	// (#514). media.Start brings up the browser + audio capture and returns the
	// CDP-driven browser; on failure it cleans up what it partially started, so
	// Close is deferred only after Start succeeds.
	media := h.selectMedia(p)
	br, err := media.Start()
	if err != nil {
		return nil, err
	}
	defer func() { _ = media.Close() }()

	// Run the (unverified) Meet join flow.
	if err := h.runJoinFlow(ctx, br, p); err != nil {
		return nil, fmt.Errorf("meeting join flow: %w", err)
	}

	// Admitted: begin recording. Ensure the meetings/ dir exists first — the host
	// ffmpeg's -y does NOT create parent directories (the container's meetingd
	// makedirs its own, but the dir is on the shared workspace mount either way).
	// The container writes the WAV as the node's own UID/GID (the meeting image's
	// PUID/PGID mapping — see services/meeting-service/entrypoint.sh), so the node
	// and container share ownership and no cross-UID perms fixup is needed here.
	wavPath := meetingWavPath(h.WorkspaceDir, p.MeetingID)
	if err := os.MkdirAll(filepath.Dir(wavPath), 0o700); err != nil {
		return nil, fmt.Errorf("create meetings dir: %w", err)
	}
	// The rolling-window transcription scratch clip (meeting_transcribe_window.go)
	// is a per-pass temp; remove it on exit so it does not linger in the workspace.
	defer func() { _ = os.Remove(meetingWindowWavPath(h.WorkspaceDir, p.MeetingID)) }()
	if err := media.StartRecording(); err != nil {
		return nil, fmt.Errorf("start recording: %w", err)
	}
	ctx.Log("info", "     - [Job %s] recording meeting to %s", job.ID, wavPath)

	// Stay in the call until it ends or the hard cap trips. When the interactive
	// layer is enabled (issue #5435) this additionally announces the bot, runs
	// rolling transcription + the in-call `/ace` command monitor, and captures
	// Meet chat — all best-effort, so a stale live selector degrades to the batch
	// behavior rather than regressing the recording. Otherwise the plain
	// record-until-end loop runs exactly as the shipped batch notetaker. A
	// non-nil recorderErr means the recording process itself died mid-call
	// (citadel#490) — the WAV on disk is truncated or empty, and this is
	// reported as a job failure below rather than a silent "completed" result.
	outcome, recorderErr := h.runMeetingLoop(ctx, br, p, wavPath, media)

	// Finalize the recording (host: SIGINT ffmpeg + unload the sink; container:
	// POST /record/stop). media.Close (deferred) then tears the browser down. Take
	// the path from StopRecording so we transcribe exactly what was written. Safe
	// to call even after a recorderErr: the process has already exited, so this
	// is just cleanup (unloading the sink / finalizing session state).
	recordedPath, stopErr := media.StopRecording()
	if stopErr != nil {
		ctx.Log("warn", "     - [Job %s] recorder stop reported: %v", job.ID, stopErr)
	}
	if recordedPath == "" {
		recordedPath = wavPath
	}

	// Sovereign audio backup + retention (aceteam#5097). Runs HERE — after the
	// WAV is finalized but BEFORE transcription — so the backend still receives
	// the audio even when transcription fails (the failure path returns early
	// below, and the backup is MOST valuable exactly then). Fully best-effort:
	// it never returns an error and never touches the meeting result. Runs even
	// on a recorderErr: whatever partial audio exists is still worth backing up.
	h.backupAndPrune(ctx, job, p, recordedPath)

	if recorderErr != nil {
		// end_reason "cancelled" (citadel#488) means the job's context was
		// cancelled — a deliberate shutdown/drain, not a recorder crash. Log and
		// wrap it distinctly so an operator reading this line does not chase a
		// phantom ffmpeg/audio-sink failure; the caller still returns a non-nil
		// error either way (the run did not complete), matching how any other
		// mid-flight cancellation already surfaces through this worker.
		if outcome.endReason == "cancelled" {
			ctx.Log("info", "     - [Job %s] meeting cancelled before it ended (shutdown/drain): %v", job.ID, recorderErr)
			return nil, fmt.Errorf("meeting did not complete: %w (partial audio saved at %s)", recorderErr, recordedPath)
		}
		ctx.Log("error", "     - [Job %s] recording died before the meeting ended (end_reason=%s): %v", job.ID, outcome.endReason, recorderErr)
		return nil, fmt.Errorf("meeting recording failed: %w (truncated/empty audio saved at %s)", recorderErr, recordedPath)
	}

	// Transcribe node-locally by reusing the transcribe handler in-process. This
	// end-of-call batch pass remains the SOURCE OF TRUTH for the stored
	// transcript; rolling transcription during the call was additive.
	transcript, tErr := h.transcribe(ctx, job, recordedPath)
	if tErr != nil {
		// Return a structured partial result rather than failing outright: the
		// recording succeeded and is on disk; transcription can be retried.
		out, _ := json.Marshal(map[string]any{
			"status":              "recorded_transcription_failed",
			"meeting_id":          p.MeetingID,
			"audio_path":          recordedPath,
			"end_reason":          outcome.endReason,
			"transcript":          nil,
			"transcript_err":      tErr.Error(),
			"chat":                chatForResult(outcome.chat),
			"recognized_commands": commandsForResult(outcome.recognized),
			"streamed_segments":   outcome.streamedSegments,
			"notes":               notesForResult(outcome.notes),
		})
		return out, nil
	}

	out, _ := json.Marshal(map[string]any{
		"status":              "completed",
		"meeting_id":          p.MeetingID,
		"audio_path":          recordedPath,
		"end_reason":          outcome.endReason,
		"transcript":          json.RawMessage(transcript),
		"chat":                chatForResult(outcome.chat),
		"recognized_commands": commandsForResult(outcome.recognized),
		"streamed_segments":   outcome.streamedSegments,
		"notes":               notesForResult(outcome.notes),
	})
	return out, nil
}

// runMeetingLoop chooses the interactive during-call loop (issue #5435) when
// streaming is enabled, else the plain record-until-end loop. In both cases it
// returns an interactiveOutcome; the plain path fills only endReason so the
// result shape is uniform (chat/recognized_commands come back empty). Splitting
// here keeps Execute's happy path readable and the streaming gate in one place.
//
// media is threaded through so BOTH paths can watch the recorder's own death
// signal (citadel#490) alongside their meeting-end poll — see
// waitForMeetingEnd and waitForMeetingEndInteractive. A non-nil returned error
// means the recorder died before the meeting ended; Execute surfaces that as a
// job failure rather than a silent "completed" result over a truncated WAV.
// This matters most for the interactive path: config.Meeting.StreamingEnabled
// defaults true, so waitForMeetingEndInteractive is the loop a production Meet
// meeting actually runs.
//
// Both loops also select on ctx.Context().Done() alongside recorderDead
// (citadel#488): before this, neither loop observed cancellation at all, so a
// SIGINT/shutdown or drain (e.g. AGENT_UPDATE landing mid-meeting) could not
// interrupt an in-flight meeting short of the wall-clock duration cap (up to
// 4h) or a SIGKILL that skips every defer (leaked sink/Xvfb/Chrome/profile
// dir — the still-open orphan-reaper half of #488). A cancelled context now
// unwinds the loop within one poll tick, same as a recorder death, so the
// existing deferred teardown in Execute (media.Close, StopRecording) still
// runs.
func (h *MeetingJoinHandler) runMeetingLoop(ctx JobContext, br meetingBrowser, p meetingJoinParams, wavPath string, media MeetingMedia) (interactiveOutcome, error) {
	// The interactive during-call layer (announce / rolling `/ace` commands /
	// chat capture) is Meet-coupled (issue #5435) and explicitly OUT of the Teams
	// MVP scope (#7000: join + record + transcribe, no in-call chat). Route Teams
	// through the plain record-until-end loop, which uses the Teams-aware
	// end-detection (checkMeetingEndedFor). Wiring the interactive layer for Teams
	// is a documented follow-up once its DOM is live-tuned.
	if !h.StreamingEnabled || p.Platform == platformTeams {
		reason, err := h.waitForMeetingEnd(ctx, br, p, media)
		return interactiveOutcome{endReason: reason}, err
	}

	// botMessages tracks the normalized text of messages the bot itself posts, so
	// the poll loop never scans its own echoed chat for commands (the
	// announcement contains "/ace leave"). Seeded by announceOnAdmission.
	botMessages := make(map[string]struct{})

	// Capability 4: announce on admittance (best-effort, never fatal).
	h.announceOnAdmission(ctx, br, botMessages)

	// Build the production rolling-transcription pass over the growing wav.
	transcribe := func() ([]TranscriptSegment, error) {
		return h.transcribeSegments(ctx, p.MeetingID, wavPath)
	}
	return h.waitForMeetingEndInteractive(ctx, br, p, transcribe, meetingPollInterval, botMessages, media)
}

// runJoinFlow dispatches to the platform-specific pre-join sequence based on the
// platform detected from the meeting URL (parseMeetingJoinParams). Meet is the
// original shipped flow; Teams (issue #7000) lives in meeting_join_teams.go and
// is scaffolded/best-guess, pending live tuning. Both share the same downstream
// record → transcribe path (nothing platform-specific past admission).
func (h *MeetingJoinHandler) runJoinFlow(ctx JobContext, br meetingBrowser, p meetingJoinParams) error {
	switch p.Platform {
	case platformTeams:
		return h.runTeamsJoinFlow(ctx, br, p)
	case platformMeet:
		return h.runMeetJoinFlow(ctx, br, p)
	default:
		// parseMeetingJoinParams rejects unknown platforms, so this is a
		// defensive belt-and-braces for a caller that bypasses it.
		return fmt.Errorf("cannot join meeting: unsupported platform %q for url %s", p.Platform, p.MeetingURL)
	}
}

// runMeetJoinFlow drives the Google Meet pre-join sequence (partially verified —
// see the LIVE-TUNING block). Non-fatal steps (permission dismissals, name entry
// when signed in) log and continue; the join click and admission are fatal.
func (h *MeetingJoinHandler) runMeetJoinFlow(ctx JobContext, br meetingBrowser, p meetingJoinParams) error {
	if err := br.Navigate(p.MeetingURL); err != nil {
		return fmt.Errorf("navigate to meeting url: %w", err)
	}
	time.Sleep(meetPageSettle)

	// Fatal: the persistent bot profile's Google session may have expired
	// (cookie expiry, revoked session, forced re-auth). Rather than silently
	// falling back to an anonymous join — which many orgs policy-reject anyway,
	// the whole reason this profile exists — detect the accounts.google.com
	// sign-in redirect and fail with a clear, actionable error pointing at the
	// re-seed doc instead of limping on and failing confusingly at the join
	// button or admission step.
	if curURL, err := br.CurrentURL(); err != nil {
		ctx.Log("warn", "     - could not read current URL for signed-out check (non-fatal): %v", err)
	} else if platform.IsGoogleSignInURL(curURL) {
		return fmt.Errorf("%w: redirected to %s — re-seed docs/meeting-bot-profile-seeding.md", ErrMeetingBotSignedOut, curURL)
	}
	// Secondary, best-effort corroborating signal (see meetAccountChipPresentJS
	// doc comment); logged only, not fatal.
	if v, err := br.Evaluate(meetAccountChipPresentJS); err == nil {
		if present, ok := v.(bool); ok && !present {
			ctx.Log("warn", "     - no signed-in account chip detected on pre-join page (non-fatal secondary signal; profile may need re-seeding)")
		}
	}

	// Fatal: poll admitted-check → dismiss-interstitial → name → join until the
	// bot is in-call (host auto-admit) or the join button is clicked, or
	// joinButtonTimeout elapses. A single-shot sequence races the page load
	// (observed live 2026-07-11: the mic/camera interstitial renders ~9s after
	// navigation, and the pre-join page a few seconds after that).
	if err := pollForJoinClick(ctx, br, p.BotDisplayName, joinButtonTimeout, meetingPollInterval); err != nil {
		return err
	}

	// Fatal: wait until admitted (in-call toolbar appears) or timeout.
	return h.waitUntilAdmitted(ctx, br, p)
}

// joinPage is the slice of platform.MeetingBrowser that pollForJoinClick needs,
// so the loop is unit-testable without a real browser.
type joinPage interface {
	Evaluate(expression string) (any, error)
	Type(selector, text string) error
}

// pollForJoinClick repeatedly (1) checks whether the bot is already in-call,
// (2) best-effort dismisses the mic/camera interstitial, (3) best-effort types
// the bot display name, and (4) tries the join/ask-to-join button, until the
// bot is admitted or the join button is clicked, or timeout elapses. When the
// bot hosts its own meeting (meet.google.com/new), Google auto-admits it
// straight into the call and NO join button ever renders (confirmed live
// 2026-07-11) — the admitted check is what lets that path succeed instead of
// false-timing-out. Steps 2 and 3 are non-fatal on every pass; only reaching
// the timeout with neither admission nor a join click is fatal. Production
// callers pass joinButtonTimeout/meetingPollInterval; they are parameters so
// tests can run the loop in milliseconds.
//
// Also selects on ctx.Context().Done() (citadel#488): this loop runs BEFORE
// waitUntilAdmitted in runMeetJoinFlow, so without this a shutdown/drain
// landing during the pre-join sequence would block up to joinButtonTimeout
// before waitUntilAdmitted's own cancellation check could even be reached.
func pollForJoinClick(ctx JobContext, page joinPage, botDisplayName string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Already in the call (host auto-admit path): success, no join button
		// needed. runJoinFlow's waitUntilAdmitted re-checks this idempotently.
		if v, err := page.Evaluate(meetIsAdmittedJS); err == nil {
			if b, ok := v.(bool); ok && b {
				ctx.Log("info", "     - already in call (host auto-admit), no join button needed")
				return nil
			}
		} else {
			ctx.Log("warn", "     - already-admitted probe errored (non-fatal): %v", err)
		}

		// Best-effort: dismiss the camera/mic interstitial or any "continue
		// without …" prompt. A missing prompt is normal on any given pass.
		if _, err := page.Evaluate(clickButtonByTextOptionalJS(meetDismissButtonLabels)); err != nil {
			ctx.Log("warn", "     - permission-prompt dismissal errored (non-fatal): %v", err)
		}

		// Best-effort: type the bot's display name into the pre-join name
		// field. A signed-in session has no name field, so a miss is non-fatal.
		if err := page.Type(meetNameInputSelector, botDisplayName); err != nil {
			ctx.Log("warn", "     - could not set bot name (non-fatal, may be signed in): %v", err)
		}

		// Try the join/ask-to-join button; a non-empty return means it was
		// clicked.
		if v, err := page.Evaluate(clickButtonByTextOptionalJS(meetJoinButtonLabels)); err != nil {
			ctx.Log("warn", "     - join-button probe errored (retrying): %v", err)
		} else if label, ok := v.(string); ok && label != "" {
			ctx.Log("info", "     - clicked join button (matched label %q)", label)
			return nil
		}

		select {
		case <-ctx.Context().Done():
			return fmt.Errorf("meeting cancelled while waiting for the join button: %w", ctx.Context().Err())
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("click join button: no button matched labels %v within %s (interstitial/pre-join page may have changed — re-tune meeting_join.go labels)", meetJoinButtonLabels, timeout)
}

// waitUntilAdmitted polls the admission heuristic until the bot is in-call, the
// lobby timeout elapses, or the job context is cancelled (worker shutdown/drain,
// citadel#488). Cancellation is checked every poll tick via a select so a
// SIGINT/shutdown mid-lobby returns promptly instead of blocking up to
// admitTimeout — the caller's deferred media.Close() then tears down the
// browser/Xvfb/sink instead of leaking them to a SIGKILL.
func (h *MeetingJoinHandler) waitUntilAdmitted(ctx JobContext, br meetingBrowser, p meetingJoinParams) error {
	deadline := time.Now().Add(admitTimeout)
	for time.Now().Before(deadline) {
		if v, err := br.Evaluate(meetIsAdmittedJS); err == nil {
			if b, ok := v.(bool); ok && b {
				ctx.Log("info", "     - admitted to meeting %s", p.MeetingID)
				return nil
			}
		} else {
			ctx.Log("warn", "     - admission check errored (retrying): %v", err)
		}
		select {
		case <-ctx.Context().Done():
			err := fmt.Errorf("meeting cancelled while waiting for admission: %w", ctx.Context().Err())
			ctx.Log("info", "     - admission wait for meeting %s cancelled (shutdown/drain): %v", p.MeetingID, err)
			return err
		case <-time.After(meetingPollInterval):
		}
	}
	return fmt.Errorf("not admitted to meeting within %s (host did not let the bot in, or admission selector is stale)", admitTimeout)
}

// waitForMeetingEnd blocks until the call ends (end heuristic true, or the bot is
// left alone), or until the hard duration cap trips. Returns a short reason
// string for the result. Reaching the cap or an unreadable DOM still yields a
// valid recording to transcribe (no error there, as before) — but a non-nil
// error IS now returned if media's recorder dies mid-call (citadel#490): the
// wedge that motivated this fix was ffmpeg exiting early (pulse restart, OOM,
// sink unloaded) while this loop kept polling the browser for the full
// duration, then handed whisper a truncated/empty file with nothing to say it
// happened. media.RecordingAlive() is selected on alongside the poll cadence
// so death is caught within one poll tick rather than only at the next
// checkMeetingEndedFor call (which never fires again once the browser side is
// otherwise healthy) or the duration cap.
func (h *MeetingJoinHandler) waitForMeetingEnd(ctx JobContext, br meetingBrowser, p meetingJoinParams, media MeetingMedia) (string, error) {
	deadline := time.Now().Add(p.maxDuration())
	recorderDead := media.RecordingAlive()
	for time.Now().Before(deadline) {
		if reason, ended := checkMeetingEndedFor(p.Platform, br); ended {
			return reason, nil
		}
		select {
		case <-recorderDead:
			err := fmt.Errorf("recording process exited before the meeting ended; the saved audio is truncated or empty")
			ctx.Log("error", "     - recorder died mid-meeting: %v", err)
			return "recorder_died", err
		case <-ctx.Context().Done():
			err := fmt.Errorf("meeting cancelled: %w", ctx.Context().Err())
			ctx.Log("info", "     - meeting wait for %s cancelled (shutdown/drain): %v", p.MeetingID, err)
			return "cancelled", err
		case <-time.After(meetingPollInterval):
		}
	}
	ctx.Log("info", "     - max meeting duration (%s) reached; leaving", p.maxDuration())
	return "max_duration_reached", nil
}

// checkMeetingEndedFor dispatches the end heuristic by platform so a Teams call
// ends on its own end/removed signal (best-guess, see meeting_join_teams.go)
// rather than always running to the max_duration cap. Meet keeps the shared
// checkMeetingEnded. The interactive (streaming) loop remains Meet-coupled and
// out of Teams MVP scope — Teams runs the plain record→transcribe path.
func checkMeetingEndedFor(plat meetingPlatform, page meetPage) (string, bool) {
	if plat == platformTeams {
		return checkTeamsMeetingEnded(page)
	}
	return checkMeetingEnded(page)
}

// syntheticTranscribeJob builds the in-process TRANSCRIBE_AUDIO job used to reuse
// the transcribe handler for both the end-of-call batch pass and each rolling
// pass, keyed by a caller-supplied id so the two are distinguishable in logs.
func syntheticTranscribeJob(id, wavPath string) *nexus.Job {
	return &nexus.Job{
		ID:   id,
		Type: JobTypeTranscribeAudioType,
		Payload: map[string]string{
			"audio_path": wavPath,
		},
	}
}

// transcribe reuses the TRANSCRIBE_AUDIO handler in-process for the end-of-call
// batch pass (the stored transcript's source of truth). It serializes on
// transcribeMu with any in-flight rolling pass so the two never hit the whisper
// sidecar or read the recording concurrently.
func (h *MeetingJoinHandler) transcribe(ctx JobContext, job *nexus.Job, wavPath string) ([]byte, error) {
	h.transcribeMu.Lock()
	defer h.transcribeMu.Unlock()
	return h.transcriber.Execute(ctx, syntheticTranscribeJob(job.ID+"-transcribe", wavPath))
}

// chatForResult and commandsForResult normalize nil slices to empty arrays so
// the additive MEETING_JOIN result fields serialize as [] rather than null,
// keeping the schema stable for consumers whether or not streaming ran.
func chatForResult(msgs []MeetChatMessage) []MeetChatMessage {
	if msgs == nil {
		return []MeetChatMessage{}
	}
	return msgs
}

func commandsForResult(cmds []RecognizedCommand) []RecognizedCommand {
	if cmds == nil {
		return []RecognizedCommand{}
	}
	return cmds
}

// notesForResult normalizes the in-call NOTE/ACTION entries to an empty array so
// the additive `notes` field serializes as [] rather than null.
func notesForResult(notes []string) []string {
	if notes == nil {
		return []string{}
	}
	return notes
}

// JobTypeTranscribeAudioType is the wire type string for the transcription job,
// duplicated here as a local const because the worker package (which owns the
// canonical JobType constants) imports this package, not the reverse. Kept in
// sync with worker.JobTypeTranscribeAudio.
const JobTypeTranscribeAudioType = "TRANSCRIBE_AUDIO"

// toInt coerces a JS-by-value number (float64 over the wire) or an int to int.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// Ensure MeetingJoinHandler implements JobHandler.
var _ JobHandler = (*MeetingJoinHandler)(nil)
