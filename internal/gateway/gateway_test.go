package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

func TestNewServer_Defaults(t *testing.T) {
	s := NewServer(Config{})
	if s.config.Port != 8443 {
		t.Errorf("default port = %d, want 8443", s.config.Port)
	}
	if s.config.Upstreams == nil {
		t.Error("upstreams map should be initialized")
	}
}

func TestNewServer_CustomPort(t *testing.T) {
	s := NewServer(Config{Port: 9443})
	if s.config.Port != 9443 {
		t.Errorf("port = %d, want 9443", s.config.Port)
	}
}

func TestHandleRoot_Exact(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.handleRoot(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["gateway"] != "citadel" {
		t.Errorf("gateway = %v, want citadel", resp["gateway"])
	}
	if resp["node"] != "test-node" {
		t.Errorf("node = %v, want test-node", resp["node"])
	}
}

func TestHandleRoot_NotFound(t *testing.T) {
	s := NewServer(Config{NodeName: "test-node"})

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	s.handleRoot(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAddUpstream(t *testing.T) {
	s := NewServer(Config{})
	s.AddUpstream("/health", &Upstream{Address: "127.0.0.1:8080"})
	s.AddUpstream("/api", &Upstream{Address: "127.0.0.1:8443", StripPrefix: true})

	if len(s.config.Upstreams) != 2 {
		t.Errorf("upstream count = %d, want 2", len(s.config.Upstreams))
	}

	if u := s.config.Upstreams["/health"]; u.Address != "127.0.0.1:8080" {
		t.Errorf("health upstream = %s, want 127.0.0.1:8080", u.Address)
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		connection string
		upgrade    string
		want       bool
	}{
		{"valid ws", "upgrade", "websocket", true},
		{"valid ws mixed case", "Upgrade", "WebSocket", true},
		{"no upgrade header", "", "websocket", false},
		{"no websocket", "upgrade", "", false},
		{"normal request", "", "", false},
		{"wrong upgrade type", "upgrade", "h2c", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.connection != "" {
				req.Header.Set("Connection", tt.connection)
			}
			if tt.upgrade != "" {
				req.Header.Set("Upgrade", tt.upgrade)
			}

			got := isWebSocketUpgrade(req)
			if got != tt.want {
				t.Errorf("isWebSocketUpgrade() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProxyRouting(t *testing.T) {
	// Two separate backends to verify route discrimination
	statusBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"backend":     "status",
			"path":        r.URL.Path,
			"x_node":      r.Header.Get("X-Citadel-Node"),
			"x_fwd_proto": r.Header.Get("X-Forwarded-Proto"),
		})
	}))
	defer statusBackend.Close()

	vncBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"backend": "vnc",
			"path":    r.URL.Path,
		})
	}))
	defer vncBackend.Close()

	statusAddr := statusBackend.URL[len("http://"):]
	vncAddr := vncBackend.URL[len("http://"):]

	// Create gateway with upstreams on separate backends
	gw := NewServer(Config{
		Port:     0,
		NodeName: "test-node",
	})
	gw.AddUpstream("/health", &Upstream{Address: statusAddr})
	gw.AddUpstream("/api/screenshot", &Upstream{Address: statusAddr})
	gw.AddUpstream("/api/actions", &Upstream{Address: statusAddr})
	gw.AddUpstream("/vnc", &Upstream{Address: vncAddr, StripPrefix: true, WebSocket: true})

	// Build routes
	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}
	gw.mux.HandleFunc("/", gw.handleRoot)

	// Test health goes to status backend
	t.Run("health -> status backend", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]string
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["backend"] != "status" {
			t.Errorf("routed to %q backend, want status", resp["backend"])
		}
		if resp["path"] != "/health" {
			t.Errorf("proxied path = %q, want /health", resp["path"])
		}
		if resp["x_node"] != "test-node" {
			t.Errorf("X-Citadel-Node = %q, want test-node", resp["x_node"])
		}
		if resp["x_fwd_proto"] != "https" {
			t.Errorf("X-Forwarded-Proto = %q, want https", resp["x_fwd_proto"])
		}
	})

	// Test /api/screenshot goes to status backend
	t.Run("api/screenshot -> status backend", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]string
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["backend"] != "status" {
			t.Errorf("routed to %q backend, want status", resp["backend"])
		}
	})

	// Test /vnc goes to vnc backend with strip prefix
	t.Run("vnc -> vnc backend with strip", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/vnc/info", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]string
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["backend"] != "vnc" {
			t.Errorf("routed to %q backend, want vnc", resp["backend"])
		}
		if resp["path"] != "/info" {
			t.Errorf("proxied path = %q, want /info (strip prefix)", resp["path"])
		}
	})

	// Test root returns gateway info (not proxied)
	t.Run("root info", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		gw.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}

		var resp map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["gateway"] != "citadel" {
			t.Errorf("gateway = %v, want citadel", resp["gateway"])
		}
	})
}

func TestProxyUpstreamDown(t *testing.T) {
	// Use a port that nothing is listening on
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := listener.Addr().String()
	listener.Close() // Close it so it's unreachable

	gw := NewServer(Config{NodeName: "test-node"})
	gw.AddUpstream("/dead", &Upstream{Address: deadAddr})

	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}

	req := httptest.NewRequest(http.MethodGet, "/dead", nil)
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestStartAndShutdownDualListen(t *testing.T) {
	// Find a free port for the primary listener
	primaryLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := primaryLn.Addr().(*net.TCPAddr).Port
	primaryLn.Close()

	// Create a second listener to simulate a VPN listener
	extraLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	extraPort := extraLn.Addr().(*net.TCPAddr).Port

	gw := NewServer(Config{
		Port:          port,
		ListenAddress: fmt.Sprintf("127.0.0.1:%d", port),
		NodeName:      "test-dual-node",
	})
	gw.AddListener(extraLn)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- gw.Start(ctx)
	}()

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	// Primary listener should serve /
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("primary GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("primary listener: status = %d, want 200", resp.StatusCode)
	}

	// Extra (VPN) listener should also serve /
	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/", extraPort))
	if err != nil {
		t.Fatalf("extra GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("extra listener: status = %d, want 200, body: %s", resp.StatusCode, string(body))
	}

	// Verify the extra listener returns the gateway info
	var gwResp map[string]interface{}
	if err := json.Unmarshal(body, &gwResp); err != nil {
		t.Fatalf("unmarshal extra listener response: %v", err)
	}
	if gwResp["gateway"] != "citadel" {
		t.Errorf("extra listener gateway = %v, want citadel", gwResp["gateway"])
	}
	if gwResp["node"] != "test-dual-node" {
		t.Errorf("extra listener node = %v, want test-dual-node", gwResp["node"])
	}

	// Shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after context cancel")
	}
}

func TestStartAndShutdown(t *testing.T) {
	// Find a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	gw := NewServer(Config{
		Port:          port,
		ListenAddress: fmt.Sprintf("127.0.0.1:%d", port),
		NodeName:      "test-node",
		// No TLS — plain HTTP for testing
	})

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- gw.Start(ctx)
	}()

	// Give it time to start
	time.Sleep(100 * time.Millisecond)

	// Make a request
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200, body: %s", resp.StatusCode, string(body))
	}

	// Shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start() did not return after context cancel")
	}
}

func TestCategoryForPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/terminal", "console"},
		{"/terminal/ws", "console"},
		{"/vnc", "desktop"},
		{"/vnc/websockify", "desktop"},
		{"/api/screenshot", "desktop"},
		{"/api/screenshot/", "desktop"},
		{"/api/actions", "desktop"},
		{"/api/actions/click", "desktop"},
		{"/services", "services"},
		{"/services/", "services"},
		{"/ssh", "ssh"},
		{"/ssh/authorized-keys", "ssh"},
		// Always-allowed paths
		{"/health", ""},
		{"/status", ""},
		{"/ping", ""},
		{"/", ""},
		{"/unknown", ""},
		{"/api/other", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := categoryForPath(tt.path)
			if got != tt.want {
				t.Errorf("categoryForPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestPermissionMiddleware_BlocksDisabledRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"backend": "ok", "path": r.URL.Path})
	}))
	defer backend.Close()
	backendAddr := backend.URL[len("http://"):]

	gw := NewServer(Config{NodeName: "test-node"})
	gw.AddUpstream("/terminal", &Upstream{Address: backendAddr, WebSocket: true})
	gw.AddUpstream("/vnc", &Upstream{Address: backendAddr, StripPrefix: true, WebSocket: true})
	gw.AddUpstream("/api/screenshot", &Upstream{Address: backendAddr})
	gw.AddUpstream("/services", &Upstream{Address: backendAddr})
	gw.AddUpstream("/ssh/authorized-keys", &Upstream{Address: backendAddr})
	gw.AddUpstream("/health", &Upstream{Address: backendAddr})

	// Build routes
	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}
	gw.mux.HandleFunc("/", gw.handleRoot)

	// Set permissions: disable console and desktop
	perms := &config.Permissions{
		Console:  false,
		Desktop:  false,
		Files:    true,
		Services: true,
		SSH:      true,
	}
	gw.SetPermissions(perms)

	handler := gw.BuildHandler()

	// Test blocked routes return 403
	blockedPaths := []string{"/terminal", "/terminal/ws", "/vnc", "/vnc/info", "/api/screenshot"}
	for _, path := range blockedPaths {
		t.Run("blocked:"+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403", path, w.Code)
			}

			var resp map[string]string
			json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["error"] != "capability disabled by node operator" {
				t.Errorf("%s: unexpected error body: %s", path, w.Body.String())
			}
		})
	}

	// Test allowed routes pass through
	allowedPaths := []string{"/health", "/services", "/ssh/authorized-keys"}
	for _, path := range allowedPaths {
		t.Run("allowed:"+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code == http.StatusForbidden {
				t.Errorf("%s: should not be blocked (status = %d)", path, w.Code)
			}
		})
	}
}

func TestPermissionMiddleware_NilPermissionsAllowsAll(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	backendAddr := backend.URL[len("http://"):]

	gw := NewServer(Config{NodeName: "test-node"})
	gw.AddUpstream("/terminal", &Upstream{Address: backendAddr, WebSocket: true})

	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}
	gw.mux.HandleFunc("/", gw.handleRoot)

	// No permissions set (nil) -- all should pass
	handler := gw.BuildHandler()

	req := httptest.NewRequest(http.MethodGet, "/terminal", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Error("nil permissions should not block any route")
	}
}

// TestSetUpstreamAddress_DynamicRoute verifies a route registered up front with
// an empty upstream returns 502 until its address is set, then proxies to the
// backend once SetUpstreamAddress wires it. This is the mechanism that exposes a
// dynamically-provisioned service (the WhatsApp bridge) on the mesh gateway
// (aceteam-ai/citadel-cli#447).
func TestSetUpstreamAddress_DynamicRoute(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"backend": "bridge", "path": r.URL.Path})
	}))
	defer backend.Close()
	backendAddr := backend.URL[len("http://"):]

	gw := NewServer(Config{NodeName: "test-node"})
	// Registered up front with NO address (dynamic upstream), StripPrefix like a
	// real module route under /modules/<prefix>.
	routePrefix := ModuleRoutePath("whatsapp")
	gw.AddUpstream(routePrefix, &Upstream{StripPrefix: true})
	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}
	gw.mux.HandleFunc("/", gw.handleRoot)
	handler := gw.BuildHandler()

	// Before wiring: a 502 (upstream unset), NOT a panic or a 200.
	req := httptest.NewRequest(http.MethodGet, routePrefix+"/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("unwired route status = %d, want 502", w.Code)
	}

	// Wire the route to the backend.
	if err := gw.SetUpstreamAddress(routePrefix, backendAddr); err != nil {
		t.Fatalf("SetUpstreamAddress: %v", err)
	}

	// After wiring: proxies through, prefix stripped (module sees /health).
	req = httptest.NewRequest(http.MethodGet, routePrefix+"/health", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("wired route status = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["backend"] != "bridge" {
		t.Errorf("backend = %q, want bridge", got["backend"])
	}
	if got["path"] != "/health" {
		t.Errorf("forwarded path = %q, want /health (prefix stripped)", got["path"])
	}
}

// TestSetUpstreamAddress_UnknownPrefix verifies setting an address on an
// unregistered prefix errors rather than silently no-oping.
func TestSetUpstreamAddress_UnknownPrefix(t *testing.T) {
	gw := NewServer(Config{})
	if err := gw.SetUpstreamAddress("/nope", "127.0.0.1:9999"); err == nil {
		t.Fatal("expected an error for an unregistered prefix, got nil")
	}
}

// stubResolver is a test CapabilityResolver: it maps declared prefixes to
// capabilities and reports not-found for the rest.
type stubResolver map[string]string

func (s stubResolver) CapabilityForPrefix(prefix string) (string, bool) {
	c, ok := s[prefix]
	return c, ok
}

// TestModulePrefixFromPath covers extracting the module prefix from a route path.
func TestModulePrefixFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/modules/whatsapp", "whatsapp"},
		{"/modules/whatsapp/health", "whatsapp"},
		{"/modules/whatsapp/admin/tenants", "whatsapp"},
		{"/modules/my-mod/x", "my-mod"},
		{"/modules/", ""},
		{"/modules", ""},
		{"/terminal", ""},
		{"/health", ""},
	}
	for _, tt := range tests {
		if got := modulePrefixFromPath(tt.path); got != tt.want {
			t.Errorf("modulePrefixFromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestCategoryForRequest_ModuleRoute verifies a /modules/<prefix>/ request maps
// to the capability the resolver (registry) records, defaults to provision for
// an unknown prefix or nil resolver, and leaves builtin routes untouched.
func TestCategoryForRequest_ModuleRoute(t *testing.T) {
	resolver := stubResolver{"whatsapp": "provision", "readmod": "services"}

	// Registered module -> its declared capability.
	for _, p := range []string{"/modules/whatsapp", "/modules/whatsapp/health", "/modules/whatsapp/admin/tenants"} {
		if got := categoryForRequest(p, resolver); got != "provision" {
			t.Errorf("categoryForRequest(%q) = %q, want provision", p, got)
		}
	}
	// A module that declares a DIFFERENT capability (data-plane decoupled from
	// provision) is gated by that one.
	if got := categoryForRequest("/modules/readmod/health", resolver); got != "services" {
		t.Errorf("categoryForRequest(readmod) = %q, want services", got)
	}
	// Unknown prefix -> fail closed to provision.
	if got := categoryForRequest("/modules/unknown/x", resolver); got != "provision" {
		t.Errorf("categoryForRequest(unknown) = %q, want provision (fail closed)", got)
	}
	// Nil resolver -> module routes still gated (provision), builtin unchanged.
	if got := categoryForRequest("/modules/whatsapp/health", nil); got != "provision" {
		t.Errorf("categoryForRequest(nil resolver) = %q, want provision", got)
	}
	if got := categoryForRequest("/terminal", resolver); got != "console" {
		t.Errorf("categoryForRequest(/terminal) = %q, want console", got)
	}
	if got := categoryForRequest("/health", resolver); got != "" {
		t.Errorf("categoryForRequest(/health) = %q, want \"\"", got)
	}
}

// TestPermissionMiddleware_ModuleCapabilityDecoupled verifies a module can
// declare a capability OTHER than provision, so revoking provision does not kill
// its route (landmine d). The read module is gated by services here.
func TestPermissionMiddleware_ModuleCapabilityDecoupled(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	backendAddr := backend.URL[len("http://"):]

	gw := NewServer(Config{NodeName: "test-node"})
	waPrefix := ModuleRoutePath("whatsapp")
	svcPrefix := ModuleRoutePath("readmod")
	gw.AddUpstream(waPrefix, &Upstream{Address: backendAddr, StripPrefix: true})
	gw.AddUpstream(svcPrefix, &Upstream{Address: backendAddr, StripPrefix: true})
	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}
	gw.mux.HandleFunc("/", gw.handleRoot)

	gw.SetProvisionedRegistry(stubResolver{"whatsapp": "provision", "readmod": "services"})
	// Provision OFF, services ON: the provision-gated module is blocked, the
	// services-gated module still serves.
	gw.SetPermissions(&config.Permissions{Provision: false, Services: true})
	handler := gw.BuildHandler()

	req := httptest.NewRequest(http.MethodGet, waPrefix+"/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("provision-gated module with provision off: status = %d, want 403", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, svcPrefix+"/health", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Errorf("services-gated module with services on: status = %d, want not-403", w.Code)
	}
}
