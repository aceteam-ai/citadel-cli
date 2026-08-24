// internal/terminal/passcode_subprotocol_test.go
//
// Covers citadel#757: a browser WebSocket client cannot set arbitrary
// request headers on the upgrade, so it cannot use the CLI's
// X-Citadel-Passcode header path. This proves the Sec-WebSocket-Protocol
// subprotocol convention (terminalPasscodeSubprotocol) works end-to-end: a
// real gorilla/websocket client dials with
// Subprotocols: ["citadel.passcode", base64url(passcode)], the server
// accepts the passcode from it, and the 101 response echoes back the
// "citadel.passcode" marker (required by RFC 6455 §4.1 — a browser fails the
// handshake client-side if the server doesn't confirm an offered
// subprotocol). It also proves the header and query-string (deprecated)
// paths still work, and the precedence order among all three.
package terminal

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startSubprotocolTestServer builds and starts a terminal server with a
// token validator and a PasscodeVerifier that only accepts "correct-pin".
func startSubprotocolTestServer(t *testing.T, port int) (int, func()) {
	t.Helper()
	auth := NewMockTokenValidator()
	auth.AddValidToken("tok-1", &TokenInfo{UserID: "alice", OrgID: "org-1"})

	cfg := &Config{
		Host:           "127.0.0.1",
		Port:           port,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		RateLimitRPS:   1000,
		RateLimitBurst: 1000,
		PasscodeVerifier: func(pin string) bool {
			return pin == "correct-pin"
		},
	}

	s := NewServer(cfg, auth)
	s.SetSilent()
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return port, func() { _ = s.Stop(context.Background()) }
}

// TestPasscodeSubprotocol_Accepted proves a passcode presented via the
// [terminalPasscodeSubprotocol, base64url(passcode)] subprotocol list is
// accepted, AND that the 101 response echoes back exactly the
// "citadel.passcode" marker (never the encoded passcode itself), which is
// what a real browser's WebSocket handshake requires to not fail client-side.
func TestPasscodeSubprotocol_Accepted(t *testing.T) {
	port, stop := startSubprotocolTestServer(t, 17910)
	defer stop()

	encoded := base64.RawURLEncoding.EncodeToString([]byte("correct-pin"))
	dialer := websocket.Dialer{
		Subprotocols: []string{terminalPasscodeSubprotocol, encoded},
	}
	url := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok-1", port)
	conn, resp, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v (status=%v)", err, statusOf(resp))
	}
	defer conn.Close()
	defer resp.Body.Close()

	if got := conn.Subprotocol(); got != terminalPasscodeSubprotocol {
		t.Errorf("negotiated subprotocol = %q, want %q", got, terminalPasscodeSubprotocol)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != terminalPasscodeSubprotocol {
		t.Errorf("response Sec-WebSocket-Protocol = %q, want %q", got, terminalPasscodeSubprotocol)
	}
}

// TestPasscodeSubprotocol_WrongPasscodeRejected proves a wrong passcode
// carried by the subprotocol convention is still rejected (fails closed),
// and that the handshake failure surfaces as a non-101 HTTP status rather
// than silently downgrading to no auth.
func TestPasscodeSubprotocol_WrongPasscodeRejected(t *testing.T) {
	port, stop := startSubprotocolTestServer(t, 17911)
	defer stop()

	encoded := base64.RawURLEncoding.EncodeToString([]byte("wrong-pin"))
	dialer := websocket.Dialer{
		Subprotocols: []string{terminalPasscodeSubprotocol, encoded},
	}
	url := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok-1", port)
	conn, resp, err := dialer.Dial(url, nil)
	if err == nil {
		conn.Close()
		t.Fatalf("dial succeeded with wrong passcode, want rejection")
	}
	if resp == nil {
		t.Fatalf("expected an HTTP response on rejection, got nil")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestPasscodeHeader_StillWorks proves the pre-existing X-Citadel-Passcode
// header path (the CLI's, which the browser cannot use) still works
// unchanged.
func TestPasscodeHeader_StillWorks(t *testing.T) {
	port, stop := startSubprotocolTestServer(t, 17912)
	defer stop()

	header := http.Header{}
	header.Set("X-Citadel-Passcode", "correct-pin")
	url := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok-1", port)
	conn, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial: %v (status=%v)", err, statusOf(resp))
	}
	defer conn.Close()
	defer resp.Body.Close()
}

// TestPasscodeQuery_StillWorksDeprecated proves the deprecated ?passcode=
// query-string path still works during rollout (removal is a later
// release), since some in-flight web-console builds may predate the
// subprotocol convention.
func TestPasscodeQuery_StillWorksDeprecated(t *testing.T) {
	port, stop := startSubprotocolTestServer(t, 17913)
	defer stop()

	url := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok-1&passcode=correct-pin", port)
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v (status=%v)", err, statusOf(resp))
	}
	defer conn.Close()
	defer resp.Body.Close()
}

// TestPasscodePrecedence_HeaderBeatsSubprotocolBeatsQuery proves the
// documented precedence order (citadel #757): header wins over subprotocol,
// which wins over the deprecated query param. Each case sets the CORRECT
// passcode on the higher-precedence source and a WRONG passcode on every
// lower-precedence source; the connection must still succeed, proving the
// higher-precedence source is the one actually consulted.
func TestPasscodePrecedence_HeaderBeatsSubprotocolBeatsQuery(t *testing.T) {
	port, stop := startSubprotocolTestServer(t, 17914)
	defer stop()

	t.Run("header wins over subprotocol and query", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Citadel-Passcode", "correct-pin")
		wrongEncoded := base64.RawURLEncoding.EncodeToString([]byte("wrong-pin"))
		dialer := websocket.Dialer{
			Subprotocols: []string{terminalPasscodeSubprotocol, wrongEncoded},
		}
		url := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok-1&passcode=also-wrong", port)
		conn, resp, err := dialer.Dial(url, header)
		if err != nil {
			t.Fatalf("dial: %v (status=%v)", err, statusOf(resp))
		}
		conn.Close()
		resp.Body.Close()
	})

	t.Run("subprotocol wins over query when header absent", func(t *testing.T) {
		encoded := base64.RawURLEncoding.EncodeToString([]byte("correct-pin"))
		dialer := websocket.Dialer{
			Subprotocols: []string{terminalPasscodeSubprotocol, encoded},
		}
		url := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok-1&passcode=wrong", port)
		conn, resp, err := dialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial: %v (status=%v)", err, statusOf(resp))
		}
		conn.Close()
		resp.Body.Close()
	})
}

func statusOf(resp *http.Response) interface{} {
	if resp == nil {
		return nil
	}
	return resp.StatusCode
}
