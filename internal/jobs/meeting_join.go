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
	"encoding/json"
	"fmt"
	"net/http"
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

// meetingJoinParams is the typed, validated job payload.
type meetingJoinParams struct {
	MeetingURL         string
	MeetingID          string
	BotDisplayName     string
	MaxDurationSeconds int // 0 means "unset"; the handler applies defaultMeetingMaxDuration
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
}

// NewMeetingJoinHandler constructs a handler rooted at the node workspace.
func NewMeetingJoinHandler(workspace string) *MeetingJoinHandler {
	return &MeetingJoinHandler{
		WorkspaceDir: workspace,
		transcriber:  NewTranscribeAudioHandler(workspace),
	}
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
	// record-until-end loop runs exactly as the shipped batch notetaker.
	outcome := h.runMeetingLoop(ctx, br, p, wavPath)

	// Finalize the recording (host: SIGINT ffmpeg + unload the sink; container:
	// POST /record/stop). media.Close (deferred) then tears the browser down. Take
	// the path from StopRecording so we transcribe exactly what was written.
	recordedPath, stopErr := media.StopRecording()
	if stopErr != nil {
		ctx.Log("warn", "     - [Job %s] recorder stop reported: %v", job.ID, stopErr)
	}
	if recordedPath == "" {
		recordedPath = wavPath
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
func (h *MeetingJoinHandler) runMeetingLoop(ctx JobContext, br meetingBrowser, p meetingJoinParams, wavPath string) interactiveOutcome {
	if !h.StreamingEnabled {
		return interactiveOutcome{endReason: h.waitForMeetingEnd(ctx, br, p)}
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
	return h.waitForMeetingEndInteractive(ctx, br, p, transcribe, meetingPollInterval, botMessages)
}

// runJoinFlow drives the Google Meet pre-join sequence (partially verified —
// see the LIVE-TUNING block). Non-fatal steps (permission dismissals, name entry
// when signed in) log and continue; the join click and admission are fatal.
func (h *MeetingJoinHandler) runJoinFlow(ctx JobContext, br meetingBrowser, p meetingJoinParams) error {
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

		time.Sleep(interval)
	}
	return fmt.Errorf("click join button: no button matched labels %v within %s (interstitial/pre-join page may have changed — re-tune meeting_join.go labels)", meetJoinButtonLabels, timeout)
}

// waitUntilAdmitted polls the admission heuristic until the bot is in-call or the
// lobby timeout elapses.
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
		time.Sleep(meetingPollInterval)
	}
	return fmt.Errorf("not admitted to meeting within %s (host did not let the bot in, or admission selector is stale)", admitTimeout)
}

// waitForMeetingEnd blocks until the call ends (end heuristic true, or the bot is
// left alone), or until the hard duration cap trips. Returns a short reason
// string for the result. It never errors: reaching the cap or an unreadable DOM
// still yields a valid recording to transcribe.
func (h *MeetingJoinHandler) waitForMeetingEnd(ctx JobContext, br meetingBrowser, p meetingJoinParams) string {
	deadline := time.Now().Add(p.maxDuration())
	for time.Now().Before(deadline) {
		if reason, ended := checkMeetingEnded(br); ended {
			return reason
		}
		time.Sleep(meetingPollInterval)
	}
	ctx.Log("info", "     - max meeting duration (%s) reached; leaving", p.maxDuration())
	return "max_duration_reached"
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
