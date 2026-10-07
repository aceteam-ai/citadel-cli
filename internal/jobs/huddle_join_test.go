package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

const (
	huddleTestOrgID     = "11111111-1111-4111-8111-111111111111"
	huddleTestChannelID = "22222222-2222-4222-8222-222222222222"
	huddleTestCallID    = "33333333-3333-4333-8333-333333333333"
	huddleTestAgentID   = "44444444-4444-4444-8444-444444444444"
	huddleTestJobID     = "55555555-5555-4555-8555-555555555555"
	huddleTestAttemptID = "66666666-6666-4666-8666-666666666666"
)

// fakeHuddleBrowser is an injectable huddleBrowser that returns a scripted
// sequence of window.__huddleBotState JSON strings from Evaluate. Each Evaluate
// consumes the next scripted value; once exhausted it repeats the last one (so a
// terminal "joined"/"error" keeps being observed if the loop samples again).
type fakeHuddleBrowser struct {
	mu              sync.Mutex
	states          []string // JSON strings (or "" for "not published yet")
	idx             int
	navigated       []string
	evalCalls       int
	closed          bool
	closeErr        error
	closeErrors     []error
	closeCalls      int
	blockNavigate   bool
	blockEvaluate   bool
	navigateStarted chan struct{}
	evaluateStarted chan struct{}
}

func (f *fakeHuddleBrowser) NavigateContext(ctx context.Context, u string) error {
	f.mu.Lock()
	f.navigated = append(f.navigated, u)
	block, started := f.blockNavigate, f.navigateStarted
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeHuddleBrowser) EvaluateContext(ctx context.Context, expr string) (any, error) {
	if f.evaluateStarted != nil {
		select {
		case f.evaluateStarted <- struct{}{}:
		default:
		}
	}
	if f.blockEvaluate {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evalCalls++
	if len(f.states) == 0 {
		return "", nil
	}
	i := f.idx
	if i >= len(f.states) {
		i = len(f.states) - 1
	} else {
		f.idx++
	}
	return f.states[i], nil
}

func (f *fakeHuddleBrowser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.closeCalls++
	if len(f.closeErrors) > 0 {
		err := f.closeErrors[0]
		f.closeErrors = f.closeErrors[1:]
		return err
	}
	return f.closeErr
}

// stateJSON builds a window.__huddleBotState JSON string for the fake browser.
func stateJSON(state, selfID string, peers, connected int) string {
	b, _ := json.Marshal(huddleBotState{
		State:              state,
		CallID:             huddleTestCallID,
		SelfID:             selfID,
		PeerCount:          peers,
		ConnectedPeerCount: connected,
		UpdatedAt:          1,
	})
	return string(b)
}

// newTestHuddleHandler wires a handler with fast poll budgets, fixed enrolled creds, an
// injected mint, and an injected browser.
func newTestHuddleHandler(deviceToken string, mint func(ctx context.Context, apiBase *url.URL, token string, p huddleJoinParams) (huddleToken, error), br *fakeHuddleBrowser) *HuddleJoinHandler {
	outboxDir, _ := os.MkdirTemp("", "citadel-huddle-test-outbox-")
	return &HuddleJoinHandler{
		WorkspaceDir: "/tmp",
		outboxDir:    outboxDir,
		credsFn: func() config.DeviceCreds {
			return config.DeviceCreds{Token: deviceToken, APIBaseURL: "https://aceteam.ai"}
		},
		mintToken: mint,
		acknowledgeReady: func(context.Context, *url.URL, huddleToken, huddleBotState) error {
			return nil
		},
		prepareTerminalIntent: func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
			return nil
		},
		authorizeNodeTeardown: func(context.Context, *url.URL, string, huddleJoinParams) (string, error) {
			return "77777777-7777-4777-8777-777777777777", nil
		},
		confirmNodeTeardown: func(context.Context, *url.URL, string, huddleJoinParams, string) error { return nil },
		newBrowser: func(ctx context.Context, p huddleJoinParams) (huddleBrowser, func(context.Context) error, error) {
			return br, func(context.Context) error { return br.Close() }, nil
		},
		connectTimeout: 200 * time.Millisecond,
		pollInterval:   2 * time.Millisecond,
	}
}

func okMint(tok huddleToken) func(ctx context.Context, apiBase *url.URL, token string, p huddleJoinParams) (huddleToken, error) {
	return func(ctx context.Context, apiBase *url.URL, token string, p huddleJoinParams) (huddleToken, error) {
		if tok.ChannelID == "" {
			tok.ChannelID = p.ChannelID
		}
		if tok.SelfID == "" {
			tok.SelfID = p.AgentID
		}
		if tok.SelfKind == "" {
			tok.SelfKind = "agent"
		}
		if tok.CallID == "" {
			tok.CallID = p.CallID
		}
		if tok.AttemptID == "" {
			tok.AttemptID = p.AttemptID
		}
		return tok, nil
	}
}

func testHuddleParams() huddleJoinParams {
	return huddleJoinParams{
		OrganizationID: huddleTestOrgID,
		ChannelID:      huddleTestChannelID,
		CallID:         huddleTestCallID,
		AgentID:        huddleTestAgentID,
		NodeID:         "1297",
		JobID:          huddleTestJobID,
		AttemptID:      huddleTestAttemptID,
	}
}

func huddleJob() *nexus.Job {
	return &nexus.Job{
		ID:   huddleTestJobID,
		Type: JobTypeHuddleJoinType,
		Payload: map[string]string{
			"organizationId":     huddleTestOrgID,
			"channelId":          huddleTestChannelID,
			"callId":             huddleTestCallID,
			"agentId":            huddleTestAgentID,
			"target_node":        "1297",
			"lifecycleAttemptId": huddleTestAttemptID,
			"api_base":           "https://aceteam.ai",
		},
	}
}

// TestHuddleJoin_ConnectingJoined asserts the handler patiently polls through
// startup and reports the final distinct-agent joined state.
func TestHuddleJoin_ConnectingJoined(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{
		"",                                // not published yet
		stateJSON("connecting", "", 0, 0), // mounting
		stateJSON("joined", huddleTestAgentID, 2, 2), // exact room + transport ready
	}}
	tok := huddleToken{Token: "act_secret", ChannelID: huddleTestChannelID, SelfID: huddleTestAgentID, SelfKind: "agent", CallID: huddleTestCallID, AttemptID: huddleTestAttemptID}
	h := newTestHuddleHandler("s3cr3t", okMint(tok), br)
	var terminalStatus string
	h.prepareTerminalIntent = func(_ context.Context, _ *url.URL, _ string, _ huddleJoinParams, status, _ string) error {
		terminalStatus = status
		return nil
	}

	out, err := h.Execute(JobContext{}, huddleJob())
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res["status"] != "completed" || res["state"] != "joined" {
		t.Errorf("status/state = %v/%v, want completed/joined", res["status"], res["state"])
	}
	if res["channel_id"] != huddleTestChannelID {
		t.Errorf("channel_id = %v, want normalized %s", res["channel_id"], huddleTestChannelID)
	}
	if res["self_id"] != huddleTestAgentID || res["self_kind"] != "agent" {
		t.Errorf("self identity = %v/%v, want %s/agent", res["self_id"], res["self_kind"], huddleTestAgentID)
	}
	if res["connected_peer_count"].(float64) != 2 {
		t.Errorf("connected_peer_count = %v, want 2", res["connected_peer_count"])
	}
	if br.evalCalls < 3 {
		t.Errorf("evalCalls = %d, expected the loop to poll through startup", br.evalCalls)
	}
	// The bot page URL must carry the token in the FRAGMENT and target the
	// normalized channel id.
	if len(br.navigated) != 1 {
		t.Fatalf("navigated = %v, want exactly one navigation", br.navigated)
	}
	nav := br.navigated[0]
	if !strings.Contains(nav, "/huddle-bot/"+huddleTestChannelID+"#token=act_secret") {
		t.Errorf("navigation URL %q missing normalized channel + fragment token", nav)
	}
	if !br.closed {
		t.Errorf("session cleanup (Close) was not called")
	}
	if terminalStatus != "completed" {
		t.Errorf("terminal status = %q, want completed", terminalStatus)
	}
}

// TestHuddleJoin_ErrorState asserts a terminal error state fails the job and
// surfaces the page's error string verbatim.
func TestHuddleJoin_ErrorState(t *testing.T) {
	errState, _ := json.Marshal(huddleBotState{State: "error", Error: "microphone permission denied"})
	br := &fakeHuddleBrowser{states: []string{
		stateJSON("connecting", "", 0, 0),
		string(errState),
	}}
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c"}), br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil {
		t.Fatal("Execute should have failed on error state")
	}
	if !strings.Contains(err.Error(), "microphone permission denied") {
		t.Errorf("error %q should surface the page error verbatim", err)
	}
}

// TestHuddleJoin_LobbyRejected asserts the client fails closed if the server
// violates the distinct-agent contract by placing an agent in a human lobby.
func TestHuddleJoin_LobbyRejected(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{stateJSON("lobby", "", 0, 0)}}
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c"}), br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil {
		t.Fatal("Execute should have timed out in the lobby")
	}
	if !strings.Contains(err.Error(), "lobby") || !strings.Contains(err.Error(), "admitted directly") {
		t.Errorf("lobby contract error = %q", err)
	}
}

// TestHuddleJoin_NeverPublishes asserts a page that never publishes a readiness
// signal fails via the connect budget (NOT a parse error) — the most likely
// real-world first-run failure.
func TestHuddleJoin_NeverPublishes(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{""}} // always ""
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c"}), br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil {
		t.Fatal("Execute should have failed when no signal is published")
	}
	if !strings.Contains(err.Error(), "never published") {
		t.Errorf("error %q should explain the page never published a signal", err)
	}
}

// TestHuddleJoin_JoinedIsTerminal asserts that once joined is observed the loop
// returns immediately and does NOT re-sample (so a post-join left can't race a
// confirmed success into failure).
func TestHuddleJoin_JoinedIsTerminal(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{
		stateJSON("joined", huddleTestAgentID, 1, 1),
		stateJSON("left", huddleTestAgentID, 0, 0), // would fail if re-sampled
	}}
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c"}), br)

	out, err := h.Execute(JobContext{}, huddleJob())
	if err != nil {
		t.Fatalf("Execute should have succeeded on first joined, got: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if res["status"] != "completed" {
		t.Errorf("status = %v, want completed", res["status"])
	}
	if br.idx > 1 {
		t.Errorf("browser was sampled %d times; joined should be terminal (no re-poll into left)", br.idx)
	}
}

// TestHuddleJoin_PollHonorsCancellation proves an in-flight readiness wait
// wakes promptly when the worker's per-job context is cancelled. In
// particular, it must not remain stuck in the poll interval until the next CDP
// sample after the runner has already timed the job out.
func TestHuddleJoin_PollHonorsCancellation(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{stateJSON("connecting", "", 0, 0)}}
	jobCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	_, err := pollForHuddleJoined(JobContext{Ctx: jobCtx}, br, huddlePollOpts{
		connectTimeout: 30 * time.Second,
		interval:       10 * time.Second,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("poll error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled poll took %s; want prompt wakeup before the 10s interval", elapsed)
	}
}

// TestHuddleJoin_MissingDeviceToken asserts the handler fails closed before any
// container work when the node has no enrolled device credential.
func TestHuddleJoin_MissingDeviceToken(t *testing.T) {
	br := &fakeHuddleBrowser{}
	minted := false
	h := newTestHuddleHandler("", func(ctx context.Context, apiBase *url.URL, token string, p huddleJoinParams) (huddleToken, error) {
		minted = true
		return huddleToken{}, nil
	}, br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil {
		t.Fatal("Execute should fail with no device token configured")
	}
	if !strings.Contains(err.Error(), "device API token") {
		t.Errorf("error %q should identify the missing enrolled credential", err)
	}
	if minted {
		t.Error("mint must not be attempted when the device token is missing (fail closed first)")
	}
	if len(br.navigated) != 0 {
		t.Error("browser must not be launched when the device token is missing")
	}
}

func TestHuddleJoin_MissingEnrolledAPIBaseFailsClosed(t *testing.T) {
	br := &fakeHuddleBrowser{}
	minted := false
	h := newTestHuddleHandler("device-token", func(context.Context, *url.URL, string, huddleJoinParams) (huddleToken, error) {
		minted = true
		return huddleToken{}, nil
	}, br)
	h.credsFn = func() config.DeviceCreds {
		return config.DeviceCreds{Token: "device-token"}
	}
	job := huddleJob()
	job.Payload["api_base"] = "https://aceteam.ai"

	_, err := h.Execute(JobContext{}, job)
	if err == nil || !strings.Contains(err.Error(), "api_base_url") || !strings.Contains(err.Error(), "citadel init") {
		t.Fatalf("Execute error = %v, want fail-closed re-enrollment diagnosis", err)
	}
	if minted || len(br.navigated) != 0 {
		t.Fatal("payload api_base must not repair a missing enrolled credential origin")
	}
}

func TestHuddleJoin_PayloadAPIBaseCannotRedirectCredential(t *testing.T) {
	br := &fakeHuddleBrowser{}
	minted := false
	h := newTestHuddleHandler("device-secret", func(context.Context, *url.URL, string, huddleJoinParams) (huddleToken, error) {
		minted = true
		return huddleToken{}, nil
	}, br)
	job := huddleJob()
	job.Payload["api_base"] = "https://attacker.example"

	_, err := h.Execute(JobContext{}, job)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Execute error = %v, want enrolled-origin mismatch", err)
	}
	if minted || len(br.navigated) != 0 {
		t.Fatal("an untrusted payload api_base must fail before mint or browser launch")
	}
}

func TestHuddleJoin_UsesConvergedCredentialOrigin(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{stateJSON("joined", huddleTestAgentID, 1, 1)}}
	var gotBase, gotToken string
	h := newTestHuddleHandler("ignored", func(_ context.Context, base *url.URL, token string, _ huddleJoinParams) (huddleToken, error) {
		gotBase, gotToken = base.String(), token
		return huddleToken{Token: "bot", ChannelID: huddleTestChannelID, SelfID: huddleTestAgentID, SelfKind: "agent", CallID: huddleTestCallID, AttemptID: huddleTestAttemptID}, nil
	}, br)
	h.credsFn = func() config.DeviceCreds {
		return config.DeviceCreds{Token: "enrolled-device-token", APIBaseURL: "https://ORG.EXAMPLE/"}
	}
	job := huddleJob()
	job.Payload["api_base"] = "https://org.example"
	if _, err := h.Execute(JobContext{}, job); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if gotBase != "https://org.example" || gotToken != "enrolled-device-token" {
		t.Fatalf("mint auth = (%q, %q), want converged credential origin/token", gotBase, gotToken)
	}
}

func TestParseTrustedHuddleAPIBase(t *testing.T) {
	for _, raw := range []string{
		"http://org.example", "ftp://org.example", "https://user:pass@org.example",
		"https://org.example/prefix", "https://org.example?q=x", "https://org.example#fragment",
		"//org.example", "https:///missing-host",
	} {
		if _, err := parseTrustedHuddleAPIBase(raw); err == nil {
			t.Errorf("parseTrustedHuddleAPIBase(%q) succeeded, want rejection", raw)
		}
	}
	for _, raw := range []string{"https://org.example", "http://127.0.0.1:3000", "http://localhost:3000"} {
		if _, err := parseTrustedHuddleAPIBase(raw); err != nil {
			t.Errorf("parseTrustedHuddleAPIBase(%q) error = %v", raw, err)
		}
	}
}

// TestHuddleJoin_MissingParams asserts required-field validation.
func TestHuddleJoin_MissingParams(t *testing.T) {
	h := NewHuddleJoinHandler("/tmp")
	for _, tc := range []map[string]string{
		{"agent_id": "a"},   // no channel_id
		{"channel_id": "c"}, // no agent_id
	} {
		if _, err := h.Execute(JobContext{}, &nexus.Job{ID: "j", Payload: tc}); err == nil {
			t.Errorf("payload %v should be rejected", tc)
		}
	}
}

// TestMintHuddleBotToken_RequestShape pins the mint request contract (body
// {agentId, channelId} + Authorization: Bearer <device token>) against a mock endpoint,
// so a future aceteam-side contract drift shows up as a red test.
func TestMintHuddleBotToken_RequestShape(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":              "act_xyz",
			"channelId":          huddleTestChannelID,
			"selfId":             huddleTestAgentID,
			"selfKind":           "agent",
			"callId":             huddleTestCallID,
			"lifecycleAttemptId": huddleTestAttemptID,
			"expiresAt":          "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	tok, err := mintHuddleBotToken(context.Background(), srv.Client(), base, "top-secret",
		testHuddleParams())
	if err != nil {
		t.Fatalf("mint returned error: %v", err)
	}
	if gotPath != "/api/huddle-bot/token" {
		t.Errorf("path = %q, want /api/huddle-bot/token", gotPath)
	}
	if gotAuth != "Bearer top-secret" {
		t.Errorf("auth = %q, want Bearer top-secret", gotAuth)
	}
	if gotBody["agentId"] != huddleTestAgentID || gotBody["channelId"] != huddleTestChannelID || gotBody["callId"] != huddleTestCallID || gotBody["jobId"] != huddleTestJobID || gotBody["lifecycleAttemptId"] != huddleTestAttemptID || gotBody["terminalProtocol"] != "coordinator-v1" {
		t.Errorf("body = %v, want the exact lifecycle tuple", gotBody)
	}
	if tok.Token != "act_xyz" || tok.ChannelID != huddleTestChannelID || tok.SelfID != huddleTestAgentID || tok.SelfKind != "agent" || tok.CallID != huddleTestCallID || tok.AttemptID != huddleTestAttemptID {
		t.Errorf("parsed token = %+v, want distinct agent identity contract populated", tok)
	}
}

func TestMintHuddleBotToken_RejectsLegacyOrMismatchedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "legacy author identity",
			body: map[string]any{
				"token": "act_xyz", "channelId": huddleTestChannelID,
				"selfUserId": "author-1", "access": "member",
				"callId": huddleTestCallID, "lifecycleAttemptId": huddleTestAttemptID,
			},
			want: "distinct agent identity",
		},
		{
			name: "different agent",
			body: map[string]any{
				"token": "act_xyz", "channelId": huddleTestChannelID,
				"selfId": "agent-2", "selfKind": "agent",
				"callId": huddleTestCallID, "lifecycleAttemptId": huddleTestAttemptID,
			},
			want: "unexpected agent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()

			base, _ := url.Parse(srv.URL)
			_, err := mintHuddleBotToken(context.Background(), srv.Client(), base, "device-token",
				testHuddleParams())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mint error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestHuddleJoin_JoinedIdentityMustMatchMint(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{stateJSON("joined", "agent-2", 1, 1)}}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "chan-1"}), br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil || !strings.Contains(err.Error(), "unexpected peer") {
		t.Fatalf("Execute error = %v, want joined identity mismatch", err)
	}
	if !br.closed {
		t.Fatal("identity mismatch did not clean up meeting session")
	}
}

// TestMintHuddleBotToken_Non200 asserts a non-200 (e.g. 403 unreachable channel)
// surfaces the status + backend message and never a token.
func TestMintHuddleBotToken_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"owner cannot access this channel"}`))
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	_, err := mintHuddleBotToken(context.Background(), srv.Client(), base, "device-token",
		huddleJoinParams{ChannelID: "c", AgentID: "a"})
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "owner cannot access") {
		t.Errorf("error %q should carry status + backend message", err)
	}
}

func TestMintHuddleBotToken_RedactsCredentialFromBackendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad device-secret"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	_, err := mintHuddleBotToken(context.Background(), srv.Client(), base, "device-secret",
		huddleJoinParams{ChannelID: "c", AgentID: "a"})
	if err == nil || strings.Contains(err.Error(), "device-secret") || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("mint error was not credential-safe: %v", err)
	}
}

func TestMintHuddleBotToken_RefusesRedirectWithoutCredentialLeak(t *testing.T) {
	var leakedAuth string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	redirects := 0
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects++
		http.Redirect(w, r, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer start.Close()
	base, _ := url.Parse(start.URL)

	_, err := mintHuddleBotToken(context.Background(), start.Client(), base, "device-secret",
		huddleJoinParams{ChannelID: "c", AgentID: "a"})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("mint error = %v, want redirect status failure", err)
	}
	if redirects != 1 || leakedAuth != "" {
		t.Fatalf("redirect requests = %d, destination auth = %q; credential must not leave enrolled origin", redirects, leakedAuth)
	}
}

func TestHuddleJoin_CancelledAfterMintNeverLaunchesBrowser(t *testing.T) {
	jobCtx, cancel := context.WithCancel(context.Background())
	launched := false
	h := newTestHuddleHandler("device-token", func(context.Context, *url.URL, string, huddleJoinParams) (huddleToken, error) {
		cancel()
		return huddleToken{Token: "bot", ChannelID: huddleTestChannelID, SelfID: huddleTestAgentID, SelfKind: "agent", CallID: huddleTestCallID, AttemptID: huddleTestAttemptID}, nil
	}, &fakeHuddleBrowser{})
	h.newBrowser = func(context.Context, huddleJoinParams) (huddleBrowser, func(context.Context) error, error) {
		launched = true
		return nil, nil, nil
	}

	_, err := h.Execute(JobContext{Ctx: jobCtx}, huddleJob())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context.Canceled", err)
	}
	if launched {
		t.Fatal("browser launched after job cancellation")
	}
}

func TestHuddleJoin_CancellationInterruptsBlockedNavigateAndCleansUp(t *testing.T) {
	br := &fakeHuddleBrowser{blockNavigate: true, navigateStarted: make(chan struct{}, 1)}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), br)
	var terminalStatus string
	h.prepareTerminalIntent = func(_ context.Context, _ *url.URL, _ string, _ huddleJoinParams, status, _ string) error {
		terminalStatus = status
		return nil
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := h.Execute(JobContext{Ctx: jobCtx}, huddleJob())
		result <- err
	}()
	select {
	case <-br.navigateStarted:
	case <-time.After(time.Second):
		t.Fatal("navigate did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked navigate did not wake on cancellation")
	}
	if !br.closed {
		t.Fatal("session cleanup did not run after cancelled navigation")
	}
	if terminalStatus != "cancelled" {
		t.Fatalf("terminal status = %q, want cancelled", terminalStatus)
	}
}

func TestHuddleJoin_CancellationInterruptsBlockedEvaluateAndCleansUp(t *testing.T) {
	br := &fakeHuddleBrowser{blockEvaluate: true, evaluateStarted: make(chan struct{}, 1)}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), br)
	jobCtx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := h.Execute(JobContext{Ctx: jobCtx}, huddleJob())
		result <- err
	}()
	select {
	case <-br.evaluateStarted:
	case <-time.After(time.Second):
		t.Fatal("evaluate did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked evaluate did not wake on cancellation")
	}
	if !br.closed {
		t.Fatal("session cleanup did not run after cancelled readiness evaluation")
	}
}

func TestHuddleJoin_SuccessRequiresCleanup(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{stateJSON("joined", huddleTestAgentID, 1, 1)}, closeErr: errors.New("delete returned 500")}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), br)
	terminalCalls := 0
	h.prepareTerminalIntent = func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
		terminalCalls++
		return nil
	}

	_, err := h.Execute(JobContext{}, huddleJob())
	if !errors.Is(err, ErrHuddleTeardownPending) || !strings.Contains(err.Error(), "delete returned 500") {
		t.Fatalf("Execute error = %v, want cleanup failure", err)
	}
	if br.closeCalls != 1 {
		t.Fatalf("cleanup attempts = %d, want one authorized attempt", br.closeCalls)
	}
	if terminalCalls != 1 {
		t.Fatalf("terminal intents = %d, want durable intent before cleanup", terminalCalls)
	}
}

func TestHuddleJoin_TransientCleanupFailureReportsOnlyAfterConfirmedRetry(t *testing.T) {
	br := &fakeHuddleBrowser{
		states:      []string{stateJSON("joined", huddleTestAgentID, 1, 1)},
		closeErrors: []error{errors.New("delete returned 500"), nil},
	}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), br)
	var terminalStatus string
	h.prepareTerminalIntent = func(_ context.Context, _ *url.URL, _ string, _ huddleJoinParams, status, _ string) error {
		terminalStatus = status
		return nil
	}

	_, err := h.Execute(JobContext{}, huddleJob())
	if !errors.Is(err, ErrHuddleTeardownPending) {
		t.Fatalf("Execute error = %v, want first cleanup failure", err)
	}
	if br.closeCalls != 1 {
		t.Fatalf("cleanup attempts = %d, want one authorized attempt", br.closeCalls)
	}
	if terminalStatus != "completed" {
		t.Fatalf("terminal status = %q, want completed intent persisted before cleanup", terminalStatus)
	}
}

func TestHuddleJoin_RedeliveryRetainsDebtUntilNodeCleanupIsConfirmed(t *testing.T) {
	first := &fakeHuddleBrowser{
		states:   []string{stateJSON("joined", huddleTestAgentID, 1, 1)},
		closeErr: errors.New("delete returned 500"),
	}
	second := &fakeHuddleBrowser{
		states: []string{stateJSON("joined", huddleTestAgentID, 1, 1)},
	}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), first)
	launches := 0
	h.newBrowser = func(context.Context, huddleJoinParams) (huddleBrowser, func(context.Context) error, error) {
		launches++
		br := first
		if launches == 2 {
			br = second
		}
		return br, func(context.Context) error { return br.Close() }, nil
	}
	var terminalStatuses []string
	h.prepareTerminalIntent = func(_ context.Context, _ *url.URL, _ string, _ huddleJoinParams, status, _ string) error {
		terminalStatuses = append(terminalStatuses, status)
		return nil
	}

	_, err := h.Execute(JobContext{}, huddleJob())
	if !errors.Is(err, ErrHuddleTeardownPending) {
		t.Fatalf("first Execute error = %v, want ErrHuddleTeardownPending", err)
	}
	if len(terminalStatuses) != 1 || terminalStatuses[0] != "completed" {
		t.Fatalf("terminal intents = %v, want one completed before cleanup", terminalStatuses)
	}
}

func TestHuddleJoin_LaunchFailureRetainsDebtWhenFinalDeleteFails(t *testing.T) {
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), &fakeHuddleBrowser{})
	h.newBrowser = func(context.Context, huddleJoinParams) (huddleBrowser, func(context.Context) error, error) {
		return nil, func(context.Context) error { return errors.New("delete returned 500") }, errors.New("clear stale meetingd session")
	}
	terminalCalls := 0
	h.prepareTerminalIntent = func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
		terminalCalls++
		return nil
	}

	_, err := h.Execute(JobContext{}, huddleJob())
	if !errors.Is(err, ErrHuddleTeardownPending) {
		t.Fatalf("Execute error = %v, want ErrHuddleTeardownPending", err)
	}
	if terminalCalls != 1 {
		t.Fatalf("terminal intents = %d, want one before cleanup", terminalCalls)
	}
}

func TestHuddleJoin_FailureKeepsPrimaryAndLogsCleanupFailure(t *testing.T) {
	errState, _ := json.Marshal(huddleBotState{State: "error", Error: "join denied"})
	br := &fakeHuddleBrowser{states: []string{string(errState)}, closeErr: errors.New("delete returned 500")}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), br)
	var logs []string
	ctx := JobContext{LogFn: func(_ string, msg string) { logs = append(logs, msg) }}

	_, err := h.Execute(ctx, huddleJob())
	if err == nil || !strings.Contains(err.Error(), "join denied") || !strings.Contains(err.Error(), "delete returned 500") {
		t.Fatalf("Execute error = %v, want primary error plus durable teardown debt", err)
	}
}

func TestHuddleLifecycleEndpointsUseExactBoundIdentity(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer act_bound" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	tok := huddleToken{
		Token: "act_bound", ChannelID: huddleTestChannelID, SelfID: huddleTestAgentID,
		SelfKind: "agent", CallID: huddleTestCallID, AttemptID: huddleTestAttemptID,
	}
	state := huddleBotState{
		State: "joined", CallID: huddleTestCallID, SelfID: huddleTestAgentID,
		PeerCount: 2, ConnectedPeerCount: 2,
	}
	if err := acknowledgeHuddleReady(context.Background(), srv.Client(), base, tok, state); err != nil {
		t.Fatalf("acknowledgeHuddleReady: %v", err)
	}
	if err := reportHuddleTerminal(context.Background(), srv.Client(), base, tok, "cancelled", "host_removed"); err != nil {
		t.Fatalf("reportHuddleTerminal: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/api/huddle-bot/lifecycle/ready" || paths[1] != "/api/huddle-bot/lifecycle/terminal" {
		t.Fatalf("paths = %v", paths)
	}
	if bodies[0]["callId"] != huddleTestCallID || bodies[0]["selfId"] != huddleTestAgentID || bodies[0]["roomReady"] != true || bodies[0]["transportReady"] != true {
		t.Errorf("readiness body = %v", bodies[0])
	}
	if bodies[1]["callId"] != huddleTestCallID || bodies[1]["lifecycleAttemptId"] != huddleTestAttemptID || bodies[1]["status"] != "cancelled" || bodies[1]["detail"] != "host_removed" {
		t.Errorf("terminal body = %v", bodies[1])
	}
}

func TestReportHuddleTerminalRedactsCredentialAndRefusesRedirect(t *testing.T) {
	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"bad act_secret"}`))
	}))
	defer errorServer.Close()
	errorBase, _ := url.Parse(errorServer.URL)
	tok := huddleToken{Token: "act_secret", CallID: huddleTestCallID, AttemptID: huddleTestAttemptID}
	err := reportHuddleTerminal(context.Background(), errorServer.Client(), errorBase, tok, "failed", "test")
	if err == nil || strings.Contains(err.Error(), tok.Token) || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("terminal error was not credential-safe: %v", err)
	}

	var leakedAuth string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer start.Close()
	base, _ := url.Parse(start.URL)
	err = reportHuddleTerminal(context.Background(), start.Client(), base, tok, "failed", "test")
	if err == nil || !strings.Contains(err.Error(), "307") || strings.Contains(err.Error(), tok.Token) {
		t.Fatalf("terminal redirect error = %v", err)
	}
	if leakedAuth != "" {
		t.Fatalf("credential leaked to redirect destination: %q", leakedAuth)
	}
}

// TestHuddleBotURL_FragmentRedaction asserts the token rides the fragment and the
// redacted form never contains it.
func TestHuddleBotURL_FragmentRedaction(t *testing.T) {
	base, _ := url.Parse("https://aceteam.ai")
	full := huddleBotURL(base, "chan-1", "act_supersecret")
	if !strings.Contains(full, "/huddle-bot/chan-1#token=act_supersecret") {
		t.Errorf("full URL %q malformed", full)
	}
	red := redactedHuddleBotURL(base, "chan-1")
	redURL, _ := url.Parse(red)
	if strings.Contains(red, "act_supersecret") || redURL.Fragment != "token=<redacted>" {
		t.Errorf("redacted URL %q must not leak the token", red)
	}
	encoded := huddleBotURL(base, "chan-1", "token&with=# reserved")
	_, rawFragment, _ := strings.Cut(encoded, "#")
	fragment, parseErr := url.ParseQuery(rawFragment)
	if parseErr != nil || fragment.Get("token") != "token&with=# reserved" || strings.Contains(encoded, "token&with=# reserved") {
		t.Errorf("fragment token was not URL-encoded: %q", encoded)
	}
	if got := huddleBotURL(base, "org/channel", "token"); !strings.Contains(got, "/huddle-bot/org%2Fchannel#") || strings.Contains(got, "%252F") {
		t.Errorf("channel path segment was not escaped exactly once: %q", got)
	}
}

func TestParseHuddleJoinParams_TeardownUsesOriginalLifecycleJob(t *testing.T) {
	payload := huddleJob().Payload
	payload["teardownOnly"] = "true"
	payload["lifecycleJobId"] = huddleTestJobID
	p, err := parseHuddleJoinParams("77777777-7777-4777-8777-777777777777", JobTypeHuddleTeardownType, payload)
	if err != nil {
		t.Fatalf("parseHuddleJoinParams: %v", err)
	}
	if !p.TeardownOnly || p.JobID != huddleTestJobID {
		t.Fatalf("teardown params = %+v, want original lifecycle job %s", p, huddleTestJobID)
	}
}

func TestHuddleJoin_TeardownOnlyCleansThenConfirmsExactAttempt(t *testing.T) {
	job := huddleJob()
	job.Type = JobTypeHuddleTeardownType
	job.ID = "77777777-7777-4777-8777-777777777777"
	job.Payload["teardownOnly"] = "true"
	job.Payload["lifecycleJobId"] = huddleTestJobID
	cleanupCalls := 0
	confirmCalls := 0
	h := &HuddleJoinHandler{
		credsFn: func() config.DeviceCreds {
			return config.DeviceCreds{Token: "device-token", APIBaseURL: "https://aceteam.ai"}
		},
		cleanupNodeSession: func(_ context.Context, p huddleJoinParams) error {
			cleanupCalls++
			if p.JobID != huddleTestJobID || p.AttemptID != huddleTestAttemptID || p.NodeID != "1297" {
				t.Fatalf("cleanup identity = %+v", p)
			}
			return nil
		},
		authorizeNodeTeardown: func(context.Context, *url.URL, string, huddleJoinParams) (string, error) {
			return "88888888-8888-4888-8888-888888888888", nil
		},
		confirmNodeTeardown: func(_ context.Context, _ *url.URL, token string, p huddleJoinParams, leaseID string) error {
			confirmCalls++
			if token != "device-token" || p.CallID != huddleTestCallID || p.AgentID != huddleTestAgentID || p.JobID != huddleTestJobID || p.AttemptID != huddleTestAttemptID || leaseID != "88888888-8888-4888-8888-888888888888" {
				t.Fatalf("confirmation identity/token = %+v / %q", p, token)
			}
			return nil
		},
	}
	out, err := h.Execute(JobContext{}, job)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if cleanupCalls != 1 || confirmCalls != 1 {
		t.Fatalf("cleanup/confirm calls = %d/%d, want 1/1", cleanupCalls, confirmCalls)
	}
	if !strings.Contains(string(out), `"lifecycle_state":"teardown_confirmed"`) {
		t.Fatalf("output = %s", out)
	}
}

func TestHuddleJoin_TeardownOnlyDoesNotConfirmFailedCleanup(t *testing.T) {
	job := huddleJob()
	job.Type = JobTypeHuddleTeardownType
	job.ID = "77777777-7777-4777-8777-777777777777"
	job.Payload["teardownOnly"] = "true"
	job.Payload["lifecycleJobId"] = huddleTestJobID
	confirmCalls := 0
	h := &HuddleJoinHandler{
		credsFn: func() config.DeviceCreds {
			return config.DeviceCreds{Token: "device-token", APIBaseURL: "https://aceteam.ai"}
		},
		cleanupNodeSession: func(context.Context, huddleJoinParams) error {
			return errors.New("delete returned 500")
		},
		authorizeNodeTeardown: func(context.Context, *url.URL, string, huddleJoinParams) (string, error) {
			return "88888888-8888-4888-8888-888888888888", nil
		},
		confirmNodeTeardown: func(context.Context, *url.URL, string, huddleJoinParams, string) error {
			confirmCalls++
			return nil
		},
	}
	_, err := h.Execute(JobContext{}, job)
	if err == nil || !strings.Contains(err.Error(), "delete returned 500") {
		t.Fatalf("Execute error = %v", err)
	}
	if confirmCalls != 0 {
		t.Fatalf("confirmation called %d times after failed cleanup", confirmCalls)
	}
}

func TestHuddleJoin_TerminalIntentOutboxSurvivesRestartWithoutDeletingFirst(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	br := &fakeHuddleBrowser{states: []string{stateJSON("joined", huddleTestAgentID, 1, 1)}}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), br)
	h.outboxDir = filepath.Join(root, "outbox")
	h.prepareTerminalIntent = func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
		return errors.New("coordinator unavailable")
	}

	_, err := h.Execute(JobContext{}, huddleJob())
	if !errors.Is(err, ErrHuddleTerminalIntentPending) {
		t.Fatalf("Execute error = %v, want durable terminal-intent debt", err)
	}
	if br.closeCalls != 0 {
		t.Fatalf("meetingd DELETE ran %d times before terminal intent was durable", br.closeCalls)
	}
	path := filepath.Join(root, "outbox", huddleTestAttemptID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("durable outbox record missing: %v", err)
	}

	replayed := 0
	restarted := &HuddleJoinHandler{
		outboxDir: filepath.Join(root, "outbox"),
		credsFn: func() config.DeviceCreds {
			return config.DeviceCreds{Token: "device-token", APIBaseURL: "https://aceteam.ai"}
		},
		prepareTerminalIntent: func(_ context.Context, _ *url.URL, token string, p huddleJoinParams, status, _ string) error {
			replayed++
			if token != "device-token" || p.AttemptID != huddleTestAttemptID || status != "completed" {
				t.Fatalf("replayed terminal intent = token %q params %+v status %q", token, p, status)
			}
			return nil
		},
	}
	restarted.processLifecycleOutbox(context.Background())
	if replayed != 1 {
		t.Fatalf("replayed intents = %d, want 1", replayed)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outbox record retained after server ownership: %v", err)
	}
}

func TestHuddleJoin_TerminalIntentOutboxDropsExactAlreadyTerminalReplay(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &HuddleJoinHandler{
		outboxDir: filepath.Join(root, "outbox"),
		credsFn: func() config.DeviceCreds {
			return config.DeviceCreds{Token: "device-token", APIBaseURL: "https://aceteam.ai"}
		},
	}
	path, err := h.persistLifecycleDebt(testHuddleParams(), "completed", "")
	if err != nil {
		t.Fatal(err)
	}
	h.prepareTerminalIntent = func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
		return errHuddleAlreadyTerminal
	}
	h.processLifecycleOutbox(context.Background())
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("already-terminal outbox record retained: %v", err)
	}
}

func TestHuddleJoin_TerminalIntentOutboxRetriesCredentialResolutionInSameProcess(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialsAvailable := false
	prepareCalls := 0
	h := &HuddleJoinHandler{
		outboxDir: filepath.Join(root, "outbox"),
		credsFn: func() config.DeviceCreds {
			if !credentialsAvailable {
				return config.DeviceCreds{}
			}
			return config.DeviceCreds{Token: "rotated-token", APIBaseURL: "https://aceteam.ai"}
		},
		prepareTerminalIntent: func(_ context.Context, _ *url.URL, token string, _ huddleJoinParams, _, _ string) error {
			prepareCalls++
			if token != "rotated-token" {
				t.Fatalf("prepare token = %q", token)
			}
			return nil
		},
	}
	path, err := h.persistLifecycleDebt(testHuddleParams(), "failed", "")
	if err != nil {
		t.Fatal(err)
	}
	h.processLifecycleOutbox(context.Background())
	if prepareCalls != 0 {
		t.Fatalf("prepare called %d times without credentials", prepareCalls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("credential failure did not restore retryable record: %v", err)
	}
	credentialsAvailable = true
	h.processLifecycleOutbox(context.Background())
	if prepareCalls != 1 {
		t.Fatalf("prepare calls after credential recovery = %d, want 1", prepareCalls)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accepted recovered record retained: %v", err)
	}
}

func TestHuddleJoin_OrdersIntentAuthorizeDeleteConfirm(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{stateJSON("joined", huddleTestAgentID, 1, 1)}}
	h := newTestHuddleHandler("device-token", okMint(huddleToken{Token: "bot", ChannelID: "c"}), br)
	orderRoot := t.TempDir()
	if err := os.Chmod(orderRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	h.outboxDir = filepath.Join(orderRoot, "outbox")
	var order []string
	h.prepareTerminalIntent = func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
		order = append(order, "intent")
		return nil
	}
	h.authorizeNodeTeardown = func(context.Context, *url.URL, string, huddleJoinParams) (string, error) {
		order = append(order, "authorize")
		return "88888888-8888-4888-8888-888888888888", nil
	}
	h.newBrowser = func(context.Context, huddleJoinParams) (huddleBrowser, func(context.Context) error, error) {
		return br, func(context.Context) error {
			order = append(order, "delete")
			return nil
		}, nil
	}
	h.confirmNodeTeardown = func(context.Context, *url.URL, string, huddleJoinParams, string) error {
		order = append(order, "confirm")
		return nil
	}
	if _, err := h.Execute(JobContext{}, huddleJob()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := strings.Join(order, ","), "intent,authorize,delete,confirm"; got != want {
		t.Fatalf("lifecycle ordering = %s, want %s", got, want)
	}
}

func TestHuddleJoin_MeetingdSessionIdentityIsAttemptScoped(t *testing.T) {
	oldAttempt := "77777777-7777-4777-8777-777777777777"
	newAttempt := "99999999-9999-4999-8999-999999999999"
	oldSession := newContainerMedia(oldAttempt, "", "", time.Minute).sessionID
	newSession := newContainerMedia(newAttempt, "", "", time.Minute).sessionID
	if oldSession == newSession {
		t.Fatalf("old and successor attempts share meetingd session %q", oldSession)
	}
	if oldSession != sanitizeMeetingFilename(oldAttempt) || newSession != sanitizeMeetingFilename(newAttempt) {
		t.Fatalf("session ids = %q/%q, want attempt-derived", oldSession, newSession)
	}
}

func TestHuddleTeardownTypeRejectsJoinModeBeforeMintOrBrowser(t *testing.T) {
	job := huddleJob()
	job.Type = JobTypeHuddleTeardownType
	delete(job.Payload, "teardownOnly")
	mintCalls := 0
	h := &HuddleJoinHandler{
		mintToken: func(context.Context, *url.URL, string, huddleJoinParams) (huddleToken, error) {
			mintCalls++
			return huddleToken{}, nil
		},
	}
	_, err := h.Execute(JobContext{}, job)
	if err == nil || !strings.Contains(err.Error(), "requires teardownOnly=true") {
		t.Fatalf("Execute error = %v, want teardown-only rejection", err)
	}
	if mintCalls != 0 {
		t.Fatalf("mint called %d times for malicious teardown delivery", mintCalls)
	}
}

func TestHuddleJoinTypeRejectsTeardownModeBeforeCredentialOrCleanup(t *testing.T) {
	job := huddleJob()
	job.Payload["teardownOnly"] = "true"
	job.Payload["lifecycleJobId"] = huddleTestJobID
	credentialCalls := 0
	cleanupCalls := 0
	h := &HuddleJoinHandler{
		credsFn: func() config.DeviceCreds {
			credentialCalls++
			return config.DeviceCreds{Token: "device-token", APIBaseURL: "https://aceteam.ai"}
		},
		cleanupNodeSession: func(context.Context, huddleJoinParams) error {
			cleanupCalls++
			return nil
		},
	}
	_, err := h.Execute(JobContext{}, job)
	if err == nil || !strings.Contains(err.Error(), "HUDDLE_JOIN cannot run teardownOnly mode") {
		t.Fatalf("Execute error = %v, want join-mode rejection", err)
	}
	if credentialCalls != 0 || cleanupCalls != 0 {
		t.Fatalf("credential/cleanup calls = %d/%d, want 0/0", credentialCalls, cleanupCalls)
	}
}

func TestHuddleLifecycleOutboxRejectsSymlinkRootAndRecord(t *testing.T) {
	realDir := t.TempDir()
	symlinkRoot := filepath.Join(t.TempDir(), "outbox-link")
	if err := os.Symlink(realDir, symlinkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	h := &HuddleJoinHandler{outboxDir: symlinkRoot}
	if _, err := h.persistLifecycleDebt(testHuddleParams(), "failed", "test"); err == nil {
		t.Fatal("symlink outbox root was accepted")
	}

	secureDir := filepath.Join(t.TempDir(), "outbox")
	if err := os.Mkdir(secureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "planted.json")
	debt := huddleLifecycleDebt{
		OrganizationID: huddleTestOrgID, ChannelID: huddleTestChannelID,
		CallID: huddleTestCallID, AgentID: huddleTestAgentID, NodeID: "1297",
		JobID: huddleTestJobID, AttemptID: huddleTestAttemptID,
		Status: "failed", Detail: "planted",
	}
	raw, _ := json.Marshal(debt)
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(secureDir, huddleTestAttemptID+".json")); err != nil {
		t.Skipf("record symlink unavailable: %v", err)
	}
	prepareCalls := 0
	h = &HuddleJoinHandler{
		outboxDir: secureDir,
		prepareTerminalIntent: func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
			prepareCalls++
			return nil
		},
	}
	h.processLifecycleOutbox(context.Background())
	if prepareCalls != 0 {
		t.Fatalf("planted symlink triggered %d credentialed prepare calls", prepareCalls)
	}
}

func TestHuddleLifecycleOutboxBlockedReplayDoesNotBlockDistinctPersist(t *testing.T) {
	blockedRoot := t.TempDir()
	if err := os.Chmod(blockedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blockedRoot, "outbox")
	h := &HuddleJoinHandler{
		outboxDir: dir,
		credsFn: func() config.DeviceCreds {
			return config.DeviceCreds{Token: "device-token", APIBaseURL: "https://aceteam.ai"}
		},
	}
	if _, err := h.persistLifecycleDebt(testHuddleParams(), "failed", "first"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	h.prepareTerminalIntent = func(context.Context, *url.URL, string, huddleJoinParams, string, string) error {
		close(started)
		<-release
		return errors.New("still unavailable")
	}
	done := make(chan struct{})
	go func() {
		h.processLifecycleOutbox(context.Background())
		close(done)
	}()
	<-started
	second := testHuddleParams()
	second.AttemptID = "99999999-9999-4999-8999-999999999999"
	second.JobID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	persisted := make(chan error, 1)
	go func() {
		_, err := h.persistLifecycleDebt(second, "failed", "second")
		persisted <- err
	}()
	select {
	case err := <-persisted:
		if err != nil {
			t.Fatalf("persist distinct debt: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("blocked network replay held the outbox filesystem lock")
	}
	close(release)
	<-done
}

func TestHuddleLifecycleOutboxBoundsRecordCountButAllowsExactOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &HuddleJoinHandler{outboxDir: filepath.Join(root, "outbox")}
	var first huddleJoinParams
	for index := 1; index <= huddleOutboxMaxRecords; index++ {
		p := testHuddleParams()
		p.AttemptID = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		p.JobID = fmt.Sprintf("10000000-0000-4000-8000-%012d", index)
		if index == 1 {
			first = p
		}
		if _, err := h.persistLifecycleDebt(p, "failed", "bounded"); err != nil {
			t.Fatalf("persist record %d: %v", index, err)
		}
	}
	overflow := testHuddleParams()
	overflow.AttemptID = "00000000-0000-4000-8000-000000000099"
	overflow.JobID = "10000000-0000-4000-8000-000000000099"
	if _, err := h.persistLifecycleDebt(overflow, "failed", "overflow"); err == nil || !strings.Contains(err.Error(), "outbox is full") {
		t.Fatalf("overflow persist error = %v, want bounded refusal", err)
	}
	if _, err := h.persistLifecycleDebt(first, "cancelled", "updated"); err != nil {
		t.Fatalf("exact-attempt overwrite should remain available: %v", err)
	}
}

func TestHuddleLifecycleOutboxBoundsTotalActiveBytesIncludingProcessing(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "outbox")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A claimed .processing record remains active debt and therefore consumes
	// the same durable capacity as a .json record.
	large := filepath.Join(dir, "00000000-0000-4000-8000-000000000001.processing")
	if err := os.WriteFile(large, make([]byte, huddleOutboxTotalMaxBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &HuddleJoinHandler{outboxDir: dir}
	if _, err := h.persistLifecycleDebt(testHuddleParams(), "failed", "backpressure"); err == nil || !strings.Contains(err.Error(), "outbox is full") {
		t.Fatalf("byte-bound persist error = %v, want durable backpressure", err)
	}
}
