package status

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

var agentControlCases = []struct {
	path   string
	method string
}{
	{"/agent/worker-status", http.MethodGet},
	{"/agent/node-info", http.MethodGet},
	{"/agent/doctor", http.MethodGet},
	{"/agent/config", http.MethodGet},
	{"/agent/logs", http.MethodGet},
	{"/agent/vendor-agents", http.MethodGet},
	{"/agent/set-log-level", http.MethodPost},
	{"/agent/resubscribe", http.MethodPost},
	{"/agent/worker-restart", http.MethodPost},
}

// vpnReq is shared with exposure tests. Source addresses are not identities.
func vpnReq(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "100.64.0.5:54321"
	return r
}

func newAgentControlServer(t *testing.T, p *AgentProviders) (*Server, *x509.Certificate, *x509.Certificate) {
	t.Helper()
	ca := newTestCA(t, "agent-test-ca")
	v, err := NewFabricCAVerifier(writeBundle(t, ca.certPEM), []string{DefaultCoordinatorSAN})
	if err != nil {
		t.Fatal(err)
	}
	serverCert := ca.issueLeaf(t, "node-server")
	coord := ca.issueLeaf(t, "coordinator", DefaultCoordinatorSAN)
	peer := ca.issueLeaf(t, "peer", "aceteam:node:peer", "aceteam:org:other")
	s := NewServer(ServerConfig{
		Agent: p, CAVerifier: v, ControlServerCert: &serverCert, SelfMeshIPs: selfIPs,
	}, NewCollector(CollectorConfig{NodeName: "test-node"}))
	return s, coord.Leaf, peer.Leaf
}

// These are injected request metadata for mux/authorization tests, not evidence
// of a successful TLS handshake. Separate server tests exercise the handshake.
func agentCoordinatorReq(method, target string, leaf *x509.Certificate) *http.Request {
	r := vpnReq(method, target)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return r
}

func agentSpyProviders(calls *int) *AgentProviders {
	read := func() any {
		(*calls)++
		return map[string]any{"ok": true}
	}
	action := func() (any, error) { return read(), nil }
	return &AgentProviders{
		WorkerStatus: read, NodeInfo: read, Doctor: read, Config: read,
		Logs: func(LogQuery) (string, error) {
			(*calls)++
			return "synthetic log", nil
		},
		VendorAgents: func(bool) (any, error) { return action() },
		SetLogLevel:  func(bool) any { return read() },
		Resubscribe:  action, WorkerRestart: action,
		Expose:   func(ExposeSpec) (any, error) { return action() },
		Unexpose: func(string) (any, error) { return action() },
	}
}

type agentTokenSpy struct{ calls int }

func (v *agentTokenSpy) ValidateToken(_ string, orgID string) (*terminal.TokenInfo, error) {
	v.calls++
	return &terminal.TokenInfo{UserID: "synthetic-user", OrgID: orgID}, nil
}

func TestAgentPlaintextAlwaysRefused(t *testing.T) {
	calls := 0
	s, coord, _ := newAgentControlServer(t, agentSpyProviders(&calls))
	validator := &agentTokenSpy{}
	s.tokenValidator, s.orgID = validator, "synthetic-org"
	for _, providers := range []*AgentProviders{s.agent, {}} {
		s.agent = providers
		mux := s.buildMux()
		for _, route := range agentControlCases {
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodConnect, "CUSTOM"} {
				for _, remote := range []string{"100.64.0.5:1234", selfMeshIP + ":1234", "127.0.0.1:1234", "192.168.1.50:1234"} {
					for _, variant := range []string{"none", "valid token", "injected coordinator TLS", "token and TLS"} {
						r := vpnReq(method, route.path+"?refresh=1&verbose=true")
						r.RemoteAddr = remote
						if variant == "valid token" || variant == "token and TLS" {
							r.Header.Set("Authorization", "Bearer valid-org-token")
						}
						if variant == "injected coordinator TLS" || variant == "token and TLS" {
							r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{coord}}
						}
						w := httptest.NewRecorder()
						mux.ServeHTTP(w, r)
						if w.Code != http.StatusForbidden || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != "{\"error\":\"agent endpoint requires coordinator mTLS on the control listener\"}\n" || calls != 0 || validator.calls != 0 {
							t.Fatalf("%s %s from %s (%s): code=%d body=%q provider calls=%d token calls=%d", method, route.path, remote, variant, w.Code, w.Body.String(), calls, validator.calls)
						}
					}
				}
			}
		}
	}
}

func TestAgentControlAuthorizationMethodsAndAvailability(t *testing.T) {
	calls := 0
	s, coord, peer := newAgentControlServer(t, agentSpyProviders(&calls))
	for _, providers := range []*AgentProviders{s.agent, {}} {
		s.agent = providers
		mux := s.buildControlMux()
		for _, route := range agentControlCases {
			for _, state := range []*tls.ConnectionState{nil, {}, {PeerCertificates: []*x509.Certificate{peer}}} {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					r := vpnReq(method, route.path)
					r.TLS = state
					r.Header.Set("Authorization", "Bearer valid-org-token")
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					if w.Code != http.StatusForbidden || calls != 0 {
						t.Fatalf("unauthorized %s %s: code=%d calls=%d", method, route.path, w.Code, calls)
					}
				}
			}
			wrong := http.MethodPost
			if route.method == http.MethodPost {
				wrong = http.MethodGet
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, agentCoordinatorReq(wrong, route.path, coord))
			if w.Code != http.StatusMethodNotAllowed || calls != 0 {
				t.Fatalf("authorized wrong method %s: code=%d calls=%d", route.path, w.Code, calls)
			}
		}
	}
	for _, route := range agentControlCases {
		w := httptest.NewRecorder()
		s.buildControlMux().ServeHTTP(w, agentCoordinatorReq(route.method, route.path, coord))
		if w.Code != http.StatusServiceUnavailable || w.Body.String() != "{\"error\":\"not available in this process\"}\n" || calls != 0 {
			t.Fatalf("authorized nil provider %s: code=%d body=%q calls=%d", route.path, w.Code, w.Body.String(), calls)
		}
	}
}

func TestAgentAbsentOnBothMuxesAndExposureAbsentOnControl(t *testing.T) {
	s, coord, _ := newAgentControlServer(t, nil)
	for _, mux := range []*http.ServeMux{s.buildMux(), s.buildControlMux()} {
		for _, route := range agentControlCases {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, agentCoordinatorReq(route.method, route.path, coord))
			if w.Code != http.StatusNotFound {
				t.Fatalf("absent Agent %s: code=%d", route.path, w.Code)
			}
		}
	}
	calls := 0
	s.agent = agentSpyProviders(&calls)
	for _, path := range []string{"/agent/expose", "/agent/unexpose"} {
		w := httptest.NewRecorder()
		s.buildControlMux().ServeHTTP(w, agentCoordinatorReq(http.MethodPost, path, coord))
		if w.Code != http.StatusNotFound || calls != 0 {
			t.Fatalf("exposure on control %s: code=%d calls=%d", path, w.Code, calls)
		}
	}
}

func TestAgentControlPreservesProviderContracts(t *testing.T) {
	calls := 0
	var gotQuery LogQuery
	var gotRefresh, gotVerbose bool
	result := map[string]any{"ok": true, "marker": "synthetic"}
	read := func() any {
		calls++
		return result
	}
	action := func() (any, error) { return read(), nil }
	p := &AgentProviders{
		WorkerStatus: read, NodeInfo: read, Doctor: read, Config: read,
		Logs: func(q LogQuery) (string, error) {
			calls++
			gotQuery = q
			return "line1\nline2", nil
		},
		VendorAgents: func(refresh bool) (any, error) { gotRefresh = refresh; return action() },
		SetLogLevel:  func(verbose bool) any { gotVerbose = verbose; return read() },
		Resubscribe:  action, WorkerRestart: action,
	}
	s, coord, _ := newAgentControlServer(t, p)
	mux := s.buildControlMux()
	request := func(method, target, wantBody string) {
		t.Helper()
		calls = 0
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, agentCoordinatorReq(method, target, coord))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != wantBody || calls != 1 {
			t.Fatalf("%s %s: code=%d body=%q calls=%d", method, target, w.Code, w.Body.String(), calls)
		}
	}
	for _, route := range agentControlCases {
		body := "{\"marker\":\"synthetic\",\"ok\":true}\n"
		if route.path == "/agent/logs" {
			body = "{\"logs\":\"line1\\nline2\"}\n"
		}
		request(route.method, route.path, body)
	}
	for _, tc := range []struct {
		query string
		want  LogQuery
	}{
		{"", LogQuery{Lines: 200}},
		{"?lines=50&level=error&grep=consume&since=5m", LogQuery{Lines: 50, Level: "error", Grep: "consume", Since: "5m"}},
		{"?lines=0", LogQuery{Lines: 200}},
		{"?lines=-1", LogQuery{Lines: 200}},
		{"?lines=invalid", LogQuery{Lines: 200}},
	} {
		request(http.MethodGet, "/agent/logs"+tc.query, "{\"logs\":\"line1\\nline2\"}\n")
		if gotQuery != tc.want {
			t.Fatalf("logs %s: query=%+v want=%+v", tc.query, gotQuery, tc.want)
		}
	}
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"", false}, {"?refresh=1", true}, {"?refresh=true", true}, {"?refresh=0", false}, {"?refresh=invalid", false},
	} {
		request(http.MethodGet, "/agent/vendor-agents"+tc.query, "{\"marker\":\"synthetic\",\"ok\":true}\n")
		if gotRefresh != tc.want {
			t.Fatalf("vendor %s: refresh=%v want=%v", tc.query, gotRefresh, tc.want)
		}
	}
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"", true}, {"?verbose=1", true}, {"?verbose=false", false}, {"?verbose=invalid", false},
	} {
		request(http.MethodPost, "/agent/set-log-level"+tc.query, "{\"marker\":\"synthetic\",\"ok\":true}\n")
		if gotVerbose != tc.want {
			t.Fatalf("log level %s: verbose=%v want=%v", tc.query, gotVerbose, tc.want)
		}
	}
}

func TestAgentControlPreservesProviderErrors(t *testing.T) {
	for _, providerErr := range []error{errors.New("synthetic provider failure"), &agentError{code: http.StatusConflict, msg: "synthetic conflict"}} {
		calls := 0
		action := func() (any, error) { calls++; return nil, providerErr }
		p := &AgentProviders{
			Logs:         func(LogQuery) (string, error) { calls++; return "", providerErr },
			VendorAgents: func(bool) (any, error) { return action() },
			Resubscribe:  action, WorkerRestart: action,
		}
		s, coord, _ := newAgentControlServer(t, p)
		for _, route := range agentControlCases {
			if route.path != "/agent/logs" && route.path != "/agent/vendor-agents" && route.path != "/agent/resubscribe" && route.path != "/agent/worker-restart" {
				continue
			}
			calls = 0
			w := httptest.NewRecorder()
			s.buildControlMux().ServeHTTP(w, agentCoordinatorReq(route.method, route.path, coord))
			want := http.StatusInternalServerError
			if ae, ok := providerErr.(*agentError); ok {
				want = ae.code
			}
			if w.Code != want || w.Body.String() != "{\"error\":\""+providerErr.Error()+"\"}\n" || calls != 1 {
				t.Fatalf("%s: code=%d body=%q calls=%d", route.path, w.Code, w.Body.String(), calls)
			}
		}
	}
}
