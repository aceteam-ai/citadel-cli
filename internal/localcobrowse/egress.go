package localcobrowse

import (
	"context"
	"net"

	"github.com/aceteam-ai/citadel-cli/internal/browserurl"
)

// EgressGuard is local mode's --proxy-server. It lives in browserurl so the
// node-wide manager can run the same guard for public-only remote sessions
// (citadel-cli#1867).
type EgressGuard = browserurl.EgressGuard

// ErrEgressRefused is returned when a destination is not a public address.
var ErrEgressRefused = browserurl.ErrEgressRefused

// StartEgressGuard listens on 127.0.0.1. resolve and dial default to the
// system resolver and dialer.
func StartEgressGuard(resolve Resolver, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*EgressGuard, error) {
	return browserurl.StartEgressGuard(resolve, dial)
}
