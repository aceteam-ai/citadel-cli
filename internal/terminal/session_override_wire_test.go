// internal/terminal/session_override_wire_test.go
//go:build !windows

// End-to-end check that handleWebSocket actually reads the "session" query
// parameter and threads it into resolveSessionCommand (citadel #759): every
// other test in session_override_test.go calls resolveSessionCommand
// directly, which would stay green even if the query-param read in
// handleWebSocket were ever dropped. This dials a real WebSocket (real PTY,
// bare shell via SessionName="citadel" with tmux forced unresolvable) so the
// only way to tell the two dials apart is the "tmux unavailable" warning the
// no-override path is expected to log and the "?session=none" path is not.
package terminal

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// captureLogger records every Printf line so a test can assert on server-side
// log output without depending on stderr, mirroring the Logger seam SetSilent
// already proves is swappable.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLogger) Printf(format string, v ...interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, v...))
}

func (c *captureLogger) Debugf(format string, v ...interface{}) {}

func (c *captureLogger) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (c *captureLogger) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = nil
}

func TestHandleWebSocket_SessionQueryParamWiring(t *testing.T) {
	// tmux never resolves, so whether the "tmux unavailable" warning fires
	// depends ENTIRELY on whether a session was wanted, i.e. on how the
	// "session" query param was (or wasn't) read. Isolate from the ambient
	// test-runner environment (citadel #751): if TMUX happened to be set, the
	// nesting guard would fire first and log a different message.
	t.Setenv("TMUX", "")
	t.Setenv("CITADEL_TMUX_BIN", filepath.Join(t.TempDir(), "missing"))

	const port = 17880
	cfg := &Config{
		Host:           "127.0.0.1",
		Port:           port,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		SessionName:    "citadel", // node default WANTS a persistent session
		RateLimitRPS:   100,
		RateLimitBurst: 100,
	}
	auth := NewMockTokenValidator()
	auth.AddValidToken("tok_test", &TokenInfo{UserID: "alice", OrgID: "org-1"})

	s := NewServer(cfg, auth)
	logger := &captureLogger{}
	s.logger = logger // same package: internal field access is deliberate here

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(context.Background())
	time.Sleep(100 * time.Millisecond)

	dial := func(sessionQS string) {
		u := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok_test", port)
		if sessionQS != "" {
			u += "&session=" + sessionQS
		}
		conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			t.Fatalf("dial(%q): %v", sessionQS, err)
		}
		if resp != nil {
			resp.Body.Close()
		}
		conn.Close()
		// Give the server goroutine time to run the session decision and log
		// (it runs synchronously right after the upgrade, before the PTY
		// read/write loop that the client's immediate Close() tears down).
		time.Sleep(150 * time.Millisecond)
	}

	// No override: the node default (SessionName="citadel") wants a
	// persistent session; tmux is forced unresolvable -> the fallback
	// warning must fire.
	dial("")
	if !logger.contains("tmux unavailable") {
		t.Error(`expected a "tmux unavailable" warning with no override (node default wants tmux)`)
	}

	logger.reset()

	// "?session=none": the override must force a bare shell, overriding the
	// node's own persistent default -> no fallback warning, because no
	// session was wanted in the first place.
	dial("none")
	if logger.contains("tmux unavailable") {
		t.Error(`unexpected "tmux unavailable" warning with "?session=none" (override should force a bare shell with nothing to fall back FROM)`)
	}
}

// TestHandleWebSocket_InsideTmuxSkipsNesting is the end-to-end counterpart to
// TestSessionCommand_InsideTmux/TestResolveSessionCommand_InsideTmuxSkipsNesting:
// with a fully resolvable tmux AND a session wanted, setting TMUX in this
// process's environment (simulating citadel itself running inside a tmux
// client) must still fall back to a bare shell and log the distinct nesting
// message rather than "tmux unavailable" (citadel #751).
func TestHandleWebSocket_InsideTmuxSkipsNesting(t *testing.T) {
	makeFakeTmux(t) // tmux IS available; the nesting guard must still win.
	t.Setenv("TMUX", "/tmp/tmux-1000/default,12345,0")

	const port = 17881
	cfg := &Config{
		Host:           "127.0.0.1",
		Port:           port,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		SessionName:    "citadel", // node default WANTS a persistent session
		RateLimitRPS:   100,
		RateLimitBurst: 100,
	}
	auth := NewMockTokenValidator()
	auth.AddValidToken("tok_test", &TokenInfo{UserID: "alice", OrgID: "org-1"})

	s := NewServer(cfg, auth)
	logger := &captureLogger{}
	s.logger = logger // same package: internal field access is deliberate here

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(context.Background())
	time.Sleep(100 * time.Millisecond)

	u := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok_test", port)
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	conn.Close()
	time.Sleep(150 * time.Millisecond)

	if logger.contains("tmux unavailable") {
		t.Error(`unexpected "tmux unavailable" warning; tmux resolves fine here, nesting is the reason`)
	}
	if !logger.contains("already inside a tmux session") {
		t.Error(`expected an "already inside a tmux session" note when TMUX is set on the citadel process`)
	}
}

// TestHandleWebSocket_ExplicitTmuxHonoredInsideTmux is the end-to-end
// counterpart to TestSessionCommand_ExplicitHonoredInsideTmux/
// TestResolveSessionCommand_ExplicitOverrideHonoredInsideTmux (the
// review-requested fix for citadel #751, PR #769 was BLOCKed for missing
// this): a connection carrying an EXPLICIT "?session=" override (the CLI's
// --tmux path) must get a real persistent-session debug log — not the
// nesting fallback — even though this process is already inside a tmux
// client. Only the auto/no-override path avoids nesting.
func TestHandleWebSocket_ExplicitTmuxHonoredInsideTmux(t *testing.T) {
	makeFakeTmux(t) // tmux IS available.
	t.Setenv("TMUX", "/tmp/tmux-1000/default,12345,0")

	const port = 17882
	cfg := &Config{
		Host:           "127.0.0.1",
		Port:           port,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		SessionName:    "none", // node default is OFF; the explicit override opts back in
		RateLimitRPS:   100,
		RateLimitBurst: 100,
	}
	auth := NewMockTokenValidator()
	auth.AddValidToken("tok_test", &TokenInfo{UserID: "alice", OrgID: "org-1"})

	s := NewServer(cfg, auth)
	logger := &captureLogger{}
	s.logger = logger // same package: internal field access is deliberate here

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(context.Background())
	time.Sleep(100 * time.Millisecond)

	u := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok_test&session=citadel", port)
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	conn.Close()
	time.Sleep(150 * time.Millisecond)

	if logger.contains("already inside a tmux session") {
		t.Error(`unexpected nesting fallback; an explicit --tmux override must be honored even inside tmux`)
	}
	if logger.contains("tmux unavailable") {
		t.Error(`unexpected "tmux unavailable" warning; tmux resolves fine here`)
	}
}
