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
// IMPORTANT (unverified): the Google Meet join flow and end-detection below are
// BEST-GUESS DOM interactions. They are NOT verified end-to-end — a human must
// run this against a live meet.google.com call and confirm/swap the selectors and
// heuristics in the LIVE-TUNING block before this can be trusted. Everything is
// isolated so tuning is a single-file edit.
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// meetingPollInterval is how often we re-check admission / meeting-end state.
	meetingPollInterval = 5 * time.Second
	// defaultMeetingMaxDuration is the absolute safety cap when a job omits
	// max_duration_seconds, so a bot can never sit in a call forever.
	defaultMeetingMaxDuration = 4 * time.Hour
)

// ---------------------------------------------------------------------------
// LIVE-TUNING REQUIRED
//
// Everything in this block is a best-guess against Google Meet's DOM and has NOT
// been verified against a live call. A human must confirm/replace each selector,
// button label, and heuristic during the live-Meet session. Kept together so
// that tuning is a one-place edit.
//
//	verified against real Google Meet on: <NOT YET VERIFIED>
//
// ---------------------------------------------------------------------------
const (
	// meetNameInputSelector: the pre-join "Your name" text field shown to
	// not-signed-in participants. Best-guess aria-label match.
	meetNameInputSelector = `input[type="text"][aria-label*="name" i]`
	// meetIsAdmittedJS returns true once the in-call toolbar is present and the
	// pre-join / lobby UI is gone. Best-guess: presence of the leave-call button.
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
)

// errMeetingBotSignedOut is a sentinel wrapped into the runJoinFlow error when
// the persistent bot profile's Google session has expired (issue #5122). A
// distinct sentinel (rather than a bare fmt.Errorf) lets a caller
// errors.Is-detect "needs re-seed" specifically, e.g. to raise a
// higher-urgency alert than a generic join failure (stale selector, host never
// admitted the bot, etc.).
var errMeetingBotSignedOut = fmt.Errorf("meeting bot Chrome profile is signed out of its Google account")

// meetJoinButtonLabels are the visible button texts Meet uses for the join
// action, in priority order. "Ask to join" appears when the bot needs host
// admission; "Join now" appears when it can enter directly. LIVE-TUNING: confirm
// exact casing/locale against a real call.
var meetJoinButtonLabels = []string{"Ask to join", "Join now", "Join"}

// meetDismissButtonLabels are best-guess labels for the permission / "continue
// without microphone|camera" prompts Meet shows before the join button. Clicking
// them is best-effort (non-fatal). LIVE-TUNING: confirm against a real call.
var meetDismissButtonLabels = []string{
	"Continue without microphone",
	"Continue without camera",
	"Continue without microphone and camera",
	"Got it",
	"Dismiss",
}

// clickButtonByTextJS builds a JS expression that clicks the FIRST visible
// button/[role=button] whose trimmed text matches (case-insensitively) any of the
// given labels, returning the matched label or throwing when none is found. The
// throw lets the caller distinguish "clicked" from "no such button" (cdpEvaluate
// maps a JS throw to a Go error). labels are json.Marshal-escaped.
func clickButtonByTextJS(labels []string) string {
	arr, _ := json.Marshal(labels)
	return `(function(){var labels=` + string(arr) + `.map(function(s){return s.toLowerCase();});` +
		`var btns=Array.prototype.slice.call(document.querySelectorAll('button,[role="button"]'));` +
		`for(var i=0;i<btns.length;i++){var b=btns[i];` +
		`var txt=(b.innerText||b.textContent||"").trim().toLowerCase();` +
		`if(!txt)continue;` +
		`for(var j=0;j<labels.length;j++){if(txt===labels[j]||txt.indexOf(labels[j])!==-1){b.click();return labels[j];}}}` +
		`throw new Error("no button matched labels");})()`
}

// clickButtonByTextOptionalJS is like clickButtonByTextJS but returns "" instead
// of throwing when nothing matches — for best-effort dismissals where a missing
// prompt is normal.
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

	// Create the per-meeting null sink FIRST so the browser's PULSE_SINK target
	// exists at launch. rec.Stop unloads the sink even if we never Start it, so
	// deferring it here covers a browser-launch failure after LoadSink.
	rec := platform.NewNullSinkRecorder(p.MeetingID)
	if err := rec.LoadSink(); err != nil {
		return nil, fmt.Errorf("load meeting audio sink: %w", err)
	}
	defer func() { _, _ = rec.Stop() }()

	// Launch the sibling meeting browser routed into the sink, reusing the
	// persistent, signed-in bot Chrome profile (issue #5122) rather than a
	// throwaway one.
	br := platform.NewMeetingBrowser(rec.SinkName(), h.ProfileDir)
	if err := br.Start(); err != nil {
		return nil, fmt.Errorf("start meeting browser: %w", err)
	}
	defer func() { _ = br.Close() }()

	// Run the (unverified) Meet join flow.
	if err := h.runJoinFlow(ctx, br, p); err != nil {
		return nil, fmt.Errorf("meeting join flow: %w", err)
	}

	// Admitted: begin recording. Ensure the meetings/ dir exists first — ffmpeg's
	// -y does NOT create parent directories.
	wavPath := meetingWavPath(h.WorkspaceDir, p.MeetingID)
	if err := os.MkdirAll(filepath.Dir(wavPath), 0o700); err != nil {
		return nil, fmt.Errorf("create meetings dir: %w", err)
	}
	if err := rec.Start(wavPath); err != nil {
		return nil, fmt.Errorf("start recording: %w", err)
	}
	ctx.Log("info", "     - [Job %s] recording meeting to %s", job.ID, wavPath)

	// Stay in the call until it ends or the hard cap trips.
	endStatus := h.waitForMeetingEnd(ctx, br, p)

	// Finalize the recording (also unloads the sink; the deferred Stop is then a
	// harmless no-op). Take the path from Stop so we transcribe exactly what was
	// written.
	recordedPath, stopErr := rec.Stop()
	if stopErr != nil {
		ctx.Log("warn", "     - [Job %s] recorder stop reported: %v", job.ID, stopErr)
	}
	if recordedPath == "" {
		recordedPath = wavPath
	}

	// Transcribe node-locally by reusing the transcribe handler in-process.
	transcript, tErr := h.transcribe(ctx, job, recordedPath)
	if tErr != nil {
		// Return a structured partial result rather than failing outright: the
		// recording succeeded and is on disk; transcription can be retried.
		out, _ := json.Marshal(map[string]any{
			"status":         "recorded_transcription_failed",
			"meeting_id":     p.MeetingID,
			"audio_path":     recordedPath,
			"end_reason":     endStatus,
			"transcript":     nil,
			"transcript_err": tErr.Error(),
		})
		return out, nil
	}

	out, _ := json.Marshal(map[string]any{
		"status":     "completed",
		"meeting_id": p.MeetingID,
		"audio_path": recordedPath,
		"end_reason": endStatus,
		"transcript": json.RawMessage(transcript),
	})
	return out, nil
}

// runJoinFlow drives the best-guess Google Meet pre-join sequence. UNVERIFIED —
// see the LIVE-TUNING block. Non-fatal steps (permission dismissals, name entry
// when signed in) log and continue; the join click and admission are fatal.
func (h *MeetingJoinHandler) runJoinFlow(ctx JobContext, br *platform.MeetingBrowser, p meetingJoinParams) error {
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
		return fmt.Errorf("%w: redirected to %s — re-seed docs/meeting-bot-profile-seeding.md", errMeetingBotSignedOut, curURL)
	}
	// Secondary, best-effort corroborating signal (see meetAccountChipPresentJS
	// doc comment); logged only, not fatal.
	if v, err := br.Evaluate(meetAccountChipPresentJS); err == nil {
		if present, ok := v.(bool); ok && !present {
			ctx.Log("warn", "     - no signed-in account chip detected on pre-join page (non-fatal secondary signal; profile may need re-seeding)")
		}
	}

	// Best-effort: dismiss any camera/mic permission or "continue without …"
	// prompts. A missing prompt is normal, so this never fails the flow.
	if _, err := br.Evaluate(clickButtonByTextOptionalJS(meetDismissButtonLabels)); err != nil {
		ctx.Log("warn", "     - permission-prompt dismissal errored (non-fatal): %v", err)
	}

	// Best-effort: type the bot's display name into the pre-join name field. A
	// signed-in session has no name field, so a miss here is non-fatal.
	if err := br.Type(meetNameInputSelector, p.BotDisplayName); err != nil {
		ctx.Log("warn", "     - could not set bot name (non-fatal, may be signed in): %v", err)
	}

	// Fatal: click the join/ask-to-join button.
	if _, err := br.Evaluate(clickButtonByTextJS(meetJoinButtonLabels)); err != nil {
		return fmt.Errorf("click join button: %w", err)
	}

	// Fatal: wait until admitted (in-call toolbar appears) or timeout.
	return h.waitUntilAdmitted(ctx, br, p)
}

// waitUntilAdmitted polls the admission heuristic until the bot is in-call or the
// lobby timeout elapses.
func (h *MeetingJoinHandler) waitUntilAdmitted(ctx JobContext, br *platform.MeetingBrowser, p meetingJoinParams) error {
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
func (h *MeetingJoinHandler) waitForMeetingEnd(ctx JobContext, br *platform.MeetingBrowser, p meetingJoinParams) string {
	deadline := time.Now().Add(p.maxDuration())
	for time.Now().Before(deadline) {
		if v, err := br.Evaluate(meetIsEndedJS); err == nil {
			if b, ok := v.(bool); ok && b {
				return "call_ended"
			}
		}
		// Secondary signal: everyone else left and the bot is alone.
		if v, err := br.Evaluate(meetParticipantCountJS); err == nil {
			if n, ok := toInt(v); ok && n >= 0 && n <= 1 {
				return "alone_in_call"
			}
		}
		time.Sleep(meetingPollInterval)
	}
	ctx.Log("info", "     - max meeting duration (%s) reached; leaving", p.maxDuration())
	return "max_duration_reached"
}

// transcribe reuses the TRANSCRIBE_AUDIO handler in-process by handing it a
// synthetic job whose audio_path points at the recording under the workspace.
func (h *MeetingJoinHandler) transcribe(ctx JobContext, job *nexus.Job, wavPath string) ([]byte, error) {
	synthetic := &nexus.Job{
		ID:   job.ID + "-transcribe",
		Type: JobTypeTranscribeAudioType,
		Payload: map[string]string{
			"audio_path": wavPath,
		},
	}
	return h.transcriber.Execute(ctx, synthetic)
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
