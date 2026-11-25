package cobrowsestream

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startLocalServer runs the real Server.Start localhost listener.
func startLocalServer(t *testing.T) (*Server, string) {
	t.Helper()
	f := newFakeChrome()
	withFakeCDP(t, f)
	srv := NewServer(Config{Port: 0}, &fakeSession{port: 9222})
	srv.SetSilent()
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(f.close)
	t.Cleanup(srv.Stop)
	return srv, srv.BoundAddr()
}

func dialAddr(addr string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.Dial("ws://"+addr+StreamPath+"?id=cb-x", hdr)
}

func wantRefused(t *testing.T, conn *websocket.Conn, resp *http.Response, err error) {
	t.Helper()
	if conn != nil {
		conn.Close()
		t.Fatal("upgrade succeeded, want 403")
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got err=%v resp=%v", err, resp)
	}
}

func wantUpgraded(t *testing.T, conn *websocket.Conn, resp *http.Response, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("upgrade refused: %v (resp=%v)", err, resp)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage() // unattachable session: server closes
}

func TestLocalhostRefusesOrigin(t *testing.T) {
	_, addr := startLocalServer(t)
	for _, o := range []string{"https://example.com", "null", "http://" + addr} {
		conn, resp, err := dialAddr(addr, http.Header{"Origin": {o}})
		wantRefused(t, conn, resp, err)
	}
}

func TestLocalhostRefusesForeignHost(t *testing.T) {
	_, addr := startLocalServer(t)
	_, port, _ := net.SplitHostPort(addr)
	for _, h := range []string{"evil.example", "evil.example:" + port, "127.0.0.1", "127.0.0.1:1"} {
		conn, resp, err := dialAddr(addr, http.Header{"Host": {h}})
		wantRefused(t, conn, resp, err)
	}
}

func TestLocalhostAllowsNoOriginLoopbackHost(t *testing.T) {
	_, addr := startLocalServer(t)
	conn, resp, err := dialAddr(addr, nil)
	wantUpgraded(t, conn, resp, err)

	_, port, _ := net.SplitHostPort(addr)
	conn, resp, err = dialAddr(addr, http.Header{"Host": {"localhost:" + port}})
	wantUpgraded(t, conn, resp, err)
}

func TestMeshListenerUnchanged(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)
	srv := NewServer(Config{}, &fakeSession{port: 9222})
	srv.SetSilent()
	// Simulate a connection that arrived on a non-loopback (mesh) listener.
	mesh := &net.TCPAddr{IP: net.ParseIP("100.64.0.5"), Port: 5911}
	mux := http.NewServeMux()
	guarded := srv.guardLocalhost(srv.handleStream)
	mux.HandleFunc(StreamPath, func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(mesh))
		guarded(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Origin and a non-loopback Host are both still accepted there.
	conn, resp, err := dialAddr(strings.TrimPrefix(ts.URL, "http://"),
		http.Header{"Origin": {"https://example.com"}, "Host": {"100.64.0.5:5911"}})
	wantUpgraded(t, conn, resp, err)
	deadline := time.Now().Add(5 * time.Second)
	for srv.ActiveConnections() != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}
