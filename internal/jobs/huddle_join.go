// internal/jobs/huddle_join.go
//
// HUDDLE_JOIN handler (aceteam#7081 — the native-huddle agent join). Drives the
// EXISTING meeting-service container's headless Chromium (over CDP) to the
// aceteam-side headless "huddle bot" page and confirms the agent actually joined
// the native huddle call. This is the native-huddle sibling of MEETING_JOIN's
// Google Meet / Teams flows, but it is MUCH simpler because the join UX lives in
// aceteam's own React page (app/(huddle-bot)/huddle-bot/[channelId]) rather than
// a third-party DOM we must reverse-engineer — the page auto-joins audio-only and
// publishes a machine-pollable readiness signal we just read.
//
// SCOPE (this wave): JOIN + presence CONFIRMATION only. The handler mints a
// short-lived bot token, navigates the container browser to the bot page, polls
// `window.__huddleBotState.state` until `joined` (patiently handling the
// `connecting` and lobby states), reports the roster/presence, then tears the
// session down (the bot LEAVES). Staying resident in the call and bridging a
// realtime STT/TTS engine to the container's virtual mic (aceteam#7079) is the
// NEXT wave — deliberately NOT here.
//
// The container + virtual mic are reused verbatim from the meeting media stack
// (meeting_media.go): a session launch (POST /sessions) wires the in-container
// Chromium to the virtual mic + capture sink, so the huddle join runs against the
// same browser surface (platform.CDPBrowser) MEETING_JOIN drives.
//
// --- aceteam-side contracts this file depends on (verified against the aceteam
//
//	    repo's #7081 commits, NOT re-implemented here) ---
//
//		Token mint  POST {api_base}/api/huddle-bot/token
//		            Authorization: Bearer <internal secret>   (isInternalServiceRequest)
//		            body   { "agentId": <id>, "channelId": <id> }
//		            200 -> { token, expiresAt, selfUserId, channelId, access }
//		            (channelId is the NORMALIZED id the page must join; access is
//		             "member" => admitted directly, "read" => lobby-then-admit.)
//
//		Bot page    {api_base}/huddle-bot/<channelId>#token=<token>
//		            The token rides the URL FRAGMENT (never the server / never a log).
//		            The page publishes window.__huddleBotState =
//		              { state:"connecting"|"lobby"|"joined"|"left"|"error",
//		                callId, selfId, peerCount, connectedPeerCount, error, updatedAt }
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// JobTypeHuddleJoinType is the wire type string for the huddle-join job,
// duplicated here as a local const (the worker package owns the canonical
// JobType constants and imports this package, not the reverse). Kept in sync with
// worker.JobTypeHuddleJoin.
const JobTypeHuddleJoinType = "HUDDLE_JOIN"

// Env vars sourcing the internal service secret used to mint a bot token. The
// aceteam mint route gates on INTERNAL_AUTH_SECRET (isInternalServiceRequest);
// the node presents the SAME secret. CITADEL_HUDDLE_BOT_SECRET wins so an
// operator can scope a dedicated value, falling back to a co-located
// INTERNAL_AUTH_SECRET when both services share a host/env.
const (
	envHuddleBotSecret    = "CITADEL_HUDDLE_BOT_SECRET"
	envInternalAuthSecret = "INTERNAL_AUTH_SECRET"
	// envRealtimeToken is the fallback source for the realtime-engine auth token
	// used by the converse bridge (payload realtime_token wins). It is an AceTeam
	// API key (act_...) or user JWT the realtime WS server's authenticate() accepts
	// — NOT the huddle-bot mint token (see huddle_realtime_conn.go).
	envRealtimeToken = "CITADEL_REALTIME_TOKEN"
)

// defaultHuddleAPIBase is the AceTeam API base used when the payload omits
// api_base and no device-creds base URL is on disk.
const defaultHuddleAPIBase = "https://aceteam.ai"

// Timeouts and cadence for the mint -> navigate -> confirm lifecycle. Two
// distinct budgets (mirroring MEETING_JOIN's joinButtonTimeout vs admitTimeout):
//   - huddleConnectTimeout bounds reaching ANY non-`connecting` state (the page
//     mounting, acquiring the mic, and opening the mesh). Seconds, not minutes;
//     a page stuck in `connecting` past this never joined.
//   - huddleAdmitTimeout bounds the human-gated lobby wait once `lobby` is
//     observed (a non-member on a lobby-enabled call needs a host to admit it).
const (
	huddleConnectTimeout   = 45 * time.Second
	huddleAdmitTimeout     = 3 * time.Minute
	huddlePollInterval     = 2 * time.Second
	huddleTokenHTTPTimeout = 20 * time.Second
	// huddleSessionMaxDuration is the container session's own reaper cap (meetingd
	// clears a session at its max_duration). Generous relative to a join+confirm so
	// a same-node retry is never blocked by our own not-yet-reaped session; Close
	// (DELETE /sessions) tears it down on the normal path well before this.
	huddleSessionMaxDuration = 1 * time.Hour
	// converseSessionMaxDuration is the reaper cap for a RESIDENT converse session:
	// the bot stays in the call bridging audio for the meeting's whole length, so
	// the 1h join+confirm cap would kill the bridge mid-meeting. Matches the worker
	// long-session tier (4h) HUDDLE_JOIN now belongs to. Only used when converse is
	// enabled, so the plain path keeps the tighter cap.
	converseSessionMaxDuration = 4 * time.Hour
	// converseStatePollInterval is how often the resident bridge re-samples
	// window.__huddleBotState to notice the call ended (left/error).
	converseStatePollInterval = 3 * time.Second
)

// huddleBrowser is the minimal CDP surface the huddle join drives — navigate to
// the bot page and poll the readiness signal. It is a subset of the
// meetingBrowser surface (meeting_media.go), so *platform.CDPBrowser satisfies it
// and a real container session's browser drops straight in. Tests inject a fake.
type huddleBrowser interface {
	Navigate(url string) error
	Evaluate(expression string) (any, error)
	Close() error
}

// huddleJoinParams is the typed, validated job payload. channel_id and agent_id
// are BOTH required: the aceteam mint route needs agentId to resolve the agent's
// author (the bot's mesh identity) and channelId to gate + normalize the target.
type huddleJoinParams struct {
	ChannelID string
	AgentID   string
	APIBase   string // optional; falls back to device creds base, then default
	// Converse turns the join into a resident two-way voice bridge (aceteam#7079).
	// Default false => exact #667 join+confirm behavior (no capture/WS/mic).
	Converse bool
	// RealtimeURL optionally overrides the realtime WS endpoint (else derived from
	// APIBase). RealtimeToken is the engine auth token (else env CITADEL_REALTIME_TOKEN).
	RealtimeURL   string
	RealtimeToken string
}

// parseHuddleJoinParams validates + normalizes the raw string payload.
func parseHuddleJoinParams(payload map[string]string) (huddleJoinParams, error) {
	p := huddleJoinParams{
		ChannelID:     strings.TrimSpace(payload["channel_id"]),
		AgentID:       strings.TrimSpace(payload["agent_id"]),
		APIBase:       strings.TrimSpace(payload["api_base"]),
		Converse:      isTruthy(payload["converse"]),
		RealtimeURL:   strings.TrimSpace(payload["realtime_url"]),
		RealtimeToken: strings.TrimSpace(payload["realtime_token"]),
	}
	if p.ChannelID == "" {
		return huddleJoinParams{}, fmt.Errorf("job payload missing required 'channel_id' field")
	}
	if p.AgentID == "" {
		return huddleJoinParams{}, fmt.Errorf("job payload missing required 'agent_id' field")
	}
	return p, nil
}

// isTruthy parses the common truthy string forms used across citadel payload flags.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// huddleToken is the subset of the mint route's 200 response the node needs.
type huddleToken struct {
	Token string `json:"token"`
	// ChannelID is the NORMALIZED channel id the bot page must join (chat-v2
	// normalizes ids); prefer it over the raw payload channel_id when navigating.
	ChannelID  string `json:"channelId"`
	SelfUserID string `json:"selfUserId"`
	// Access is "member" (admitted directly) or "read" (lobby, needs host admit).
	Access    string `json:"access"`
	ExpiresAt string `json:"expiresAt"`
}

// huddleBotState mirrors aceteam's window.__huddleBotState readiness payload
// (utils/huddle/botState.ts). Null JSON fields decode to Go zero values, which is
// exactly what we want (callId/selfId/error -> "").
type huddleBotState struct {
	State              string  `json:"state"`
	CallID             string  `json:"callId"`
	SelfID             string  `json:"selfId"`
	PeerCount          int     `json:"peerCount"`
	ConnectedPeerCount int     `json:"connectedPeerCount"`
	Error              string  `json:"error"`
	UpdatedAt          float64 `json:"updatedAt"`
}

// HuddleJoinHandler handles HUDDLE_JOIN jobs. All external dependencies are
// injectable seams so the whole flow is unit-testable without a live container,
// mesh, or backend.
type HuddleJoinHandler struct {
	WorkspaceDir string

	// secretFn returns the internal service secret used to mint a bot token, read
	// at USE-time (like meeting_audio_backup's backupCreds) so a rotated secret is
	// honored. nil uses the env-var resolver.
	secretFn func() string
	// mintToken mints a bot token from the aceteam backend. nil uses the real HTTP
	// implementation; tests inject a fake mint endpoint.
	mintToken func(ctx context.Context, apiBase, secret string, p huddleJoinParams) (huddleToken, error)
	// newBrowser launches (or reuses) the container session and returns the
	// CDP-driven browser plus a cleanup that tears the session down. nil uses the
	// real container-session implementation; tests inject a fake browser. The
	// converse media (capture + speak on the same session) is returned separately
	// for the resident bridge; it is nil when this seam is a test fake (the
	// converse path is then exercised via the runConverse seam instead).
	newBrowser func(p huddleJoinParams) (br huddleBrowser, media converseMedia, cleanup func() error, err error)

	// runConverse runs the resident converse bridge after `joined`. nil uses the
	// real implementation (dial the realtime WS, bridge audio). Tests inject a fake
	// to assert it is invoked only when converse:true, without a live WS/engine.
	runConverse func(ctx JobContext, br huddleBrowser, media converseMedia, tok huddleToken, p huddleJoinParams) (converseStats, error)

	// Tunables (zero => package defaults at use). Tests set them to milliseconds.
	connectTimeout time.Duration
	admitTimeout   time.Duration
	pollInterval   time.Duration
}

// NewHuddleJoinHandler constructs a handler rooted at the node workspace.
func NewHuddleJoinHandler(workspace string) *HuddleJoinHandler {
	return &HuddleJoinHandler{WorkspaceDir: workspace}
}

// Execute mints a token, drives the container browser to the bot page, confirms
// the agent joined, and returns a structured result. Order is deliberate: parse
// -> resolve secret -> resolve base -> MINT -> launch browser. Minting BEFORE the
// container means a misconfigured secret (401) or an unreachable/forbidden
// channel (403/404) costs nothing — we never pay for a container session we would
// immediately abandon.
func (h *HuddleJoinHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	p, err := parseHuddleJoinParams(job.Payload)
	if err != nil {
		return nil, err
	}

	secret := h.resolveSecret()
	if secret == "" {
		return nil, fmt.Errorf("HUDDLE_JOIN requires the internal service secret; set %s (or %s) on this node",
			envHuddleBotSecret, envInternalAuthSecret)
	}
	apiBase := h.resolveAPIBase(p)

	ctx.Log("info", "     - [Job %s] HUDDLE_JOIN channel=%s agent=%s (api_base=%s)", job.ID, p.ChannelID, p.AgentID, apiBase)

	// Mint the short-lived bot token FIRST (before any container work).
	tok, err := h.mint(ctx.Context(), apiBase, secret, p)
	if err != nil {
		return nil, fmt.Errorf("mint huddle bot token: %w", err)
	}
	// Navigate to the mint response's NORMALIZED channel id when present.
	joinChannel := tok.ChannelID
	if joinChannel == "" {
		joinChannel = p.ChannelID
	}
	ctx.Log("info", "     - [Job %s] minted bot token (self=%s, access=%s, channel=%s)", job.ID, tok.SelfUserID, tok.Access, joinChannel)

	// Launch (or reuse) the container session -> CDP browser (+ converse media).
	br, media, cleanup, err := h.launchBrowser(p)
	if err != nil {
		return nil, fmt.Errorf("launch huddle browser: %w", err)
	}
	defer func() {
		if cleanup != nil {
			_ = cleanup()
		}
	}()

	// Navigate to the bot page. The token rides the URL FRAGMENT so it never
	// reaches the server; we also NEVER log the full URL (redacted form only) so a
	// live org-wildcard key is never written to the node journal.
	navURL := huddleBotURL(apiBase, joinChannel, tok.Token)
	if err := br.Navigate(navURL); err != nil {
		return nil, fmt.Errorf("navigate to huddle bot page %s: %w", redactedHuddleBotURL(apiBase, joinChannel), err)
	}
	ctx.Log("info", "     - [Job %s] navigated to %s; waiting for join", job.ID, redactedHuddleBotURL(apiBase, joinChannel))

	// Poll the readiness signal until joined / terminal failure / timeout.
	final, err := pollForHuddleJoined(ctx, br, huddlePollOpts{
		connectTimeout: h.effConnectTimeout(),
		admitTimeout:   h.effAdmitTimeout(),
		interval:       h.effPollInterval(),
		access:         tok.Access,
	})
	if err != nil {
		return nil, err
	}

	selfID := final.SelfID
	if selfID == "" {
		selfID = tok.SelfUserID
	}
	ctx.Log("info", "     - [Job %s] JOINED huddle %s (self=%s, peers=%d, connected=%d)",
		job.ID, joinChannel, selfID, final.PeerCount, final.ConnectedPeerCount)

	result := map[string]any{
		"status":               "joined",
		"channel_id":           joinChannel,
		"agent_id":             p.AgentID,
		"self_id":              selfID,
		"call_id":              final.CallID,
		"peer_count":           final.PeerCount,
		"connected_peer_count": final.ConnectedPeerCount,
		"access":               tok.Access,
		"state":                final.State,
	}

	// Resident converse bridge (aceteam#7079). Additive: only when converse:true.
	// It blocks (the session stays up, so `defer cleanup()` above tears it down
	// after) until the call ends / the job context cancels.
	if p.Converse {
		ctx.Log("info", "     - [Job %s] converse enabled; starting resident voice bridge", job.ID)
		stats, cErr := h.converse(ctx, br, media, tok, p)
		result["converse"] = stats
		if cErr != nil {
			// A converse failure does not un-join: we DID join. Report it in the
			// result but keep status joined so the backend sees a successful join.
			ctx.Log("warn", "     - [Job %s] converse bridge ended with error: %v", job.ID, cErr)
			result["converse_error"] = cErr.Error()
		}
		ctx.Log("info", "     - [Job %s] converse bridge ended (reason=%s, appended=%d, deltas=%d, spoken=%d)",
			job.ID, stats.StopReason, stats.AppendedFrames, stats.AudioDeltas, stats.SpokenChunks)
	}

	out, _ := json.Marshal(result)
	return out, nil
}

// converse runs (or delegates) the resident realtime bridge after the bot joined.
// It resolves the realtime token, dials the engine, and bridges room audio <-> the
// virtual mic until the call ends or ctx cancels.
func (h *HuddleJoinHandler) converse(ctx JobContext, br huddleBrowser, media converseMedia, tok huddleToken, p huddleJoinParams) (converseStats, error) {
	if h.runConverse != nil {
		return h.runConverse(ctx, br, media, tok, p)
	}
	if media == nil {
		return converseStats{StopReason: "no media"}, fmt.Errorf("converse requires a container media handle (meeting module)")
	}
	token := h.resolveRealtimeToken(p)
	if token == "" {
		return converseStats{StopReason: "no realtime token"}, fmt.Errorf(
			"converse requires a realtime engine token; set payload 'realtime_token' or env %s", envRealtimeToken)
	}
	apiBase := h.resolveAPIBase(p)
	conn, err := dialRealtime(apiBase, p.RealtimeURL, token, p.AgentID)
	if err != nil {
		return converseStats{StopReason: "ws dial failed"}, fmt.Errorf("dial realtime engine: %w", err)
	}
	defer conn.Close()

	// stateCheck lets the bridge notice the call ended by re-reading the bot page's
	// readiness signal (left/error) — the same signal the join poll used.
	stateCheck := func() (bool, string) {
		st, present, sErr := readHuddleBotState(br)
		if sErr != nil || !present {
			return false, "" // a transient probe miss is not "ended"
		}
		switch st.State {
		case "left":
			return true, "bot left the call"
		case "error":
			return true, "bot reported error: " + st.Error
		default:
			return false, ""
		}
	}

	bridge := newConverseBridge(conn, media, stateCheck,
		func(level, format string, args ...any) { ctx.Log(level, format, args...) },
		converseConfig{StatePollInterval: converseStatePollInterval})
	return bridge.Run(ctx.Context())
}

// resolveRealtimeToken returns the realtime-engine auth token: payload override
// first, then env CITADEL_REALTIME_TOKEN.
func (h *HuddleJoinHandler) resolveRealtimeToken(p huddleJoinParams) string {
	if p.RealtimeToken != "" {
		return p.RealtimeToken
	}
	return strings.TrimSpace(os.Getenv(envRealtimeToken))
}

// resolveSecret returns the internal service secret (seam-overridable).
func (h *HuddleJoinHandler) resolveSecret() string {
	if h.secretFn != nil {
		return h.secretFn()
	}
	return defaultHuddleSecret()
}

// defaultHuddleSecret reads the secret from env, preferring the citadel-scoped
// name and falling back to the shared internal-auth name.
func defaultHuddleSecret() string {
	if s := strings.TrimSpace(os.Getenv(envHuddleBotSecret)); s != "" {
		return s
	}
	return strings.TrimSpace(os.Getenv(envInternalAuthSecret))
}

// resolveAPIBase resolves the AceTeam API base: explicit payload override, then
// the node's device-creds base URL, then the default.
func (h *HuddleJoinHandler) resolveAPIBase(p huddleJoinParams) string {
	if p.APIBase != "" {
		return strings.TrimRight(p.APIBase, "/")
	}
	if base := config.LoadDeviceCreds(platform.ConfigDir()).APIBaseURL; base != "" {
		return strings.TrimRight(base, "/")
	}
	return defaultHuddleAPIBase
}

// mint delegates to the injected mint seam or the real HTTP implementation.
func (h *HuddleJoinHandler) mint(ctx context.Context, apiBase, secret string, p huddleJoinParams) (huddleToken, error) {
	if h.mintToken != nil {
		return h.mintToken(ctx, apiBase, secret, p)
	}
	return mintHuddleBotToken(ctx, &http.Client{Timeout: huddleTokenHTTPTimeout}, apiBase, secret, p)
}

// mintHuddleBotToken POSTs to {apiBase}/api/huddle-bot/token with the internal
// secret as the Bearer credential and returns the minted bot token. A non-200 is
// surfaced with the status + response body (the body carries the backend's error
// message and never the token, so it is safe to include).
func mintHuddleBotToken(ctx context.Context, client *http.Client, apiBase, secret string, p huddleJoinParams) (huddleToken, error) {
	body, err := json.Marshal(map[string]string{"agentId": p.AgentID, "channelId": p.ChannelID})
	if err != nil {
		return huddleToken{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/api/huddle-bot/token", bytes.NewReader(body))
	if err != nil {
		return huddleToken{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return huddleToken{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return huddleToken{}, fmt.Errorf("mint endpoint returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var tok huddleToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return huddleToken{}, fmt.Errorf("parse mint response: %w", err)
	}
	if tok.Token == "" {
		return huddleToken{}, fmt.Errorf("mint endpoint returned an empty token")
	}
	return tok, nil
}

// launchBrowser delegates to the injected browser seam or the real container
// session implementation. It returns the browser, the converse media handle (the
// SAME container session, for the resident bridge; may be nil for a test fake or
// the host path), and a session-teardown cleanup.
func (h *HuddleJoinHandler) launchBrowser(p huddleJoinParams) (huddleBrowser, converseMedia, func() error, error) {
	if h.newBrowser != nil {
		return h.newBrowser(p)
	}
	return defaultHuddleBrowser(p)
}

// defaultHuddleBrowser launches a meeting-service container session (reusing the
// meeting media stack: this wires the in-container Chromium to the virtual mic +
// capture sink) and returns its CDP browser, the containerMedia (for the converse
// bridge's capture+speak), plus a session-teardown cleanup. The meeting module MUST
// be healthy on this node — huddle join is a WebRTC join that needs the container's
// headless Chromium; there is no host fallback here.
func defaultHuddleBrowser(p huddleJoinParams) (huddleBrowser, converseMedia, func() error, error) {
	if !meetingdHealthy(&http.Client{Timeout: meetingContainerHealthTimeout}, meetingdBaseURL()) {
		return nil, nil, nil, fmt.Errorf("the meeting-service container is not healthy on this node; HUDDLE_JOIN requires it (install/enable the meeting module)")
	}
	// A resident converse session must outlive the 1h join+confirm reaper cap.
	maxDur := huddleSessionMaxDuration
	if p.Converse {
		maxDur = converseSessionMaxDuration
	}
	// No recording for join+confirm, so the WAV paths are empty. Start() creates
	// the session and returns the CDP browser; Close() (cleanup) deletes it.
	media := newContainerMedia(p.ChannelID, "", "", maxDur)
	br, err := media.Start()
	if err != nil {
		return nil, nil, nil, err
	}
	return br, media, media.Close, nil
}

// huddlePollOpts bundles the poll budgets + the token's access level (for a
// legible lobby-timeout diagnosis).
type huddlePollOpts struct {
	connectTimeout time.Duration
	admitTimeout   time.Duration
	interval       time.Duration
	access         string
}

// huddleReadinessPage is the slice of the browser pollForHuddleJoined needs, so
// the poll loop is unit-testable without a full browser.
type huddleReadinessPage interface {
	Evaluate(expression string) (any, error)
}

// readHuddleBotStateJS reads window.__huddleBotState and returns it JSON-encoded,
// or "" when the page has not published it yet (still mounting). A try/catch keeps
// a not-yet-defined window from throwing.
const readHuddleBotStateJS = `(function(){try{var s=window.__huddleBotState;` +
	`return s?JSON.stringify(s):"";}catch(e){return "";}})()`

// readHuddleBotState evaluates the readiness signal. It returns (state, present,
// err): present=false means the page has not published a signal yet (keep
// polling), which is NOT an error — it is the expected first-N-polls condition.
func readHuddleBotState(page huddleReadinessPage) (huddleBotState, bool, error) {
	v, err := page.Evaluate(readHuddleBotStateJS)
	if err != nil {
		return huddleBotState{}, false, err
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return huddleBotState{}, false, nil
	}
	var st huddleBotState
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return huddleBotState{}, false, fmt.Errorf("parse huddle bot state %q: %w", s, err)
	}
	return st, true, nil
}

// pollForHuddleJoined polls the readiness signal until the bot is `joined`
// (terminal SUCCESS — returned immediately, never re-sampled, so a post-join
// `left` can't race a confirmed success into a failure), a terminal failure
// (`error`/`left`), or a timeout. Timeout budgets are split: reaching any
// non-`connecting` state must happen within connectTimeout; once `lobby` is
// observed the human-gated admission gets the longer admitTimeout. A transient
// Evaluate error or a not-yet-published signal is non-fatal (keep polling).
func pollForHuddleJoined(ctx JobContext, page huddleReadinessPage, opts huddlePollOpts) (huddleBotState, error) {
	start := time.Now()
	connectDeadline := start.Add(opts.connectTimeout)
	var lobbyDeadline time.Time // set the first time `lobby` is observed
	var last huddleBotState
	var everSaw bool

	for {
		st, present, err := readHuddleBotState(page)
		if err != nil {
			ctx.Log("warn", "     - huddle readiness probe errored (retrying): %v", err)
		} else if present {
			last, everSaw = st, true
			switch st.State {
			case "joined":
				// Terminal success. Return NOW without another sample so a later
				// `left` (call ended after we confirmed) can't flip a real success.
				return st, nil
			case "error":
				msg := st.Error
				if msg == "" {
					msg = "unknown error"
				}
				return st, fmt.Errorf("huddle bot reported an error state: %s", msg)
			case "left":
				// `left` is only reachable AFTER a prior join (see botState.ts): the
				// bot joined then the call ended / it was removed before we sampled
				// `joined`. Fail this wave (join+confirm), but say so accurately.
				return st, fmt.Errorf("huddle bot left the call before its join was confirmed (state=left)")
			case "lobby":
				if lobbyDeadline.IsZero() {
					ctx.Log("info", "     - in huddle lobby (access=%s); waiting for host admission", opts.access)
					lobbyDeadline = time.Now().Add(opts.admitTimeout)
				}
			case "connecting":
				// Page mounting / acquiring mic / opening the mesh. Keep waiting.
			}
		}

		now := time.Now()
		switch {
		case !lobbyDeadline.IsZero() && now.After(lobbyDeadline):
			return last, fmt.Errorf("not admitted from the huddle lobby within %s (access=%q — the agent's owner may not be a channel member, so no host admitted the bot)", opts.admitTimeout, opts.access)
		case lobbyDeadline.IsZero() && now.After(connectDeadline):
			detail := "the bot never left 'connecting'"
			if everSaw {
				detail = fmt.Sprintf("the bot never left '%s'", last.State)
			} else {
				detail = "the bot page never published a readiness signal"
			}
			return last, fmt.Errorf("huddle join not confirmed within %s: %s (page failed to mount or join)", opts.connectTimeout, detail)
		}
		time.Sleep(opts.interval)
	}
}

// huddleBotURL builds the bot-page URL with the token in the FRAGMENT (so it
// never reaches the server). Only this value is passed to Navigate; it is NEVER
// logged — use redactedHuddleBotURL for any log or error string.
func huddleBotURL(apiBase, channelID, token string) string {
	return strings.TrimRight(apiBase, "/") + "/huddle-bot/" + url.PathEscape(channelID) + "#token=" + token
}

// redactedHuddleBotURL is huddleBotURL with the token replaced, safe to log.
func redactedHuddleBotURL(apiBase, channelID string) string {
	return strings.TrimRight(apiBase, "/") + "/huddle-bot/" + url.PathEscape(channelID) + "#token=<redacted>"
}

func (h *HuddleJoinHandler) effConnectTimeout() time.Duration {
	if h.connectTimeout > 0 {
		return h.connectTimeout
	}
	return huddleConnectTimeout
}

func (h *HuddleJoinHandler) effAdmitTimeout() time.Duration {
	if h.admitTimeout > 0 {
		return h.admitTimeout
	}
	return huddleAdmitTimeout
}

func (h *HuddleJoinHandler) effPollInterval() time.Duration {
	if h.pollInterval > 0 {
		return h.pollInterval
	}
	return huddlePollInterval
}

// Ensure HuddleJoinHandler implements JobHandler and CDPBrowser satisfies the
// huddleBrowser surface the container path relies on.
var (
	_ JobHandler    = (*HuddleJoinHandler)(nil)
	_ huddleBrowser = (*platform.CDPBrowser)(nil)
)
