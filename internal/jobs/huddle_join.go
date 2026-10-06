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
// AceTeam-side contracts this file depends on (verified against the aceteam
// repo's settled #10087 S2 design, not re-implemented here). Deployment is
// gated on AceTeam implementing both the enrolled-device bearer and the
// distinct-agent response below; its current internal-secret/selfUserId route
// is deliberately incompatible. Citadel never loads a global service secret:
//
//	Token mint  POST {api_base}/api/huddle-bot/token
//	            Authorization: Bearer <enrolled device_api_token>
//	            body   { "agentId": <id>, "channelId": <id> }
//	            200 -> { token, expiresAt, selfId, selfKind, channelId }
//	            (channelId is the NORMALIZED id the page must join; selfId is
//	             the requested agent id and selfKind is always "agent".)
//
//	Bot page    {api_base}/huddle-bot/<channelId>#token=<token>
//	            The token rides the URL FRAGMENT (never the server / never a log).
//	            The page publishes window.__huddleBotState =
//	              { state:"connecting"|"lobby"|"joined"|"left"|"error",
//	                callId, selfId, peerCount, connectedPeerCount, error, updatedAt }
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/google/uuid"
)

// JobTypeHuddleJoinType is the wire type string for the huddle-join job,
// duplicated here as a local const (the worker package owns the canonical
// JobType constants and imports this package, not the reverse). Kept in sync with
// worker.JobTypeHuddleJoin.
const JobTypeHuddleJoinType = "HUDDLE_JOIN"

// Timeouts and cadence for the mint -> navigate -> confirm lifecycle.
// huddleConnectTimeout bounds the page mounting, acquiring the mic, opening the
// mesh, and reaching `joined`. Agents are always admitted participants under the
// distinct-agent contract; observing `lobby` is therefore a contract error.
const (
	huddleConnectTimeout   = 45 * time.Second
	huddlePollInterval     = 2 * time.Second
	huddleTokenHTTPTimeout = 20 * time.Second
	// huddleSessionMaxDuration is the container session's own reaper cap (meetingd
	// clears a session at its max_duration). Generous relative to a join+confirm so
	// a same-node retry is never blocked by our own not-yet-reaped session; Close
	// (DELETE /sessions) tears it down on the normal path well before this.
	huddleSessionMaxDuration = 1 * time.Hour
)

// huddleBrowser is the minimal CDP surface the huddle join drives — navigate to
// the bot page and poll the readiness signal. It is a subset of the
// meetingBrowser surface (meeting_media.go), so *platform.CDPBrowser satisfies it
// and a real container session's browser drops straight in. Tests inject a fake.
type huddleBrowser interface {
	NavigateContext(ctx context.Context, url string) error
	EvaluateContext(ctx context.Context, expression string) (any, error)
	Close() error
}

// huddleJoinParams is the typed, validated job payload. channel_id and agent_id
// are BOTH required: the aceteam mint route binds the server-minted credential
// to the agent's distinct mesh identity and gates + normalizes the target.
type huddleJoinParams struct {
	OrganizationID   string
	ChannelID        string
	CallID           string
	AgentID          string
	NodeID           string
	JobID            string
	AttemptID        string
	RequestedAPIBase string // legacy compatibility check only; never a destination
	Converse         bool
}

// parseHuddleJoinParams validates + normalizes the raw string payload.
func parseHuddleJoinParams(jobID string, payload map[string]string) (huddleJoinParams, error) {
	p := huddleJoinParams{
		OrganizationID:   strings.TrimSpace(payload["organizationId"]),
		ChannelID:        strings.TrimSpace(payload["channelId"]),
		CallID:           strings.TrimSpace(payload["callId"]),
		AgentID:          strings.TrimSpace(payload["agentId"]),
		NodeID:           strings.TrimSpace(payload["target_node"]),
		JobID:            strings.TrimSpace(jobID),
		AttemptID:        strings.TrimSpace(payload["lifecycleAttemptId"]),
		RequestedAPIBase: strings.TrimSpace(payload["api_base"]),
	}
	if raw := strings.TrimSpace(payload["converse"]); raw != "" {
		converse, err := strconv.ParseBool(raw)
		if err != nil {
			return huddleJoinParams{}, fmt.Errorf("invalid 'converse' value %q: must be true or false", raw)
		}
		p.Converse = converse
	}
	for field, value := range map[string]string{
		"organizationId":     p.OrganizationID,
		"channelId":          p.ChannelID,
		"callId":             p.CallID,
		"agentId":            p.AgentID,
		"jobId":              p.JobID,
		"lifecycleAttemptId": p.AttemptID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return huddleJoinParams{}, fmt.Errorf("%s must be a canonical UUID", field)
		}
	}
	if p.NodeID == "" || strings.HasPrefix(p.NodeID, "0") {
		return huddleJoinParams{}, fmt.Errorf("target_node must be a canonical positive integer string")
	}
	if nodeID, err := strconv.ParseUint(p.NodeID, 10, 64); err != nil || nodeID == 0 || strconv.FormatUint(nodeID, 10) != p.NodeID {
		return huddleJoinParams{}, fmt.Errorf("target_node must be a canonical positive integer string")
	}
	if p.Converse {
		return huddleJoinParams{}, fmt.Errorf("this HUDDLE_JOIN build does not support resident converse")
	}
	return p, nil
}

// huddleToken is the subset of the mint route's 200 response the node needs.
type huddleToken struct {
	Token string `json:"token"`
	// ChannelID is the NORMALIZED channel id the bot page must join (chat-v2
	// normalizes ids); prefer it over the raw payload channel_id when navigating.
	ChannelID string `json:"channelId"`
	SelfID    string `json:"selfId"`
	SelfKind  string `json:"selfKind"`
	CallID    string `json:"callId"`
	AttemptID string `json:"lifecycleAttemptId"`
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

	// credsFn reads the enrolled device token and its API base together at use
	// time. The payload is never allowed to choose where this credential is sent.
	credsFn func() config.DeviceCreds
	// mintToken mints a bot token from the aceteam backend. nil uses the real HTTP
	// implementation; tests inject a fake mint endpoint.
	mintToken func(ctx context.Context, apiBase *url.URL, deviceToken string, p huddleJoinParams) (huddleToken, error)
	// newBrowser launches (or reuses) the container session and returns the
	// CDP-driven browser plus a cleanup that tears the session down. nil uses the
	// real container-session implementation; tests inject a fake browser.
	newBrowser func(ctx context.Context, p huddleJoinParams) (br huddleBrowser, cleanup func(context.Context) error, err error)
	// acknowledgeReady promotes the exact server-owned lifecycle attempt only
	// after the browser proves its call/self/room/transport state. nil uses the
	// authenticated same-origin HTTP endpoint.
	acknowledgeReady func(ctx context.Context, apiBase *url.URL, tok huddleToken, final huddleBotState) error
	// reportTerminal persists the exact attempt's completed, failed, or
	// cancelled state. nil uses the authenticated lifecycle terminal endpoint.
	reportTerminal func(ctx context.Context, apiBase *url.URL, tok huddleToken, status, detail string) error

	// Tunables (zero => package defaults at use). Tests set them to milliseconds.
	connectTimeout time.Duration
	pollInterval   time.Duration
}

// NewHuddleJoinHandler constructs a handler rooted at the node workspace.
func NewHuddleJoinHandler(workspace string) *HuddleJoinHandler {
	return &HuddleJoinHandler{WorkspaceDir: workspace}
}

// Execute mints a token, drives the container browser to the bot page, confirms
// the agent joined, and returns a structured result. Order is deliberate: parse
// -> resolve enrolled device auth -> MINT -> launch browser. Minting BEFORE the
// container means a misconfigured token (401) or an unreachable/forbidden
// channel (403/404) costs nothing — we never pay for a container session we would
// immediately abandon.
func (h *HuddleJoinHandler) Execute(ctx JobContext, job *nexus.Job) (output []byte, err error) {
	p, err := parseHuddleJoinParams(job.ID, job.Payload)
	if err != nil {
		return nil, err
	}

	jobCtx := ctx.Context()
	apiBase, deviceToken, err := h.resolveDeviceAuth(p)
	if err != nil {
		return nil, err
	}

	ctx.Log("info", "     - [Job %s] HUDDLE_JOIN channel=%s agent=%s (api_origin=%s)", job.ID, p.ChannelID, p.AgentID, apiBase.Redacted())

	// Mint the short-lived bot token FIRST (before any container work).
	tok, err := h.mint(jobCtx, apiBase, deviceToken, p)
	if err != nil {
		return nil, fmt.Errorf("mint huddle bot token: %w", err)
	}
	if err := validateHuddleToken(tok, p); err != nil {
		return nil, fmt.Errorf("mint huddle bot token: %w", err)
	}
	defer func() {
		status := "completed"
		detail := ""
		if err != nil {
			status = "failed"
			detail = truncateHuddleDetail(err.Error())
			if errors.Is(err, context.Canceled) || errors.Is(jobCtx.Err(), context.Canceled) {
				status = "cancelled"
			}
		}
		reportCtx, cancel := context.WithTimeout(context.Background(), huddleTokenHTTPTimeout)
		defer cancel()
		if reportErr := h.terminal(reportCtx, apiBase, tok, status, detail); reportErr != nil {
			if err == nil {
				err = fmt.Errorf("report huddle lifecycle terminal state: %w", reportErr)
			} else {
				ctx.Log("error", "     - [Job %s] terminal lifecycle report failed: %v", job.ID, reportErr)
			}
		}
	}()
	if err := jobCtx.Err(); err != nil {
		return nil, fmt.Errorf("huddle join cancelled after token mint: %w", err)
	}
	// Navigate to the mint response's NORMALIZED channel id when present.
	joinChannel := tok.ChannelID
	if joinChannel == "" {
		joinChannel = p.ChannelID
	}
	ctx.Log("info", "     - [Job %s] minted bot token (self=%s, kind=%s, channel=%s)", job.ID, tok.SelfID, tok.SelfKind, joinChannel)

	// Launch (or reuse) the container session -> CDP browser.
	br, cleanup, err := h.launchBrowser(jobCtx, p)
	if err != nil {
		return nil, fmt.Errorf("launch huddle browser: %w", err)
	}
	cleaned := false
	defer func() {
		if cleanup != nil && !cleaned {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), meetingContainerHTTPTimeout)
			defer cancel()
			if cleanupErr := cleanup(cleanupCtx); cleanupErr != nil {
				ctx.Log("error", "     - [Job %s] huddle session cleanup failed after job error: %v", job.ID, cleanupErr)
			}
		}
	}()
	if err := jobCtx.Err(); err != nil {
		return nil, fmt.Errorf("huddle join cancelled after browser launch: %w", err)
	}

	// Navigate to the bot page. The token rides the URL FRAGMENT so it never
	// reaches the server; we also NEVER log the full URL (redacted form only) so a
	// live org-wildcard key is never written to the node journal.
	navURL := huddleBotURL(apiBase, joinChannel, tok.Token)
	if err := br.NavigateContext(jobCtx, navURL); err != nil {
		return nil, fmt.Errorf("navigate to huddle bot page %s: %w", redactedHuddleBotURL(apiBase, joinChannel), err)
	}
	if err := jobCtx.Err(); err != nil {
		return nil, fmt.Errorf("huddle join cancelled after browser navigation: %w", err)
	}
	ctx.Log("info", "     - [Job %s] navigated to %s; waiting for join", job.ID, redactedHuddleBotURL(apiBase, joinChannel))

	// Poll the readiness signal until joined / terminal failure / timeout.
	final, err := pollForHuddleJoined(ctx, br, huddlePollOpts{
		connectTimeout:   h.effConnectTimeout(),
		interval:         h.effPollInterval(),
		expectedCallID:   p.CallID,
		expectedSelfID:   p.AgentID,
		requireTransport: true,
	})
	if err != nil {
		return nil, err
	}

	if final.SelfID == "" {
		return nil, fmt.Errorf("huddle bot joined without publishing its distinct agent identity")
	}
	if final.SelfID != tok.SelfID {
		return nil, fmt.Errorf("huddle bot joined as unexpected peer %q (expected agent %q)", final.SelfID, tok.SelfID)
	}
	ctx.Log("info", "     - [Job %s] JOINED huddle %s (self=%s, peers=%d, connected=%d)",
		job.ID, joinChannel, final.SelfID, final.PeerCount, final.ConnectedPeerCount)

	if err := h.ready(jobCtx, apiBase, tok, final); err != nil {
		return nil, fmt.Errorf("acknowledge huddle media readiness: %w", err)
	}

	// A joined result is only successful after meetingd confirms teardown. This
	// prevents a leaked one-session-per-node slot from being reported as success.
	if cleanup != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), meetingContainerHTTPTimeout)
		cleanupErr := cleanup(cleanupCtx)
		cancel()
		cleaned = true
		if cleanupErr != nil {
			return nil, fmt.Errorf("huddle joined but session cleanup failed: %w", cleanupErr)
		}
	}

	out, _ := json.Marshal(map[string]any{
		"status":               "completed",
		"lifecycle_state":      "completed",
		"organization_id":      p.OrganizationID,
		"channel_id":           joinChannel,
		"agent_id":             p.AgentID,
		"node_id":              p.NodeID,
		"job_id":               p.JobID,
		"lifecycle_attempt_id": p.AttemptID,
		"self_id":              final.SelfID,
		"self_kind":            tok.SelfKind,
		"call_id":              final.CallID,
		"peer_count":           final.PeerCount,
		"connected_peer_count": final.ConnectedPeerCount,
		"state":                final.State,
	})
	return out, nil
}

func (h *HuddleJoinHandler) resolveDeviceAuth(p huddleJoinParams) (*url.URL, string, error) {
	var creds config.DeviceCreds
	if h.credsFn != nil {
		creds = h.credsFn()
	} else {
		creds = config.LoadDeviceCredsConverged()
	}
	deviceToken := strings.TrimSpace(creds.Token)
	if deviceToken == "" {
		return nil, "", fmt.Errorf("HUDDLE_JOIN requires an enrolled device API token; run citadel init on this node")
	}
	rawBase := strings.TrimSpace(creds.APIBaseURL)
	if rawBase == "" {
		return nil, "", fmt.Errorf("HUDDLE_JOIN requires api_base_url bound to the enrolled device token; re-run citadel init on this node")
	}
	base, err := parseTrustedHuddleAPIBase(rawBase)
	if err != nil {
		return nil, "", fmt.Errorf("invalid enrolled api_base_url: %w", err)
	}
	// api_base was present in early job payloads. Accept it only when it
	// canonicalizes to the enrolled origin; it can never redirect credentials.
	if p.RequestedAPIBase != "" {
		requested, parseErr := parseTrustedHuddleAPIBase(p.RequestedAPIBase)
		if parseErr != nil || requested.String() != base.String() {
			return nil, "", fmt.Errorf("job payload api_base does not match this node's enrolled API origin")
		}
	}
	return base, deviceToken, nil
}

func parseTrustedHuddleAPIBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.IsAbs() == false || u.Host == "" || u.Opaque != "" {
		return nil, fmt.Errorf("must be an absolute URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, fmt.Errorf("must be an origin only (no userinfo, path, query, or fragment)")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if u.Scheme != "https" {
		ip := net.ParseIP(host)
		if u.Scheme != "http" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, fmt.Errorf("must use HTTPS (HTTP is allowed only for an explicitly enrolled loopback origin)")
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = "", ""
	return u, nil
}

// mint delegates to the injected mint seam or the real HTTP implementation.
func (h *HuddleJoinHandler) mint(ctx context.Context, apiBase *url.URL, deviceToken string, p huddleJoinParams) (huddleToken, error) {
	if h.mintToken != nil {
		return h.mintToken(ctx, apiBase, deviceToken, p)
	}
	return mintHuddleBotToken(ctx, &http.Client{Timeout: huddleTokenHTTPTimeout}, apiBase, deviceToken, p)
}

// mintHuddleBotToken POSTs to {apiBase}/api/huddle-bot/token with the enrolled
// device token as the Bearer credential and returns the minted bot token. A non-200 is
// surfaced with the status + response body (the body carries the backend's error
// message and never the token, so it is safe to include).
func mintHuddleBotToken(ctx context.Context, client *http.Client, apiBase *url.URL, deviceToken string, p huddleJoinParams) (huddleToken, error) {
	body, err := json.Marshal(map[string]string{
		"agentId": p.AgentID, "channelId": p.ChannelID, "callId": p.CallID,
		"jobId": p.JobID, "lifecycleAttemptId": p.AttemptID,
	})
	if err != nil {
		return huddleToken{}, err
	}
	mintURL := *apiBase
	mintURL.Path = "/api/huddle-bot/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mintURL.String(), bytes.NewReader(body))
	if err != nil {
		return huddleToken{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	// Never follow redirects on an authenticated mint. Even same-origin
	// redirects are a contract error; cross-origin redirects are credential
	// exfiltration risks and must fail closed.
	noRedirectClient := *client
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return huddleToken{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		message := strings.ReplaceAll(strings.TrimSpace(string(raw)), deviceToken, "<redacted>")
		return huddleToken{}, fmt.Errorf("mint endpoint returned status %d: %s", resp.StatusCode, message)
	}
	var tok huddleToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return huddleToken{}, fmt.Errorf("parse mint response: %w", err)
	}
	if err := validateHuddleToken(tok, p); err != nil {
		return huddleToken{}, err
	}
	return tok, nil
}

// validateHuddleToken pins the cross-repository distinct-agent contract. The
// credential is useful only when the server binds it to the exact requested
// agent; accepting a human/author identity would recreate the collision that
// aceteam#10087 explicitly forbids.
func validateHuddleToken(tok huddleToken, p huddleJoinParams) error {
	if strings.TrimSpace(tok.Token) == "" {
		return fmt.Errorf("mint endpoint returned an empty token")
	}
	if strings.TrimSpace(tok.ChannelID) == "" {
		return fmt.Errorf("mint endpoint returned an empty normalized channel id")
	}
	if tok.SelfKind != "agent" {
		return fmt.Errorf("mint endpoint returned selfKind %q; expected distinct agent identity", tok.SelfKind)
	}
	if strings.TrimSpace(tok.SelfID) == "" {
		return fmt.Errorf("mint endpoint returned an empty agent selfId")
	}
	if tok.SelfID != p.AgentID {
		return fmt.Errorf("mint endpoint bound credential to unexpected agent %q (requested %q)", tok.SelfID, p.AgentID)
	}
	if tok.CallID != p.CallID {
		return fmt.Errorf("mint endpoint bound credential to unexpected call %q (requested %q)", tok.CallID, p.CallID)
	}
	if tok.AttemptID != p.AttemptID {
		return fmt.Errorf("mint endpoint bound credential to unexpected lifecycle attempt %q (requested %q)", tok.AttemptID, p.AttemptID)
	}
	return nil
}

func (h *HuddleJoinHandler) ready(ctx context.Context, apiBase *url.URL, tok huddleToken, final huddleBotState) error {
	if h.acknowledgeReady != nil {
		return h.acknowledgeReady(ctx, apiBase, tok, final)
	}
	return acknowledgeHuddleReady(ctx, &http.Client{Timeout: huddleTokenHTTPTimeout}, apiBase, tok, final)
}

func (h *HuddleJoinHandler) terminal(ctx context.Context, apiBase *url.URL, tok huddleToken, status, detail string) error {
	if h.reportTerminal != nil {
		return h.reportTerminal(ctx, apiBase, tok, status, detail)
	}
	return reportHuddleTerminal(ctx, &http.Client{Timeout: huddleTokenHTTPTimeout}, apiBase, tok, status, detail)
}

// acknowledgeHuddleReady sends the server-required boolean ACK only after the
// caller has obtained real source evidence: the meetingd-owned browser is in
// the exact call as the exact agent and all advertised room peers have an
// established transport. The booleans are therefore derived assertions, never
// caller optimism.
func acknowledgeHuddleReady(ctx context.Context, client *http.Client, apiBase *url.URL, tok huddleToken, final huddleBotState) error {
	if err := validateHuddleMediaReady(final, tok.CallID, tok.SelfID); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"callId": tok.CallID, "selfId": tok.SelfID,
		"roomReady": true, "transportReady": true,
	})
	if err != nil {
		return err
	}
	readyURL := *apiBase
	readyURL.Path = "/api/huddle-bot/lifecycle/ready"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, readyURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	noRedirectClient := *client
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusNoContent {
		message := strings.ReplaceAll(strings.TrimSpace(string(raw)), tok.Token, "<redacted>")
		return fmt.Errorf("readiness endpoint returned status %d: %s", resp.StatusCode, message)
	}
	return nil
}

func reportHuddleTerminal(ctx context.Context, client *http.Client, apiBase *url.URL, tok huddleToken, status, detail string) error {
	if status != "completed" && status != "failed" && status != "cancelled" {
		return fmt.Errorf("invalid huddle terminal status %q", status)
	}
	body, err := json.Marshal(map[string]string{
		"callId": tok.CallID, "lifecycleAttemptId": tok.AttemptID,
		"status": status, "detail": truncateHuddleDetail(detail),
	})
	if err != nil {
		return err
	}
	terminalURL := *apiBase
	terminalURL.Path = "/api/huddle-bot/lifecycle/terminal"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, terminalURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	noRedirectClient := *client
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusNoContent {
		message := strings.ReplaceAll(strings.TrimSpace(string(raw)), tok.Token, "<redacted>")
		return fmt.Errorf("terminal endpoint returned status %d: %s", resp.StatusCode, message)
	}
	return nil
}

func truncateHuddleDetail(detail string) string {
	runes := []rune(detail)
	if len(runes) > 400 {
		runes = runes[:400]
	}
	return string(runes)
}

func validateHuddleMediaReady(state huddleBotState, callID, selfID string) error {
	if state.State != "joined" {
		return fmt.Errorf("huddle room is not joined (state=%q)", state.State)
	}
	if state.CallID != callID {
		return fmt.Errorf("huddle browser joined unexpected call %q (expected %q)", state.CallID, callID)
	}
	if state.SelfID != selfID {
		return fmt.Errorf("huddle browser joined as unexpected peer %q (expected %q)", state.SelfID, selfID)
	}
	if state.PeerCount < 1 {
		return fmt.Errorf("huddle room has no remote peer to prove media transport readiness")
	}
	if state.ConnectedPeerCount != state.PeerCount {
		return fmt.Errorf("huddle media transport is not ready (%d/%d peers connected)", state.ConnectedPeerCount, state.PeerCount)
	}
	return nil
}

// launchBrowser delegates to the injected browser seam or the real container
// session implementation.
func (h *HuddleJoinHandler) launchBrowser(ctx context.Context, p huddleJoinParams) (huddleBrowser, func(context.Context) error, error) {
	if h.newBrowser != nil {
		return h.newBrowser(ctx, p)
	}
	return defaultHuddleBrowser(ctx, p)
}

// defaultHuddleBrowser launches a meeting-service container session (reusing the
// meeting media stack: this wires the in-container Chromium to the virtual mic +
// capture sink) and returns its CDP browser plus a session-teardown cleanup. The
// meeting module MUST be healthy on this node — huddle join is a WebRTC join that
// needs the container's headless Chromium; there is no host fallback here.
func defaultHuddleBrowser(ctx context.Context, p huddleJoinParams) (huddleBrowser, func(context.Context) error, error) {
	if !meetingdHealthyContext(ctx, &http.Client{Timeout: meetingContainerHealthTimeout}, meetingdBaseURL()) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("the meeting-service container is not healthy on this node; HUDDLE_JOIN requires it (install/enable the meeting module)")
	}
	// No recording for join+confirm, so the WAV paths are empty. Start() creates
	// the session and returns the CDP browser; Close() (cleanup) deletes it.
	media := newContainerMedia(p.ChannelID, "", "", huddleSessionMaxDuration)
	br, err := media.StartContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	return br, media.CloseContext, nil
}

// huddlePollOpts bundles the readiness poll budget and cadence.
type huddlePollOpts struct {
	connectTimeout   time.Duration
	interval         time.Duration
	expectedCallID   string
	expectedSelfID   string
	requireTransport bool
}

// huddleReadinessPage is the slice of the browser pollForHuddleJoined needs, so
// the poll loop is unit-testable without a full browser.
type huddleReadinessPage interface {
	EvaluateContext(ctx context.Context, expression string) (any, error)
}

// readHuddleBotStateJS reads window.__huddleBotState and returns it JSON-encoded,
// or "" when the page has not published it yet (still mounting). A try/catch keeps
// a not-yet-defined window from throwing.
const readHuddleBotStateJS = `(function(){try{var s=window.__huddleBotState;` +
	`return s?JSON.stringify(s):"";}catch(e){return "";}})()`

// readHuddleBotState evaluates the readiness signal. It returns (state, present,
// err): present=false means the page has not published a signal yet (keep
// polling), which is NOT an error — it is the expected first-N-polls condition.
func readHuddleBotState(ctx context.Context, page huddleReadinessPage) (huddleBotState, bool, error) {
	v, err := page.EvaluateContext(ctx, readHuddleBotStateJS)
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
// (`error`/`left`), or a timeout. `joined` must happen within connectTimeout. A
// transient Evaluate error or a not-yet-published signal is non-fatal.
func pollForHuddleJoined(ctx JobContext, page huddleReadinessPage, opts huddlePollOpts) (huddleBotState, error) {
	start := time.Now()
	connectDeadline := start.Add(opts.connectTimeout)
	jobCtx := ctx.Context()
	var last huddleBotState
	var everSaw bool

	for {
		select {
		case <-jobCtx.Done():
			return last, fmt.Errorf("huddle join cancelled: %w", jobCtx.Err())
		default:
		}

		st, present, err := readHuddleBotState(jobCtx, page)
		if err != nil {
			ctx.Log("warn", "     - huddle readiness probe errored (retrying): %v", err)
		} else if present {
			last, everSaw = st, true
			switch st.State {
			case "joined":
				if opts.expectedCallID != "" && st.CallID != opts.expectedCallID {
					return st, fmt.Errorf("huddle browser joined unexpected call %q (expected %q)", st.CallID, opts.expectedCallID)
				}
				if opts.expectedSelfID != "" && st.SelfID != opts.expectedSelfID {
					return st, fmt.Errorf("huddle browser joined as unexpected peer %q (expected %q)", st.SelfID, opts.expectedSelfID)
				}
				if opts.requireTransport {
					// `joined` proves roster + local mic only. Wait until the bot page's
					// actual RTCPeerConnection observations prove the remote room and
					// every advertised peer transport are connected.
					if st.PeerCount < 1 || st.ConnectedPeerCount != st.PeerCount {
						break
					}
				}
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
				return st, fmt.Errorf("huddle bot entered the lobby; distinct agent participants must be admitted directly")
			case "connecting":
				// Page mounting / acquiring mic / opening the mesh. Keep waiting.
			}
		}

		now := time.Now()
		switch {
		case now.After(connectDeadline):
			detail := "the bot never left 'connecting'"
			if everSaw {
				if last.State == "joined" && opts.requireTransport {
					detail = fmt.Sprintf("media transport never became ready (%d/%d peers connected)", last.ConnectedPeerCount, last.PeerCount)
				} else {
					detail = fmt.Sprintf("the bot never left '%s'", last.State)
				}
			} else {
				detail = "the bot page never published a readiness signal"
			}
			return last, fmt.Errorf("huddle join not confirmed within %s: %s (page failed to mount or join)", opts.connectTimeout, detail)
		}
		timer := time.NewTimer(opts.interval)
		select {
		case <-jobCtx.Done():
			timer.Stop()
			return last, fmt.Errorf("huddle join cancelled: %w", jobCtx.Err())
		case <-timer.C:
		}
	}
}

// huddleBotURL builds the bot-page URL with the token in the FRAGMENT (so it
// never reaches the server). Only this value is passed to Navigate; it is NEVER
// logged — use redactedHuddleBotURL for any log or error string.
func huddleBotURL(apiBase *url.URL, channelID, token string) string {
	u := *apiBase
	u.Path = "/huddle-bot/" + channelID
	u.RawPath = "/huddle-bot/" + url.PathEscape(channelID)
	u.Fragment, u.RawFragment = "", ""
	return u.String() + "#token=" + url.QueryEscape(token)
}

// redactedHuddleBotURL is huddleBotURL with the token replaced, safe to log.
func redactedHuddleBotURL(apiBase *url.URL, channelID string) string {
	u := *apiBase
	u.Path = "/huddle-bot/" + channelID
	u.RawPath = "/huddle-bot/" + url.PathEscape(channelID)
	u.Fragment, u.RawFragment = "", ""
	return u.Redacted() + "#token=" + url.QueryEscape("<redacted>")
}

func (h *HuddleJoinHandler) effConnectTimeout() time.Duration {
	if h.connectTimeout > 0 {
		return h.connectTimeout
	}
	return huddleConnectTimeout
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
