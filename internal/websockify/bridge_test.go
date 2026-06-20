package websockify

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startEchoTCPServer starts a TCP server that echoes everything it receives
// back to the sender. Returns the listen address and a cleanup function.
func startEchoTCPServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		ln.Close()
	}
}

// newBridgeTestServer wires a Bridge.Handler into an httptest.Server pointed at
// the given VNC TCP address.
func newBridgeTestServer(t *testing.T, vncAddr string) (*httptest.Server, *Bridge) {
	t.Helper()
	b := NewBridge(Config{VNCAddress: vncAddr})
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	return srv, b
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestBridgeRoundTripBinary(t *testing.T) {
	vncAddr, cleanup := startEchoTCPServer(t)
	defer cleanup()

	srv, _ := newBridgeTestServer(t, vncAddr)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// A realistic RFB-ish binary payload including a NUL byte to ensure binary
	// (not text) framing is used end-to-end.
	payload := []byte{0x52, 0x46, 0x42, 0x00, 0x03, 0x00, 0x08, 0xFF}
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	msgType, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != websocket.BinaryMessage {
		t.Errorf("expected BinaryMessage (%d), got %d", websocket.BinaryMessage, msgType)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round-trip mismatch: sent %v, got %v", payload, got)
	}
}

func TestBridgeMultipleFrames(t *testing.T) {
	vncAddr, cleanup := startEchoTCPServer(t)
	defer cleanup()

	srv, _ := newBridgeTestServer(t, vncAddr)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	frames := [][]byte{
		[]byte("first"),
		{0x00, 0x01, 0x02, 0x03},
		[]byte("the quick brown fox"),
	}

	var want []byte
	for _, f := range frames {
		if err := conn.WriteMessage(websocket.BinaryMessage, f); err != nil {
			t.Fatalf("write: %v", err)
		}
		want = append(want, f...)
	}

	// The echo server returns a raw byte stream; frame boundaries on the
	// TCP->WS side are not preserved, so accumulate until we have all bytes.
	var got []byte
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for len(got) < len(want) {
		_, chunk, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, chunk...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("stream mismatch: want %v, got %v", want, got)
	}
}

func TestBridgeUpgradeOnRootPath(t *testing.T) {
	vncAddr, cleanup := startEchoTCPServer(t)
	defer cleanup()

	srv, _ := newBridgeTestServer(t, vncAddr)

	// The gateway strips the "/vnc" prefix, so the bridge must accept the
	// upgrade on "/" (not only "/websockify").
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL)+"/", nil)
	if err != nil {
		t.Fatalf("ws dial on root path: %v (status: %v)", err, resp)
	}
	conn.Close()
}

func TestBridgeDialFailureClosesCleanly(t *testing.T) {
	// Point at a port nothing is listening on so the TCP dial fails.
	b := NewBridge(Config{VNCAddress: "127.0.0.1:1"})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// The bridge should close the connection rather than hang or crash.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected connection to close after dial failure")
	}
}

func TestBridgeStartAndShutdown(t *testing.T) {
	vncAddr, cleanup := startEchoTCPServer(t)
	defer cleanup()

	// Bind to an ephemeral port by listening first to discover a free one.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	b := NewBridge(Config{ListenPort: port, VNCAddress: vncAddr})
	listenAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	// Wait for the listener to come up.
	deadline := time.Now().Add(2 * time.Second)
	var dialErr error
	for time.Now().Before(deadline) {
		c, e := net.Dial("tcp", listenAddr)
		if e == nil {
			c.Close()
			dialErr = nil
			break
		}
		dialErr = e
		time.Sleep(20 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("bridge never started listening: %v", dialErr)
	}

	// Verify it actually serves a websocket upgrade.
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+listenAddr+"/", nil)
	if err != nil {
		t.Fatalf("ws dial against running bridge: %v", err)
	}
	conn.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("Start returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not shut down after ctx cancel")
	}
}

func TestCheckOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		// Allowed: no origin (same-origin / CLI)
		{"", true},
		// Allowed: localhost variants
		{"http://localhost:3000", true},
		{"http://127.0.0.1:8443", true},
		// Allowed: exact aceteam.ai and subdomains
		{"https://aceteam.ai", true},
		{"https://app.aceteam.ai", true},
		{"https://staging.app.aceteam.ai", true},
		// Rejected: unrelated domains
		{"https://evil.example.com", false},
		// Rejected: bypass attempts — attacker-controlled domains
		{"https://aceteam.ai.evil.com", false},
		{"https://evil-aceteam.ai", false},
		{"https://notaceteam.ai", false},
		{"https://localhost.evil.com", false},
		{"https://fake127.0.0.1.evil.com", false},
		// Rejected: malformed origins
		{"not-a-url", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := checkOrigin(r); got != tc.want {
			t.Errorf("checkOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}
