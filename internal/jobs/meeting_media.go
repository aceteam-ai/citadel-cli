// internal/jobs/meeting_media.go
//
// MeetingMedia abstracts the meeting media stack (the browser the join flow
// drives + the audio recorder) so MEETING_JOIN can run against either backend
// without touching the fragile Google Meet DOM logic (aceteam-ai/citadel-cli#514):
//
//   - hostMedia (unchanged, backwards-compat house rule): the in-process host
//     stack — a PulseAudio null sink (NullSinkRecorder) plus a host Chromium on a
//     managed Xvfb (MeetingBrowser). This is what pre-provisioned meeting nodes
//     already use.
//   - containerMedia: an HTTP client for meetingd, the session supervisor inside
//     the installable meeting module (published image, PR #517). meetingd owns
//     the in-container null sink + Chromium + ffmpeg; this drives it over its
//     loopback control API and drives the browser over the published CDP port.
//
// The container path needs NO host chrome/pulse/Xvfb/ffmpeg, and the WAV lands on
// the SAME ${CITADEL_WORKSPACE} mount the whisper/transcribe sidecar reads, so
// the MEETING_JOIN -> TRANSCRIBE_AUDIO hand-off is unchanged. See
// docs/meeting-bot-profile-seeding.md and aceteam#5097.
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
)

// The four platform calls hostMedia.Start() drives for the citadel-cli#925
// fix are package vars (mirroring the injectable-seam pattern used
// throughout internal/platform, e.g. pidKiller/pactlListModulesFn) so a
// jobs-level test can verify the exact CALL ORDER hermetically, in Go, with
// no subprocess involved at all -- unlike a real `pactl` binary invoked via
// PATH, an injected fake here cannot be affected by any cross-process
// filesystem-visibility timing, so the resulting test cannot flake for
// reasons unrelated to the code under test.
var (
	acquireMeetingProfileSetupLockFn = platform.AcquireMeetingProfileSetupLock
	markMeetingProfileOwnedFn        = platform.MarkMeetingProfileOwned
	clearMeetingProfilePlaceholderFn = platform.ClearMeetingProfilePlaceholder
	reapOrphanedMeetingSinksFn       = platform.ReapOrphanedMeetingSinks
)

// meetingBrowser is the CDP surface the MEETING_JOIN join + interactive flow
// drives. Both the in-process *platform.MeetingBrowser and the container-driving
// *platform.CDPBrowser satisfy it, so the same Meet DOM logic runs against either
// backend.
type meetingBrowser interface {
	Navigate(url string) error
	CurrentURL() (string, error)
	Evaluate(expression string) (any, error)
	Type(selector, text string) error
	Close() error
}

// MeetingMedia is the media backend for one meeting run: bring up the browser +
// audio capture, record the call audio to the workspace WAV, and tear down. The
// backend is chosen per run (container when the meeting module is healthy on this
// node, else host — see MeetingJoinHandler.selectMedia).
type MeetingMedia interface {
	// Start brings up the media stack (host: load the null sink then launch
	// Chromium+Xvfb; container: POST /sessions to meetingd) and returns the
	// CDP-driven browser. On failure it cleans up anything it partially brought
	// up, so the caller only defers Close on success.
	Start() (meetingBrowser, error)
	// StartRecording begins capturing the call audio to the meeting's workspace
	// WAV (host: ffmpeg on the sink monitor; container: POST /sessions/{id}/record).
	StartRecording() error
	// StopRecording finalizes the recording (valid WAV trailer) and returns the
	// absolute host path the transcriber should read.
	StopRecording() (string, error)
	// Close tears down the browser and audio stack. Idempotent.
	Close() error
	// RecordingAlive returns a channel that is closed the moment the underlying
	// recording process exits — cleanly or otherwise — so the meeting loop can
	// detect a dead recorder WHILE the call is still in progress (citadel#490)
	// instead of only discovering it after the meeting-end poll (or the hard
	// duration cap) finally returns, by which point the WAV is truncated or
	// empty and nothing has flagged it. Callers select on this alongside their
	// existing poll ticker.
	//
	// A backend that cannot observe recorder liveness this way returns a
	// channel that never fires (nil is fine here — a nil channel blocks forever
	// in a select), degrading to the pre-#490 behavior for that backend rather
	// than risking a false "recorder died" report.
	RecordingAlive() <-chan struct{}
}

// ---------------------------------------------------------------------------
// host backend (unchanged in-process stack)
// ---------------------------------------------------------------------------

type hostMedia struct {
	meetingID  string
	profileDir string
	wavPath    string
	rec        *platform.NullSinkRecorder
	br         *platform.MeetingBrowser
	// releaseProfileLock releases the process-wide profile lock acquired at
	// the top of Start() (see acquireMeetingProfileSetupLockFn), once Start()
	// has succeeded far enough that ownership of that lock has moved from
	// Start()'s own local defer to this struct -- i.e. it now spans the
	// browser's full lifetime, handed to platform.MeetingBrowser via
	// WithHeldProfileLock (citadel-cli#927) rather than released and
	// re-acquired. nil before that point (Start()'s own defer still owns
	// releasing it) and after Close() has released it.
	releaseProfileLock func()
}

func newHostMedia(meetingID, profileDir, wavPath string) *hostMedia {
	return &hostMedia{meetingID: meetingID, profileDir: profileDir, wavPath: wavPath}
}

func (m *hostMedia) Start() (meetingBrowser, error) {
	// Claim the SAME process-wide profile lock MeetingBrowser.Start() would
	// otherwise acquire itself -- a real, goroutine-correct mutex that closes
	// the window in which two genuinely concurrent SAME-PROCESS meeting
	// attempts against this profile (MEETING_JOIN runs on its own dedicated
	// async lane -- see CLAUDE.md's "Long-session and GPU-bound jobs get a
	// dedicated always-async lane" -- so two overlapping meetings really can
	// race here) could interleave; a PID-keyed pidfile alone cannot
	// distinguish two goroutines in this SAME process (see
	// meetingSinkSweepBlocked's doc comment).
	//
	// Unlike the pre-citadel-cli#927 version of this function, this lock is
	// now held CONTINUOUSLY from here through the browser's full launch, not
	// just this function's own setup phase (mark-owned + sink-sweep +
	// LoadSink): it is handed off to platform.MeetingBrowser via
	// WithHeldProfileLock below instead of being released and having Start()
	// re-acquire its own. That removes the brief release-then-reacquire
	// window that used to exist between this function's setup and
	// MeetingBrowser.Start()'s own acquisition -- the residual gap
	// meetingSinkSweepBlocked's doc comment previously described as
	// "narrowed, not fully closed".
	//
	// lockOwnedHere tracks whether THIS function still owns the single
	// release call. It starts true and the deferred release below fires on
	// every early-return failure path (MeetingMedia.Start's documented
	// contract: the caller only defers Close() on SUCCESS, so a failure here
	// must clean up its own lock hold, not rely on Close() to do it). On
	// success, ownership moves to m.releaseProfileLock, released by Close()
	// only once the browser itself has been torn down.
	profileDir, release, err := acquireMeetingProfileSetupLockFn(m.profileDir)
	if err != nil {
		return nil, err
	}
	lockOwnedHere := true
	defer func() {
		if lockOwnedHere {
			release()
		}
	}()

	// Mark this profile OWNED by this process BEFORE the sink sweep or
	// LoadSink runs (citadel-cli#925 review). Root cause: the real pidfile
	// is only written deep inside MeetingBrowser.Start(), well after Chrome
	// actually launches -- so there is a real window where THIS meeting has
	// already called LoadSink (its sink is now live and visible to `pactl
	// list short modules`) but no pidfile exists yet. A concurrent
	// hostMedia.Start() (another goroutine in this process, or a sibling
	// process sharing this same persistent profile) that runs its own
	// ReapOrphanedMeetingSinks in that window sees "no live owner" and
	// unloads THIS meeting's sink out from under it -- the browser is left
	// with PULSE_SINK pointing at a now-gone sink, so ffmpeg silently
	// records silence. A placeholder pidfile (owner=this process, chrome=0,
	// xvfb=0) is enough to make any concurrent sweep see this profile as
	// owned, while being completely inert to reapMeetingProcessOrphans
	// (which never acts when both child PIDs are <=0). MarkMeetingProfileOwned
	// refuses to overwrite an existing live owner (protects a genuinely
	// in-flight launch's real chrome/xvfb PIDs from being clobbered), so
	// `wrote` tells us whether the placeholder here is actually ours to
	// clean up.
	_, wrote, err := markMeetingProfileOwnedFn(profileDir)
	if err != nil {
		return nil, fmt.Errorf("mark meeting profile owned: %w", err)
	}
	if wrote {
		// Clears the placeholder ONLY if it is still exactly what we wrote
		// (ClearMeetingProfilePlaceholder's own compare-before-delete): once
		// MeetingBrowser.Start() launches successfully it overwrites this
		// with the real (owner, chrome, xvfb) pidfile, at which point this
		// defer is a safe no-op -- that real pidfile's lifecycle from there
		// is closeLocked's job (deletes it on graceful/CDP-fail teardown),
		// not this one's. This defer exists only for the window BEFORE any
		// browser ever launches: rec.LoadSink() failing below, or
		// br.Start() failing before it ever writes its own real pidfile.
		defer clearMeetingProfilePlaceholderFn(profileDir)
	}

	// Reap any `citadel_meeting_*` null sink orphaned by a SIGKILLed/crashed
	// prior process (issue #488) BEFORE creating THIS meeting's own sink
	// below. Ordering is load-bearing: ReapOrphanedMeetingSinks only ever
	// unloads sinks that already exist at call time, so calling it here --
	// strictly before rec.LoadSink() creates the current sink -- guarantees
	// the sweep can never unload the sink this very Start() is about to load.
	// Reversing the order (sweeping after LoadSink) would unload the current
	// meeting's own just-created sink, since nothing yet distinguishes it
	// from a stale one. The placeholder written above additionally protects
	// against a CONCURRENT caller's sweep landing in this same window (see
	// MarkMeetingProfileOwned's doc comment) -- this call and that
	// protection are independent halves of the same fix.
	reapOrphanedMeetingSinksFn(profileDir)

	// Create the per-meeting null sink FIRST so the browser's PULSE_SINK target
	// exists at launch.
	rec := platform.NewNullSinkRecorder(m.meetingID)
	if err := rec.LoadSink(); err != nil {
		return nil, fmt.Errorf("load meeting audio sink: %w", err)
	}
	m.rec = rec

	// Launch the sibling browser routed into the sink, reusing the persistent,
	// signed-in bot Chrome profile (issue #5122). WithHeldProfileLock hands
	// the SAME lock this function has held since before the sink sweep into
	// the browser's own full-launch-lifetime hold (citadel-cli#927): Start()
	// skips its own (redundant) acquisition, and it will NOT release this
	// lock on Close() -- that release call stays ours (see m.releaseProfileLock
	// below), never transferred to the browser.
	br := platform.NewMeetingBrowser(rec.SinkName(), profileDir).WithHeldProfileLock()
	if err := br.Start(); err != nil {
		// Unload the sink we just loaded so a browser-launch failure does not leak it.
		_, _ = rec.Stop()
		m.rec = nil
		return nil, fmt.Errorf("start meeting browser: %w", err)
	}
	m.br = br

	// Success: the lock now spans the browser's full lifetime. Ownership
	// moves from this function's own defer to m -- Close() releases it,
	// AFTER m.br.Close() has torn the browser down, not this function's exit.
	m.releaseProfileLock = release
	lockOwnedHere = false
	return br, nil
}

func (m *hostMedia) StartRecording() error {
	if m.rec == nil {
		return fmt.Errorf("host media not started")
	}
	return m.rec.Start(m.wavPath)
}

func (m *hostMedia) StopRecording() (string, error) {
	if m.rec == nil {
		return m.wavPath, nil
	}
	p, err := m.rec.Stop()
	if p == "" {
		p = m.wavPath
	}
	return p, err
}

// RecordingAlive delegates to the null-sink recorder's own death signal
// (platform.NullSinkRecorder.Exited). m.rec is set by Start and cleared by
// Close, so a call outside that window (or before StartRecording has run)
// returns nil — no signal, never a false positive.
func (m *hostMedia) RecordingAlive() <-chan struct{} {
	if m.rec == nil {
		return nil
	}
	return m.rec.Exited()
}

func (m *hostMedia) Close() error {
	var firstErr error
	if m.br != nil {
		if err := m.br.Close(); err != nil {
			firstErr = err
		}
		m.br = nil
	}
	// rec.Stop is idempotent (safe after StopRecording), and unloads the sink.
	if m.rec != nil {
		if _, err := m.rec.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
		m.rec = nil
	}
	// Release the profile lock LAST, only once the browser (and therefore
	// Chrome's own --user-data-dir hold) is confirmed torn down -- mirrors
	// platform.MeetingBrowser.closeLocked's own release-last ordering.
	// Ownership of this lock lives here (not on m.br) precisely because
	// br.Start() was called with WithHeldProfileLock (citadel-cli#927):
	// m.br.Close() above will not have released anything. nil (a no-op)
	// whenever Start() never reached success, or Close() has already run.
	if m.releaseProfileLock != nil {
		m.releaseProfileLock()
		m.releaseProfileLock = nil
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// container backend (meetingd HTTP control API)
// ---------------------------------------------------------------------------

const (
	// meetingContainerHealthTimeout bounds the /health probe used to pick the
	// backend. /health runs meetingd's canary-tone capture (~1.5s record + tone
	// generation), so it is deliberately generous.
	meetingContainerHealthTimeout = 20 * time.Second
	// meetingContainerCDPTimeout bounds waiting for the freshly launched
	// in-container Chromium to expose CDP through the published port.
	meetingContainerCDPTimeout = 30 * time.Second
	// meetingContainerHTTPTimeout bounds a single meetingd control call.
	meetingContainerHTTPTimeout = 30 * time.Second
	// meetingSpeakTimeout bounds a blocking POST /mic/play. meetingd plays the clip
	// SYNCHRONOUSLY (it returns when playback finishes), and a TTS clip can run tens
	// of seconds — well past meetingContainerHTTPTimeout — so speaking uses its own
	// generous client, else a legitimately long clip surfaces as a spurious timeout
	// error mid-playback.
	meetingSpeakTimeout = 3 * time.Minute
)

// meetingdBaseURL is the loopback base URL for the meeting module's control API.
// The container publishes meetingd on services.MeetingdHostPort (8207) bound to
// 127.0.0.1 (compose), so the co-located citadel process reaches it here.
func meetingdBaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", services.MeetingdHostPort)
}

// meetingdHealthy reports whether the containerized meeting module answers a
// healthy /health on this node's loopback. A 200 is, by meetingd's design, proof
// it can actually capture non-silent audio (the canary tone probe returns 503
// otherwise), so this is a strictly stronger signal than the host-binary probes.
func meetingdHealthy(client *http.Client, base string) bool {
	resp, err := client.Get(base + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// meetingSessionResponse is meetingd's POST /sessions body.
type meetingSessionResponse struct {
	SessionID string `json:"session_id"`
	// CDPPort is the CONTAINER-internal CDP port meetingd reports (9223). The host
	// reaches CDP at the PUBLISHED port (services.MeetingCDPHostPort) instead, so
	// this field is intentionally not used to build the CDP client — see
	// containerMedia.cdpPort.
	CDPPort int    `json:"cdp_port"`
	Sink    string `json:"sink"`
}

type containerMedia struct {
	// wavRelPath is the workspace-RELATIVE output path meetingd writes under its
	// /workspace mount (== ${CITADEL_WORKSPACE} == the transcriber's workspace).
	wavRelPath string
	// wavAbsPath is the host-absolute form the transcriber reads.
	wavAbsPath  string
	maxDuration time.Duration
	base        string
	// cdpPort is the PUBLISHED host CDP port (services.MeetingCDPHostPort, 8208).
	// meetingd's POST /sessions reports the container-internal port (9223); we
	// ignore it and use the publish, because the advertised port is unreachable
	// from the host (see internal/platform CDPBrowser).
	cdpPort   int
	client    *http.Client
	sessionID string
	browser   *platform.CDPBrowser
}

func newContainerMedia(meetingID, wavRelPath, wavAbsPath string, maxDuration time.Duration) *containerMedia {
	return &containerMedia{
		wavRelPath:  wavRelPath,
		wavAbsPath:  wavAbsPath,
		maxDuration: maxDuration,
		base:        meetingdBaseURL(),
		cdpPort:     services.MeetingCDPHostPort,
		client:      &http.Client{Timeout: meetingContainerHTTPTimeout},
		// A deterministic session id (derived from the meeting id) lets a
		// same-meeting retry reclaim its own orphaned session on a 409.
		sessionID: sanitizeMeetingFilename(meetingID),
	}
}

func (m *containerMedia) Start() (meetingBrowser, error) {
	if err := m.createSession(); err != nil {
		return nil, err
	}
	br := platform.NewCDPBrowser(m.cdpPort)
	if err := br.Ready(meetingContainerCDPTimeout); err != nil {
		_ = m.deleteSession()
		return nil, err
	}
	m.browser = br
	return br, nil
}

func (m *containerMedia) createSession() error {
	body := map[string]any{
		"session_id":           m.sessionID,
		"max_duration_seconds": int(m.maxDuration.Seconds()),
	}
	respBody, status, err := m.postJSON("/sessions", body)
	if err != nil {
		return fmt.Errorf("meetingd create session: %w", err)
	}
	if status == http.StatusConflict {
		// A prior session is still active. meetingd enforces one meeting per node
		// (fixed CDP port), and its reaper only clears a session at its
		// max_duration. If the orphan is OURS (a same-meeting retry) it shares our
		// deterministic id, so clear it and retry once; a DIFFERENT meeting's
		// orphan we cannot address (meetingd has no list/clear endpoint) and it is
		// a legitimate busy state, so we surface a clear error below.
		_ = m.deleteSession()
		respBody, status, err = m.postJSON("/sessions", body)
		if err != nil {
			return fmt.Errorf("meetingd create session (after clearing stale): %w", err)
		}
	}
	switch status {
	case http.StatusCreated:
		var sr meetingSessionResponse
		if err := json.Unmarshal(respBody, &sr); err != nil {
			return fmt.Errorf("parse meetingd session response: %w", err)
		}
		if sr.SessionID != "" {
			m.sessionID = sr.SessionID
		}
		return nil
	case http.StatusConflict:
		return fmt.Errorf("meeting module is busy with another active session on this node " +
			"(one meeting per node); retry after it ends, or restart the meeting module to clear it")
	case http.StatusServiceUnavailable:
		return fmt.Errorf("meeting module not ready to start a session: %s", string(respBody))
	default:
		return fmt.Errorf("meetingd POST /sessions returned status %d: %s", status, string(respBody))
	}
}

func (m *containerMedia) StartRecording() error {
	respBody, status, err := m.postJSON(m.sessionPath("/record"), map[string]any{"out": m.wavRelPath})
	if err != nil {
		return fmt.Errorf("meetingd start recording: %w", err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("meetingd start recording returned status %d: %s", status, string(respBody))
	}
	return nil
}

func (m *containerMedia) StopRecording() (string, error) {
	respBody, status, err := m.postJSON(m.sessionPath("/record/stop"), nil)
	if err != nil {
		return m.wavAbsPath, fmt.Errorf("meetingd stop recording: %w", err)
	}
	if status != http.StatusOK {
		return m.wavAbsPath, fmt.Errorf("meetingd stop recording returned status %d: %s", status, string(respBody))
	}
	return m.wavAbsPath, nil
}

// RecordingAlive: meetingd owns and reaps its own in-container ffmpeg process
// entirely over HTTP (POST /sessions, /record, /record/stop) — there is no
// equivalent liveness channel exposed by that control API today, so container
// recording liveness isn't observable the same way host recording is. Return a
// channel that never fires; this backend degrades to the pre-citadel#490
// behavior (a dead in-container recorder is invisible until the next
// meeting-end poll or the duration cap) rather than the fix, which is
// documented here as a known gap, not silently dropped.
func (m *containerMedia) RecordingAlive() <-chan struct{} {
	return nil
}

func (m *containerMedia) Close() error {
	if m.browser != nil {
		_ = m.browser.Close()
		m.browser = nil
	}
	return m.deleteSession()
}

// SpeakFile injects a workspace-relative audio file into the container's virtual
// microphone (meetingd POST /mic/play), so the bot is HEARD in the live meeting —
// the bot->room complement of the room->bot capture path (aceteam#7079). It is
// strictly ADDITIVE: the join/record flow never calls it, so a bot that only
// listens behaves exactly as before. Wiring it to a realtime TTS/agent engine is a
// later wave; this is the tested, minimal transport.
//
// It blocks until meetingd finishes playing the clip (synchronous playback), so it
// uses a dedicated long-timeout client rather than m.client (30s). A 409 means
// another clip is already playing; a 503 means the virtual mic is not present on
// this node.
func (m *containerMedia) SpeakFile(wavRelPath string) error {
	body, err := json.Marshal(map[string]any{"path": wavRelPath})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, m.base+"/mic/play", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: meetingSpeakTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("meetingd mic play: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("meetingd mic play returned status %d: %s", resp.StatusCode, string(out))
	}
	return nil
}

func (m *containerMedia) sessionPath(suffix string) string {
	return "/sessions/" + url.PathEscape(m.sessionID) + suffix
}

func (m *containerMedia) deleteSession() error {
	req, err := http.NewRequest(http.MethodDelete, m.base+"/sessions/"+url.PathEscape(m.sessionID), nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// postJSON POSTs an optional JSON body to path and returns the response body,
// status code, and any transport error. A nil body sends an empty POST.
func (m *containerMedia) postJSON(path string, body any) ([]byte, int, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, m.base+path, buf)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

// Compile-time interface checks.
var (
	_ MeetingMedia   = (*hostMedia)(nil)
	_ MeetingMedia   = (*containerMedia)(nil)
	_ meetingBrowser = (*platform.MeetingBrowser)(nil)
	_ meetingBrowser = (*platform.CDPBrowser)(nil)
)
