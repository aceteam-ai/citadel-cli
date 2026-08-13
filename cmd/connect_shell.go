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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// runRemoteShell opens an interactive shell on the target node over the mesh.
//
// Auth (citadel #585): no --token is required. The node authorizes a connection
// arriving over its VPN listener by our verified mesh-peer identity, so dialing
// <vpn_ip>:7860 over the mesh is sufficient. A token is still accepted for the
// platform terminal path or when the target disables mesh trust.
//
// Idempotency (issue #582 / coordinate with #571): this client publishes no
// heartbeat and holds no worklock, so repeated `citadel connect <target>` never
// creates duplicate node state — each invocation is an independent view, and on
// exit the terminal is always restored and the socket closed cleanly. Stateful
// re-attach (reconnecting to the *same* live shell with its running command and
// scrollback after a dropped connection) is now the default: the node-side
// terminal server backs sessions with a persistent per-user tmux session by
// default (citadel #585, DefaultSessionName="citadel"), so a repeated connect —
// or a reconnect after a drop — re-attaches to the same live shell. Operators
// can force a bare shell with CITADEL_TERMINAL_SESSION=none.
func runRemoteShell(target, passcode string) error {
	// Ensure the mesh is up before resolving/dialing.
	netCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ensureNetworkConnectedFn(netCtx); err != nil {
		return err
	}

	// Resolve name -> mesh IP (or accept a literal IP).
	ip, hostname, err := resolvePeer(target)
	if err != nil {
		suggestAvailablePeers()
		return fmt.Errorf("could not resolve '%s': %w", target, err)
	}

	// Auth token: OPTIONAL as of citadel #585. When we omit it, the target node
	// authorizes this connection by our verified mesh-peer identity — dialing its
	// <vpn_ip>:7860 over the mesh already proves we are an authenticated member
	// of the org tailnet (auth happened at the WireGuard layer). A token is still
	// accepted (and takes precedence node-side) for the platform terminal path or
	// when mesh trust is disabled on the target (CITADEL_TERMINAL_TRUST_MESH=false).
	token := connectToken
	if token == "" {
		token = os.Getenv("CITADEL_TERMINAL_TOKEN")
	}

	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", connectTerminalPort))
	wsURL := terminalWSURL(addr, token, passcode)

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

// terminalWSURL builds the terminal endpoint's WebSocket upgrade URL for
// addr (host:port), forwarding token and/or passcode as query parameters the
// same way the server reads them (internal/terminal/server.go:
// r.URL.Query().Get("token") / .Get("passcode")). Each is set ONLY when
// non-empty, so a node with neither configured sees the exact same request
// it always has, and a node passcode (citadel#753) is never appended when
// none was supplied. Pure and side-effect-free so the exact query string
// (notably, the "passcode" key's presence) can be pinned by a test.
func terminalWSURL(addr, token, passcode string) url.URL {
	query := url.Values{}
	if token != "" {
		query.Set("token", token)
	}
	if passcode != "" {
		query.Set("passcode", passcode)
	}
	return url.URL{
		Scheme:   "ws",
		Host:     addr,
		Path:     "/terminal",
		RawQuery: query.Encode(),
	}
}

// terminalDialErrKind classifies why the ts-net terminal-endpoint dial failed
// (#754), so connectToNode knows whether to prompt for a passcode and retry,
// fall back to the legacy raw-sshd path, or just report the error as-is.
type terminalDialErrKind int

const (
	// terminalDialErrOther covers every dial failure that is neither a
	// passcode rejection nor a bare connection-refused: forbidden/not-found
	// auth rejections, 503, malformed handshake, etc. Never a fallback
	// trigger and never a passcode-retry trigger, surfaced as-is.
	terminalDialErrOther terminalDialErrKind = iota
	// terminalDialErrUnreachable means the ts-net terminal endpoint refused
	// the TCP connection outright (not listening / 'citadel work' not
	// running / --no-terminal). Safe to fall back to raw sshd for.
	terminalDialErrUnreachable
	// terminalDialErrPasscode means the endpoint IS reachable but rejected
	// the connection with the terminal passcode gate (HTTP 401, node
	// passcode required). NOT a fallback trigger: the caller should prompt
	// for a passcode and retry ts-net.
	terminalDialErrPasscode
)

// terminalDialError wraps a ts-net terminal-endpoint dial failure with a
// classification connectToNode uses to route between prompting for a
// passcode, falling back to raw sshd, and reporting the error directly.
type terminalDialError struct {
	kind terminalDialErrKind
	err  error
}

func (e *terminalDialError) Error() string { return e.err.Error() }
func (e *terminalDialError) Unwrap() error { return e.err }

// terminalErrorBody mirrors the JSON error shape internal/terminal/server.go
// writes ({"error":...,"status":...}, plus an optional "reason" field added
// by citadel#753) closely enough to detect the passcode gate, without
// importing that package: the wire contract, not the Go type, is what a
// client should couple to (same rationale as isConnRefused's string match).
type terminalErrorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// isPasscodeRequiredResponse reports whether a 401 from the terminal
// endpoint is specifically the passcode gate (node passcode required)
// rather than a rejected/invalid auth token: both return HTTP 401
// (internal/terminal/server.go resolveAuth vs. the PasscodeVerifier check),
// so the response body is the only thing that tells them apart. A
// malformed/unreadable body is treated as NOT passcode-required (falls
// through to the existing generic auth-rejected message) rather than
// guessing. The "reason" field (citadel#753) is read opportunistically when
// present but never required; the "error" text is the stable contract.
func isPasscodeRequiredResponse(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false
	}
	var body terminalErrorBody
	if err := json.Unmarshal(data, &body); err != nil {
		return false
	}
	if strings.HasPrefix(body.Reason, "passcode_") {
		return true
	}
	return body.Error == "node passcode required"
}

// remoteShellDialError turns a WebSocket dial failure into an actionable message.
func remoteShellDialError(err error, resp *http.Response, display, addr string) error {
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			if isPasscodeRequiredResponse(resp) {
				return &terminalDialError{
					kind: terminalDialErrPasscode,
					err:  fmt.Errorf("%s requires a node passcode", display),
				}
			}
			return fmt.Errorf("authentication rejected by %s (HTTP %d): mesh-peer identity was not accepted (target may have mesh trust disabled, or we are not a same-owner tailnet peer) and no valid --token was supplied", display, resp.StatusCode)
		case http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("authentication rejected by %s (HTTP %d): mesh-peer identity was not accepted (target may have mesh trust disabled, or we are not a same-owner tailnet peer) and no valid --token was supplied", display, resp.StatusCode)
		case http.StatusServiceUnavailable:
			return fmt.Errorf("%s is at capacity or its auth service is unavailable (HTTP %d)", display, resp.StatusCode)
		default:
			return fmt.Errorf("terminal handshake with %s failed (HTTP %d): %w", display, resp.StatusCode, err)
		}
	}
	// No HTTP response => transport-level failure (refused / unreachable).
	if isConnRefused(err) {
		// Reuses the isConnRefused pattern from cmd/mesh.go: a refused dial
		// means the terminal endpoint isn't listening at all (e.g.
		// 'citadel work' isn't running, or --no-terminal), which IS safe to
		// fall back to raw sshd for (#754), unlike the passcode case above.
		return &terminalDialError{
			kind: terminalDialErrUnreachable,
			err: fmt.Errorf("could not reach a terminal endpoint on %s (%s): %w\n"+
				"  The target may not be running 'citadel work' (its terminal endpoint is\n"+
				"  enabled by default), may have it disabled (--no-terminal), or may be\n"+
				"  offline / not yet reachable on the mesh.", display, addr, err),
		}
	}
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
