// internal/terminal/meshauth_test.go
//
// Tests for mesh-peer identity trust on the terminal endpoint (citadel #585).
// These exercise Server.resolveAuth directly with an injected MockMeshResolver
// so no live mesh is needed, pinning the security contract:
//   - a verified same-owner peer on the VPN path is authorized WITHOUT a token,
//   - an unverified/absent identity with no token is rejected,
//   - the localhost/token path is unchanged (token still required off-VPN, and a
//     valid token's identity is never discarded on a VPN connection).
package terminal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newTestServer builds a server with a mock token validator and (optionally) a
// mock mesh resolver, without starting any listeners.
func newTestServer(mesh MeshIdentityResolver, trustMesh bool) (*Server, *MockTokenValidator) {
	cfg := &Config{
		Port:           7860,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		RateLimitRPS:   1.0,
		RateLimitBurst: 5,
		TrustMeshPeers: trustMesh,
	}
	auth := NewMockTokenValidator()
	s := NewServer(cfg, auth)
	if mesh != nil {
		s.SetMeshResolver(mesh)
	}
	return s, auth
}

// TestResolveAuth_MeshVerifiedOnVPN_NoToken: a verified same-owner peer on the
// VPN listener is authorized with no token, and the session UserID comes from
// the mesh login (per-user tmux re-attach key).
func TestResolveAuth_MeshVerifiedOnVPN_NoToken(t *testing.T) {
	mesh := &MockMeshResolver{Identity: &MeshPeerIdentity{
		NodeName: "gpu-node-1", UserID: "alice@example.com", SameOwner: true,
	}}
	s, _ := newTestServer(mesh, true)

	info, via, err := s.resolveAuth(context.Background(), "", true /*overVPN*/, "100.64.0.9:5000")
	if err != nil {
		t.Fatalf("expected mesh authorization, got error: %v", err)
	}
	if via != "mesh" {
		t.Errorf("authVia = %q, want %q", via, "mesh")
	}
	if info == nil || info.UserID != "alice@example.com" {
		t.Errorf("expected UserID from mesh login, got %+v", info)
	}
	if info.OrgID != "org-1" {
		t.Errorf("expected OrgID from server config, got %q", info.OrgID)
	}
}

// TestResolveAuth_NoTokenNoIdentity_Rejected: without a token and without a
// verified identity the connection is rejected (fail-safe), whether the resolver
// errors or is entirely absent.
func TestResolveAuth_NoTokenNoIdentity_Rejected(t *testing.T) {
	t.Run("resolver errors", func(t *testing.T) {
		mesh := &MockMeshResolver{Err: errors.New("peer not found")}
		s, _ := newTestServer(mesh, true)
		if _, _, err := s.resolveAuth(context.Background(), "", true, "100.64.0.9:5000"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("no resolver wired", func(t *testing.T) {
		s, _ := newTestServer(nil, true)
		if _, _, err := s.resolveAuth(context.Background(), "", true, "100.64.0.9:5000"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected ErrInvalidToken, got %v", err)
		}
	})

	t.Run("not same owner", func(t *testing.T) {
		mesh := &MockMeshResolver{Identity: &MeshPeerIdentity{NodeName: "x", UserID: "eve", SameOwner: false}}
		s, _ := newTestServer(mesh, true)
		if _, _, err := s.resolveAuth(context.Background(), "", true, "100.64.0.9:5000"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected ErrInvalidToken for non-same-owner peer, got %v", err)
		}
	})
}

// TestResolveAuth_MeshTrustGatedToVPN: even a verified peer is NOT trusted when
// the connection did not arrive on the VPN listener (localhost/LAN), nor when
// mesh trust is disabled. Both must require a token.
func TestResolveAuth_MeshTrustGatedToVPN(t *testing.T) {
	mesh := &MockMeshResolver{Identity: &MeshPeerIdentity{NodeName: "n", UserID: "alice", SameOwner: true}}

	t.Run("not over VPN (localhost) -> token required", func(t *testing.T) {
		s, _ := newTestServer(mesh, true)
		if _, _, err := s.resolveAuth(context.Background(), "", false /*overVPN*/, "127.0.0.1:5000"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected rejection off-VPN, got %v", err)
		}
	})

	t.Run("mesh trust disabled -> token required even on VPN", func(t *testing.T) {
		s, _ := newTestServer(mesh, false /*trustMesh*/)
		if _, _, err := s.resolveAuth(context.Background(), "", true, "100.64.0.9:5000"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expected rejection with mesh trust off, got %v", err)
		}
	})
}

// TestResolveAuth_TokenPathUnchanged: the token path is preserved exactly. A
// valid token authorizes on every listener; an invalid token is rejected with
// its mapped error.
func TestResolveAuth_TokenPathUnchanged(t *testing.T) {
	s, auth := newTestServer(nil, true)
	auth.AddValidToken("good", &TokenInfo{UserID: "u-token", OrgID: "org-1"})

	// Valid token, localhost.
	info, via, err := s.resolveAuth(context.Background(), "good", false, "127.0.0.1:5000")
	if err != nil || via != "token" || info.UserID != "u-token" {
		t.Fatalf("valid token off-VPN: info=%+v via=%q err=%v", info, via, err)
	}

	// Invalid token, over VPN, no resolver -> ErrInvalidToken (mesh cannot save
	// a *present but bad* token; a bad token governs).
	if _, _, err := s.resolveAuth(context.Background(), "bad", true, "100.64.0.9:5000"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("invalid token: expected ErrInvalidToken, got %v", err)
	}
}

// TestResolveAuth_TokenFirstOnVPN is the guard against reintroducing "mesh-first"
// ordering: on a VPN connection carrying a VALID platform token, the token's own
// UserID must win — the mesh login must NOT overwrite it. Mesh-first would
// collapse every relayed user onto the relay's single mesh login (one shared
// tmux session), a cross-user regression.
func TestResolveAuth_TokenFirstOnVPN(t *testing.T) {
	mesh := &MockMeshResolver{Identity: &MeshPeerIdentity{
		NodeName: "backend-relay", UserID: "relay@aceteam.ai", SameOwner: true,
	}}
	s, auth := newTestServer(mesh, true)
	auth.AddValidToken("user-tok", &TokenInfo{UserID: "per-user-42", OrgID: "org-1"})

	info, via, err := s.resolveAuth(context.Background(), "user-tok", true /*overVPN*/, "100.64.0.2:5000")
	if err != nil {
		t.Fatalf("expected token authorization on VPN, got %v", err)
	}
	if via != "token" {
		t.Errorf("authVia = %q, want token (token must take precedence over mesh)", via)
	}
	if info.UserID != "per-user-42" {
		t.Errorf("UserID = %q, want per-user-42 (mesh login must NOT overwrite the token identity)", info.UserID)
	}
}
