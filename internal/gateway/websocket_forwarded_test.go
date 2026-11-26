package gateway

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The WebSocket path must overwrite a client-supplied X-Forwarded-For with the
// real client address, like the HTTP proxy path does.
func TestProxyWebSocketOverwritesForwardedFor(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer up.Close()
	got := make(chan string, 1)
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(strings.ToLower(line), "x-forwarded-for:") {
				got <- strings.TrimSpace(line[len("x-forwarded-for:"):])
				return
			}
			if line == "\r\n" {
				got <- ""
				return
			}
		}
	}()

	s := NewServer(Config{Port: 0})
	target := &url.URL{Scheme: "http", Host: up.Addr().String()}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.proxyWebSocket(w, r, target, "/", false)
	}))
	defer front.Close()

	c, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := "GET /ws HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nX-Forwarded-For: 203.0.113.9\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v == "" || strings.Contains(v, "203.0.113.9") || !strings.HasPrefix(v, "127.0.0.1:") {
			t.Fatalf("upstream saw X-Forwarded-For %q, want the real client address", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream saw no request")
	}
}
