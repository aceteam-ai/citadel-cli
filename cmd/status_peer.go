package cmd

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// statusBindLAN owns the operator opt-in for a LAN bind of the plaintext
// status listener: the flag, or CITADEL_STATUS_BIND_LAN with a truthy value.
// The default is loopback plus the mesh listener only.
func statusBindLAN(flag bool) bool {
	if flag {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_STATUS_BIND_LAN"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// meshPeerVerifierTimeout bounds one WhoIs lookup on the request path.
const meshPeerVerifierTimeout = 3 * time.Second

// meshPeerVerifier backs status.ServerConfig.PeerVerifier. It trusts a mesh
// caller only when the coordination server vouches for it (whois returns no
// error) AND the peer is same-owner with a stable device id and owner id, the
// same keys meshtransfer.Verify binds on. An error, a missing id or a
// different owner is unverified (fail closed).
func meshPeerVerifier(whois func(ctx context.Context, remoteAddr string) (*network.PeerIdentity, error)) status.PeerVerifier {
	return func(ctx context.Context, remoteAddr string) bool {
		ctx, cancel := context.WithTimeout(ctx, meshPeerVerifierTimeout)
		defer cancel()
		id, err := whois(ctx, remoteAddr)
		if err != nil || id == nil {
			return false
		}
		return id.SameOwner && id.StableID != "" && id.OwnerID != ""
	}
}
