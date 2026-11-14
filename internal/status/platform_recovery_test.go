package status

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platformhttp"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

type offlineRestartValidator struct{ calls int }

func (v *offlineRestartValidator) ValidateToken(string, string) (*terminal.TokenInfo, error) {
	v.calls++
	return nil, fmt.Errorf("synthetic unavailable token service")
}

func TestVPNRestartRouteIndependentOfBrokenOutboundPlatform(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	outbound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "synthetic wrong edge")
	}))
	defer outbound.Close()
	var health platformhttp.Health
	client := &http.Client{Transport: platformhttp.New(outbound.URL, platformhttp.Options{Observe: health.Observe}), Timeout: time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(outbound.URL + "/consume")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if health.Snapshot().FailingSince == nil {
		t.Fatal("fixture did not establish broken outbound")
	}
	called := 0
	s, coord, peer := newAgentControlServer(t, &AgentProviders{WorkerRestart: func() (any, error) { called++; return map[string]any{"ok": true, "restarting": true}, nil }})
	validator := &offlineRestartValidator{}
	s.tokenValidator = validator
	// The coordinator identity is verified locally, so restart does not
	// depend on outbound token validation.
	w := httptest.NewRecorder()
	s.buildControlMux().ServeHTTP(w, agentCoordinatorReq(http.MethodPost, "/agent/worker-restart", coord))
	if w.Code != http.StatusOK || called != 1 || validator.calls != 0 {
		t.Fatal("coordinator restart depended on outbound token validation")
	}
	// Other callers stay refused on the control mux and on the plaintext path.
	w = httptest.NewRecorder()
	s.buildControlMux().ServeHTTP(w, agentCoordinatorReq(http.MethodPost, "/agent/worker-restart", peer))
	if w.Code != http.StatusForbidden || called != 1 || validator.calls != 0 {
		t.Fatal("non-coordinator restart was not refused")
	}
	req := httptest.NewRequest(http.MethodPost, "/agent/worker-restart", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("Authorization", "Bearer synthetic-token")
	w = httptest.NewRecorder()
	s.buildMux().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || called != 1 || validator.calls != 0 {
		t.Fatal("plaintext restart was not refused")
	}
}

func TestWorkerEndpointCarriesPlatformEvidence(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	ok := time.Unix(1700000000, 0)
	failed := ok.Add(time.Minute)
	s := NewServer(ServerConfig{}, NewCollector(CollectorConfig{NodeName: "fixture", WorkerLiveness: func() *WorkerLiveness {
		return &WorkerLiveness{PlatformOKAt: &ok, PlatformFailingSince: &failed, PlatformLastOutcome: "wrong_answer", PlatformLastHTTPStatus: 404}
	}}))
	w := httptest.NewRecorder()
	s.handleWorker(w, httptest.NewRequest(http.MethodGet, "/worker", nil))
	var envelope struct{ Worker map[string]json.RawMessage }
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"platform_ok_at", "platform_failing_since", "platform_last_outcome", "platform_last_http_status"} {
		if len(envelope.Worker[key]) == 0 {
			t.Fatalf("/worker omitted %s", key)
		}
	}
}
