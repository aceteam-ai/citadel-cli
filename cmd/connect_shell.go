// cmd/connect_shell.go
//
// Remote-shell client for `citadel connect <name|ip>`. This is the client half
// of the terminal WebSocket server that `citadel work` runs by default (see
// internal/terminal/server.go). It resolves the target to a mesh IP, dials the
// node's /terminal endpoint *over the tsnet mesh* (not a host socket — bare
// tsnet only forwards to ports Citadel explicitly ListenVPNs, and the terminal
// server does exactly that on :7860), negotiates a PTY, and streams stdin/stdout
// with terminal-resize propagation.
package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// runRemoteShell opens an interactive shell on the target node over the mesh.
//
// Idempotency (issue #582 / coordinate with #571): this client publishes no
// heartbeat and holds no worklock, so repeated `citadel connect <target>` never
// creates duplicate node state — each invocation is an independent view, and on
// exit the terminal is always restored and the socket closed cleanly. Stateful
// re-attach (reconnecting to the *same* live shell with its running command and
// scrollback after a dropped connection) is provided by the node-side terminal
// server when it backs sessions with a persistent per-user tmux session
// (CITADEL_TERMINAL_SESSION). With the default (bare-shell) node config each
// connection is a fresh shell. See the PR / follow-up for making tmux-backed
// sessions the default so `connect` re-attach is seamless out of the box.
func runRemoteShell(target string) error {
	// Ensure the mesh is up before resolving/dialing.
	netCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ensureNetworkConnected(netCtx); err != nil {
		return err
	}

	// Resolve name -> mesh IP (or accept a literal IP).
	ip, hostname, err := resolvePeer(target)
	if err != nil {
		suggestAvailablePeers()
		return fmt.Errorf("could not resolve '%s': %w", target, err)
	}

	// Auth token: --token flag or CITADEL_TERMINAL_TOKEN env.
	token := connectToken
	if token == "" {
		token = os.Getenv("CITADEL_TERMINAL_TOKEN")
	}
	if token == "" {
		return fmt.Errorf("no terminal token provided\n"+
			"  A terminal token authenticates the remote shell. Provide one with:\n"+
			"    --token <token>            or\n"+
			"    CITADEL_TERMINAL_TOKEN=<token> citadel connect %s\n"+
			"  (Tokens are issued by the AceTeam platform terminal feature.)", target)
	}

	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", connectTerminalPort))
	wsURL := url.URL{
		Scheme:   "ws",
		Host:     addr,
		Path:     "/terminal",
		RawQuery: url.Values{"token": {token}}.Encode(),
	}

	display := hostname
	if display == "" {
		display = ip
	}
	fmt.Fprintf(os.Stderr, "Connecting to %s (%s) over the mesh...\r\n", display, addr)

	// Dial the WebSocket *through the mesh*: NetDialContext routes the TCP
	// handshake through tsnet userspace networking so we reach 100.64.x.x peers
	// that host networking can't.
	dialer := websocket.Dialer{
		NetDialContext:   network.Dial,
		HandshakeTimeout: 20 * time.Second,
	}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer dialCancel()

	conn, resp, err := dialer.DialContext(dialCtx, wsURL.String(), nil)
	if err != nil {
		return remoteShellDialError(err, resp, display, addr)
	}
	defer conn.Close()

	fmt.Fprintf(os.Stderr, "Connected. (Ctrl-D or 'exit' to disconnect)\r\n")

	return pumpRemoteShell(conn)
}

// remoteShellDialError turns a WebSocket dial failure into an actionable message.
func remoteShellDialError(err error, resp *http.Response, display, addr string) error {
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("authentication rejected by %s (HTTP %d): the terminal token is invalid or not authorized for this node's org", display, resp.StatusCode)
		case http.StatusServiceUnavailable:
			return fmt.Errorf("%s is at capacity or its auth service is unavailable (HTTP %d)", display, resp.StatusCode)
		default:
			return fmt.Errorf("terminal handshake with %s failed (HTTP %d): %w", display, resp.StatusCode, err)
		}
	}
	// No HTTP response => transport-level failure (refused / unreachable).
	return fmt.Errorf("could not reach a terminal endpoint on %s (%s): %w\n"+
		"  The target may not be running 'citadel work' (its terminal endpoint is\n"+
		"  enabled by default), may have it disabled (--no-terminal), or may be\n"+
		"  offline / not yet reachable on the mesh.", display, addr, err)
}

// pumpRemoteShell drives the interactive session: local raw terminal <-> PTY.
func pumpRemoteShell(conn *websocket.Conn) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// All WebSocket writes must be serialized (gorilla forbids concurrent writes).
	var writeMu sync.Mutex
	send := func(msg *terminal.Message) error {
		data, err := msg.Marshal()
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, data)
	}

	// Put the local terminal into raw mode so keystrokes stream unbuffered.
	stdinFd := int(os.Stdin.Fd())
	var restore func()
	if term.IsTerminal(stdinFd) {
		oldState, err := term.MakeRaw(stdinFd)
		if err != nil {
			return fmt.Errorf("failed to set raw terminal mode: %w", err)
		}
		restore = func() { _ = term.Restore(stdinFd, oldState) }
		defer restore()
	}

	// Send the initial size and propagate resizes to the remote PTY.
	sendResize := func() {
		if cols, rows, err := term.GetSize(stdinFd); err == nil && cols > 0 && rows > 0 {
			_ = send(terminal.NewResizeMessage(uint16(cols), uint16(rows)))
		}
	}
	sendResize()
	watchResize(ctx, sendResize)

	// stdin -> PTY (input messages).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if serr := send(terminal.NewInputMessage(buf[:n])); serr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	// PTY -> stdout (output messages). This is the main loop; when the remote
	// shell exits, the server closes the socket and we return.
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			// The remote shell exiting (`exit` / Ctrl-D) tears the socket down.
			// The server does not always send a close frame first, so accept the
			// normal, going-away, no-status, and abnormal-closure codes as a clean
			// end-of-session (exit 0) rather than surfacing them as an error.
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
				websocket.CloseAbnormalClosure) {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return fmt.Errorf("connection closed: %w", err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		msg, err := terminal.UnmarshalMessage(data)
		if err != nil {
			continue
		}
		switch msg.Type {
		case terminal.MessageTypeOutput:
			if len(msg.Payload) > 0 {
				_, _ = os.Stdout.Write(msg.Payload)
			}
		case terminal.MessageTypeError:
			// Terminal is restored by the deferred restore().
			return fmt.Errorf("remote error: %s", msg.Error)
		case terminal.MessageTypePing:
			_ = send(terminal.NewPongMessage())
		}
	}
}
