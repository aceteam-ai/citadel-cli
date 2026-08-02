package jobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// fakeHuddleBrowser is an injectable huddleBrowser that returns a scripted
// sequence of window.__huddleBotState JSON strings from Evaluate. Each Evaluate
// consumes the next scripted value; once exhausted it repeats the last one (so a
// terminal "joined"/"error" keeps being observed if the loop samples again).
type fakeHuddleBrowser struct {
	mu        sync.Mutex
	states    []string // JSON strings (or "" for "not published yet")
	idx       int
	navigated []string
	evalCalls int
	closed    bool
}

func (f *fakeHuddleBrowser) Navigate(u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.navigated = append(f.navigated, u)
	return nil
}

func (f *fakeHuddleBrowser) Evaluate(expr string) (any, error) {
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
	return nil
}

// stateJSON builds a window.__huddleBotState JSON string for the fake browser.
func stateJSON(state, selfID string, peers, connected int) string {
	b, _ := json.Marshal(huddleBotState{
		State:              state,
		CallID:             "call-123",
		SelfID:             selfID,
		PeerCount:          peers,
		ConnectedPeerCount: connected,
		UpdatedAt:          1,
	})
	return string(b)
}

// newTestHuddleHandler wires a handler with fast poll budgets, a fixed secret, an
// injected mint, and an injected browser.
func newTestHuddleHandler(secret string, mint func(ctx context.Context, apiBase, secret string, p huddleJoinParams) (huddleToken, error), br *fakeHuddleBrowser) *HuddleJoinHandler {
	return &HuddleJoinHandler{
		WorkspaceDir:   "/tmp",
		secretFn:       func() string { return secret },
		mintToken:      mint,
		newBrowser:     func(p huddleJoinParams) (huddleBrowser, func() error, error) { return br, br.Close, nil },
		connectTimeout: 200 * time.Millisecond,
		admitTimeout:   400 * time.Millisecond,
		pollInterval:   2 * time.Millisecond,
	}
}

func okMint(tok huddleToken) func(ctx context.Context, apiBase, secret string, p huddleJoinParams) (huddleToken, error) {
	return func(ctx context.Context, apiBase, secret string, p huddleJoinParams) (huddleToken, error) {
		return tok, nil
	}
}

func huddleJob() *nexus.Job {
	return &nexus.Job{
		ID:      "job-1",
		Type:    JobTypeHuddleJoinType,
		Payload: map[string]string{"channel_id": "chan-1", "agent_id": "agent-1", "api_base": "https://example.test"},
	}
}

// TestHuddleJoin_ConnectingLobbyJoined asserts the handler patiently polls
// through connecting -> lobby -> joined and reports the final joined state.
func TestHuddleJoin_ConnectingLobbyJoined(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{
		"",                                // not published yet
		stateJSON("connecting", "", 0, 0), // mounting
		stateJSON("lobby", "", 0, 0),      // waiting for admit
		stateJSON("lobby", "", 0, 0),
		stateJSON("joined", "agent-author-1", 2, 1), // admitted + mic
	}}
	tok := huddleToken{Token: "act_secret", ChannelID: "chan-norm", SelfUserID: "agent-author-1", Access: "read"}
	h := newTestHuddleHandler("s3cr3t", okMint(tok), br)

	out, err := h.Execute(JobContext{}, huddleJob())
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res["status"] != "joined" || res["state"] != "joined" {
		t.Errorf("status/state = %v/%v, want joined/joined", res["status"], res["state"])
	}
	if res["channel_id"] != "chan-norm" {
		t.Errorf("channel_id = %v, want normalized chan-norm", res["channel_id"])
	}
	if res["self_id"] != "agent-author-1" {
		t.Errorf("self_id = %v, want agent-author-1", res["self_id"])
	}
	if res["connected_peer_count"].(float64) != 1 {
		t.Errorf("connected_peer_count = %v, want 1", res["connected_peer_count"])
	}
	if res["access"] != "read" {
		t.Errorf("access = %v, want read", res["access"])
	}
	// The lobby state must have been observed before joined (patient polling).
	if br.evalCalls < 4 {
		t.Errorf("evalCalls = %d, expected the loop to poll through connecting+lobby", br.evalCalls)
	}
	// The bot page URL must carry the token in the FRAGMENT and target the
	// normalized channel id.
	if len(br.navigated) != 1 {
		t.Fatalf("navigated = %v, want exactly one navigation", br.navigated)
	}
	nav := br.navigated[0]
	if !strings.Contains(nav, "/huddle-bot/chan-norm#token=act_secret") {
		t.Errorf("navigation URL %q missing normalized channel + fragment token", nav)
	}
	if !br.closed {
		t.Errorf("session cleanup (Close) was not called")
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
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c", Access: "member"}), br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil {
		t.Fatal("Execute should have failed on error state")
	}
	if !strings.Contains(err.Error(), "microphone permission denied") {
		t.Errorf("error %q should surface the page error verbatim", err)
	}
}

// TestHuddleJoin_LobbyTimeout asserts an unadmitted lobby times out with an
// access-aware diagnosis rather than a generic timeout.
func TestHuddleJoin_LobbyTimeout(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{stateJSON("lobby", "", 0, 0)}}
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c", Access: "read"}), br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil {
		t.Fatal("Execute should have timed out in the lobby")
	}
	if !strings.Contains(err.Error(), "lobby") || !strings.Contains(err.Error(), "read") {
		t.Errorf("lobby timeout error %q should mention the lobby and access=read", err)
	}
}

// TestHuddleJoin_NeverPublishes asserts a page that never publishes a readiness
// signal fails via the connect budget (NOT a parse error) — the most likely
// real-world first-run failure.
func TestHuddleJoin_NeverPublishes(t *testing.T) {
	br := &fakeHuddleBrowser{states: []string{""}} // always ""
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c", Access: "member"}), br)

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
		stateJSON("joined", "self-1", 1, 1),
		stateJSON("left", "self-1", 0, 0), // would fail if re-sampled
	}}
	h := newTestHuddleHandler("s3cr3t", okMint(huddleToken{Token: "t", ChannelID: "c", Access: "member"}), br)

	out, err := h.Execute(JobContext{}, huddleJob())
	if err != nil {
		t.Fatalf("Execute should have succeeded on first joined, got: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if res["status"] != "joined" {
		t.Errorf("status = %v, want joined", res["status"])
	}
	if br.idx > 1 {
		t.Errorf("browser was sampled %d times; joined should be terminal (no re-poll into left)", br.idx)
	}
}

// TestHuddleJoin_MissingSecret asserts the handler fails closed (before any
// container work) when no internal secret is configured.
func TestHuddleJoin_MissingSecret(t *testing.T) {
	br := &fakeHuddleBrowser{}
	minted := false
	h := newTestHuddleHandler("", func(ctx context.Context, apiBase, secret string, p huddleJoinParams) (huddleToken, error) {
		minted = true
		return huddleToken{}, nil
	}, br)

	_, err := h.Execute(JobContext{}, huddleJob())
	if err == nil {
		t.Fatal("Execute should fail with no secret configured")
	}
	if !strings.Contains(err.Error(), envHuddleBotSecret) {
		t.Errorf("error %q should name the secret env var", err)
	}
	if minted {
		t.Error("mint must not be attempted when the secret is missing (fail closed first)")
	}
	if len(br.navigated) != 0 {
		t.Error("browser must not be launched when the secret is missing")
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
// {agentId, channelId} + Authorization: Bearer <secret>) against a mock endpoint,
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
			"token":      "act_xyz",
			"channelId":  "chan-normalized",
			"selfUserId": "author-9",
			"access":     "member",
			"expiresAt":  "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	tok, err := mintHuddleBotToken(context.Background(), srv.Client(), srv.URL, "top-secret",
		huddleJoinParams{ChannelID: "chan-1", AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("mint returned error: %v", err)
	}
	if gotPath != "/api/huddle-bot/token" {
		t.Errorf("path = %q, want /api/huddle-bot/token", gotPath)
	}
	if gotAuth != "Bearer top-secret" {
		t.Errorf("auth = %q, want Bearer top-secret", gotAuth)
	}
	if gotBody["agentId"] != "agent-1" || gotBody["channelId"] != "chan-1" {
		t.Errorf("body = %v, want {agentId:agent-1, channelId:chan-1}", gotBody)
	}
	if tok.Token != "act_xyz" || tok.ChannelID != "chan-normalized" || tok.Access != "member" {
		t.Errorf("parsed token = %+v, want token/channelId/access populated", tok)
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

	_, err := mintHuddleBotToken(context.Background(), srv.Client(), srv.URL, "s",
		huddleJoinParams{ChannelID: "c", AgentID: "a"})
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "owner cannot access") {
		t.Errorf("error %q should carry status + backend message", err)
	}
}

// TestHuddleBotURL_FragmentRedaction asserts the token rides the fragment and the
// redacted form never contains it.
func TestHuddleBotURL_FragmentRedaction(t *testing.T) {
	full := huddleBotURL("https://aceteam.ai/", "chan-1", "act_supersecret")
	if !strings.Contains(full, "/huddle-bot/chan-1#token=act_supersecret") {
		t.Errorf("full URL %q malformed", full)
	}
	red := redactedHuddleBotURL("https://aceteam.ai/", "chan-1")
	if strings.Contains(red, "act_supersecret") || !strings.Contains(red, "<redacted>") {
		t.Errorf("redacted URL %q must not leak the token", red)
	}
}
