package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/gorilla/websocket"
)

func TestLivePolicyGatewayReload(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Config{})
	s.SetPermissions(&config.Permissions{Services: true})
	s.SetPermissionSource(func() (*config.Permissions, error) { return config.LoadPermissionsStrict(dir) })
	h := s.permissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	call := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code
	}
	for _, enabled := range []bool{true, false, true} {
		if err := config.SavePermissions(dir, &config.Permissions{Services: enabled}); err != nil {
			t.Fatal(err)
		}
		want := http.StatusForbidden
		if enabled {
			want = http.StatusNoContent
		}
		if got := call("/services/list"); got != want {
			t.Fatalf("enabled=%v: %d", enabled, got)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte("services: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if got := call("/services/list"); got != http.StatusForbidden {
		t.Fatal(got)
	}
	if got := call("/health"); got != http.StatusNoContent {
		t.Fatal(got)
	}
}

func TestLivePolicyGatewayAttachedWebSocket(t *testing.T) {
	dir := t.TempDir()
	if err := config.SavePermissions(dir, &config.Permissions{Services: true}); err != nil {
		t.Fatal(err)
	}
	upgrader := websocket.Upgrader{}
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		defer close(upstreamClosed)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{})
	s.SetPermissionSource(func() (*config.Permissions, error) { return config.LoadPermissionsStrict(dir) })
	h := httptest.NewServer(s.permissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.proxyWebSocket(w, r, target, "/", false) })))
	defer h.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/services/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := config.SavePermissions(dir, &config.Permissions{}); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("connection remained open")
	}
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream remained open")
	}
}

func TestLivePolicyGatewayCancelsAttachedRequest(t *testing.T) {
	dir := t.TempDir()
	if err := config.SavePermissions(dir, &config.Permissions{Services: true}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{})
	s.SetPermissionSource(func() (*config.Permissions, error) { return config.LoadPermissionsStrict(dir) })
	entered := make(chan struct{})
	done := make(chan struct{})
	h := s.permissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/services/stream", nil))
	}()
	<-entered
	if err := config.SavePermissions(dir, &config.Permissions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("attached request not canceled")
	}
}

func TestLivePolicyGatewaySourceFailure(t *testing.T) {
	s := NewServer(Config{})
	s.SetPermissionSource(func() (*config.Permissions, error) { return nil, errors.New("fixture") })
	h := s.permissionMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("delegated unavailable policy") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/services", nil))
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}

func TestLivePolicyGatewayPasscodeReload(t *testing.T) {
	dir := t.TempDir()
	p := &config.Permissions{Console: true}
	if err := p.SetPasscode("fixture-old"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{})
	s.SetPermissionSource(func() (*config.Permissions, error) { return config.LoadPermissionsStrict(dir) })
	h := s.permissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	call := func(pin string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/terminal", nil)
		r.Header.Set("X-Citadel-Passcode", pin)
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := call("fixture-old"); got != http.StatusNoContent {
		t.Fatal(got)
	}
	if err := p.SetPasscode("fixture-new"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if got := call("fixture-old"); got != http.StatusUnauthorized {
		t.Fatal(got)
	}
	if got := call("fixture-new"); got != http.StatusNoContent {
		t.Fatal(got)
	}
}

func TestLivePolicyCacheReload(t *testing.T) {
	dir := t.TempDir()
	p := authorizedPerms(t)
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{})
	s.SetMeshResolver(okResolver())
	s.SetPermissions(p)
	s.SetPermissionSource(func() (*config.Permissions, error) { return config.LoadPermissionsStrict(dir) })
	check := func() bool {
		return s.authorizeCacheRequest(httptest.NewRecorder(), cacheReq("GET", "/cache/index", testPasscode))
	}
	if !check() {
		t.Fatal("permitted read refused")
	}
	p.Files = false
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if check() {
		t.Fatal("read admitted after policy change")
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), []byte("files: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if check() {
		t.Fatal("read admitted with malformed policy")
	}
}
