// Package websockify implements a minimal, dependency-free (beyond the
// already-vendored gorilla/websocket) WebSocket-to-TCP bridge equivalent to
// noVNC's classic "websockify" helper.
//
// A browser-based noVNC client speaks the RFB (VNC) protocol over a WebSocket
// connection, sending RFB bytes as binary WebSocket frames. A raw VNC server
// (e.g. x11vnc on port 5900) speaks RFB over a plain TCP socket and knows
// nothing about WebSockets. This bridge sits in between: it terminates the
// WebSocket on one side, dials the TCP VNC server on the other, and pumps
// bytes bidirectionally:
//
//	browser <--WS binary frames--> bridge <--raw TCP--> VNC server (5900)
//
// The Citadel gateway routes "/vnc/..." to this bridge as a transparent raw
// byte pump, so the WebSocket handshake is negotiated end-to-end between the
// browser and this bridge. The bridge therefore performs a real RFC 6455
// upgrade and must emit RFB bytes as BinaryMessage frames.
package websockify

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Logger is the minimal logging surface used by the bridge. It matches the
// signature of the package-level Debug helper in cmd/root.go so callers can
// pass it directly.
type Logger func(format string, args ...interface{})

// Config configures a Bridge.
type Config struct {
	// ListenPort is the local TCP port the WebSocket server listens on
	// (the gateway's websockify upstream, conventionally 6080).
	ListenPort int

	// VNCAddress is the TCP address of the raw VNC server to forward to,
	// e.g. "127.0.0.1:5900".
	VNCAddress string

	// Logger, if non-nil, receives debug/error messages. A nil Logger
	// discards all messages.
	Logger Logger
}

// Bridge is a WebSocket-to-TCP bridge server.
type Bridge struct {
	cfg        Config
	upgrader   websocket.Upgrader
	httpServer *http.Server

	mu      sync.Mutex
	running bool
}

// NewBridge constructs a Bridge from cfg. It does not start listening until
// Start is called.
func NewBridge(cfg Config) *Bridge {
	b := &Bridge{cfg: cfg}
	b.upgrader = websocket.Upgrader{
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
		// noVNC historically negotiates the "binary" subprotocol. gorilla
		// only echoes it back when the client actually offers it, so this is
		// harmless for modern clients that omit it.
		Subprotocols: []string{"binary"},
		CheckOrigin:  checkOrigin,
	}
	return b
}

// checkOrigin allows same-origin/CLI requests (no Origin header), localhost
// origins, and aceteam.ai origins. The browser noVNC client connects with an
// Origin of the AceTeam web app, so the default gorilla same-origin policy
// would reject it.
//
// The origin is parsed as a URL and the hostname is checked with an exact
// match (or a subdomain suffix match) to prevent bypass via attacker-controlled
// domains like "aceteam.ai.evil.com".
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" {
		return true
	}
	if host == "aceteam.ai" || strings.HasSuffix(host, ".aceteam.ai") {
		return true
	}
	return false
}

func (b *Bridge) logf(format string, args ...interface{}) {
	if b.cfg.Logger != nil {
		b.cfg.Logger(format, args...)
	}
}

// Handler returns the http.Handler that upgrades requests to WebSocket and
// bridges them to the configured VNC server. It is registered for all paths
// because the gateway may strip the "/vnc" prefix, leaving the bridge to see
// either "/" or "/websockify" depending on the noVNC client URL.
func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", b.handleWebSocket)
	return mux
}

func (b *Bridge) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response on failure.
		b.logf("websockify: upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	tcp, err := net.DialTimeout("tcp", b.cfg.VNCAddress, 10*time.Second)
	if err != nil {
		b.logf("websockify: dial %s failed: %v", b.cfg.VNCAddress, err)
		_ = ws.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "vnc dial failed"),
			time.Now().Add(time.Second),
		)
		return
	}
	defer tcp.Close()

	b.logf("websockify: bridging connection to %s", b.cfg.VNCAddress)
	pump(ws, tcp, b.logf)
}

// pump copies bytes bidirectionally between a WebSocket connection and a TCP
// connection until either side closes. RFB is a binary protocol, so all
// WebSocket frames written toward the browser use BinaryMessage.
//
// gorilla forbids concurrent writers on a single connection, so each
// destination is written by exactly one goroutine: the WS->TCP goroutine is
// the sole writer to tcp, and the TCP->WS goroutine is the sole writer to ws.
func pump(ws *websocket.Conn, tcp net.Conn, logf Logger) {
	done := make(chan struct{}, 2)

	// WebSocket -> TCP: decode incoming binary frames, write raw bytes to VNC.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, reader, err := ws.NextReader()
			if err != nil {
				return
			}
			if _, err := io.Copy(tcp, reader); err != nil {
				logf("websockify: ws->tcp copy error: %v", err)
				return
			}
		}
	}()

	// TCP -> WebSocket: read raw VNC bytes, emit as binary frames.
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					logf("websockify: tcp->ws write error: %v", werr)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for one direction to finish, then unblock the other by closing
	// both underlying connections (the deferred Close calls in the caller
	// also run, but closing here promptly interrupts the blocked goroutine).
	<-done
	_ = tcp.Close()
	_ = ws.Close()
	<-done
}

// Start binds the listener and serves until ctx is cancelled. It blocks until
// the server stops. Returns nil on graceful shutdown.
func (b *Bridge) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("websockify bridge already running")
	}
	b.running = true
	b.httpServer = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", b.cfg.ListenPort),
		Handler: b.Handler(),
		// Long timeouts: VNC sessions are long-lived streaming connections.
		ReadHeaderTimeout: 15 * time.Second,
	}
	srv := b.httpServer
	b.mu.Unlock()

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("websockify: listen on %s: %w", srv.Addr, err)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	b.logf("websockify: listening on %s, forwarding to %s", srv.Addr, b.cfg.VNCAddress)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("websockify: serve: %w", err)
	}
	return nil
}
