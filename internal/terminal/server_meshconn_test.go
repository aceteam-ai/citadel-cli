// internal/terminal/server_meshconn_test.go
//go:build !windows

// End-to-end check that the VPN-listener tagging (meshTaggedListener + the
// ConnContext hook) actually threads the "arrived over the mesh" signal into
// handleWebSocket, so mesh-peer identity trust is really gated to VPN
// connections (citadel #585). Uses a real PTY (bare /bin/sh via
// SessionName="none"), so it is Linux/macOS only, matching the PTY code.
package terminal

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMeshTrust_VPNvsLocalhost_EndToEnd(t *testing.T) {
	const primaryPort = 17870

	cfg := &Config{
		Host:           "127.0.0.1",
		Port:           primaryPort,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		SessionName:    "none", // force a bare shell so the test needs no tmux
		TrustMeshPeers: true,
		RateLimitRPS:   100,
		RateLimitBurst: 100,
	}
	s := NewServer(cfg, NewMockTokenValidator())
	s.SetMeshResolver(&MockMeshResolver{Identity: &MeshPeerIdentity{
		NodeName: "gpu-node-1", UserID: "alice@example.com", SameOwner: true,
	}})

	// Extra listener simulates the tsnet VPN listener; AddListener wraps it so
	// its connections are tagged mesh-originated.
	vpnLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("vpn listener: %v", err)
	}
	vpnAddr := vpnLn.Addr().(*net.TCPAddr)
	s.AddListener(vpnLn)

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(context.Background())
	time.Sleep(100 * time.Millisecond)

	dial := func(port int) (int, error) {
		u := fmt.Sprintf("ws://127.0.0.1:%d/terminal", port) // no ?token=
		conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if conn != nil {
			conn.Close()
		}
		return status, err
	}

	// Over the VPN listener, no token -> authorized by mesh identity (101).
	if _, err := dial(vpnAddr.Port); err != nil {
		t.Errorf("VPN tokenless connect should succeed via mesh identity, got error: %v", err)
	}

	// Over the localhost (primary) listener, no token -> rejected (401). The
	// mesh path must NOT engage off the VPN listener.
	status, err := dial(primaryPort)
	if err == nil {
		t.Errorf("localhost tokenless connect should be rejected, but it succeeded")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("localhost tokenless connect: status = %d, want 401", status)
	}
}
