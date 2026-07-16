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
}

func newHostMedia(meetingID, profileDir, wavPath string) *hostMedia {
	return &hostMedia{meetingID: meetingID, profileDir: profileDir, wavPath: wavPath}
}

func (m *hostMedia) Start() (meetingBrowser, error) {
	// Create the per-meeting null sink FIRST so the browser's PULSE_SINK target
	// exists at launch.
	rec := platform.NewNullSinkRecorder(m.meetingID)
	if err := rec.LoadSink(); err != nil {
		return nil, fmt.Errorf("load meeting audio sink: %w", err)
	}
	m.rec = rec

	// Launch the sibling browser routed into the sink, reusing the persistent,
	// signed-in bot Chrome profile (issue #5122).
	br := platform.NewMeetingBrowser(rec.SinkName(), m.profileDir)
	if err := br.Start(); err != nil {
		// Unload the sink we just loaded so a browser-launch failure does not leak it.
		_, _ = rec.Stop()
		m.rec = nil
		return nil, fmt.Errorf("start meeting browser: %w", err)
	}
	m.br = br
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

func (m *containerMedia) Close() error {
	if m.browser != nil {
		_ = m.browser.Close()
		m.browser = nil
	}
	return m.deleteSession()
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
