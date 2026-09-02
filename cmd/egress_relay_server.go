// cmd/egress_relay_server.go
//
// citadel #787: wires internal/egressrelay's Server into `citadel work`.
// Kept out of cmd/work.go itself (which callers already touch heavily) --
// see startCobrowseStreamServer (cmd/cobrowse_stream.go) for the identical
// "helper started from runWork, defined in its own file" pattern this
// mirrors.
package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/egressrelay"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/services"
)

// startEgressRelay starts the on-node SOCKS5 egress relay (citadel #787) for
// the worker's lifetime, when enabled. Unlike startCobrowseStreamServer, this
// binds MESH-ONLY via network.ListenVPN -- never localhost/LAN -- because the
// relay's entire authorization model is verified mesh-peer identity
// (egressRelayMeshResolver / network.WhoIsPeer); a plain TCP/LAN bind would
// have no equivalent gate and would let anyone who can reach this host on the
// LAN egress through it as this node.
//
// A no-op (with a clear reason logged) when: the relay is not enabled
// (resolveEgressRelay/workEgressRelay), or the node is not connected to the
// AceTeam Network (no mesh, nothing to authorize against). Never fails the
// worker -- mirrors every other best-effort optional listener started here
// (terminal VPN listener, cobrowse stream server).
//
// --egress-relay force-enables regardless of the persisted/env-resolved
// value (workEgressRelay is OR'd into enabled); there is deliberately no
// analogous flag for allow_lan -- that knob only ever comes from
// resolveEgressAllowLAN (config/env), the fail-closed direction, so a
// caller cannot accidentally force LAN/mesh pivot on via a CLI flag the way
// they can force the relay itself on.
func startEgressRelay(ctx context.Context) {
	enabled := workEgressRelay || resolveEgressRelay()
	if !enabled {
		Debug("egress relay disabled (see 'citadel egress-relay status')")
		return
	}
	if !network.IsGlobalConnected() {
		Log("egress relay enabled but not connected to the AceTeam Network; skipping")
		return
	}

	vpnPort := fmt.Sprintf("%d", services.EgressRelayPort)
	vpnLn, vpnIP, err := network.ListenVPN("tcp", vpnPort)
	if err != nil {
		Log("egress relay VPN listener failed: %v", err)
		fmt.Fprintf(os.Stderr, "   - ⚠️ Egress relay enabled but its VPN listener failed: %v\n", err)
		return
	}

	// KeepAlive bounds a relayed connection whose peer goes silent without
	// closing (the CONNECT target never sends FIN/RST): without it, an
	// abandoned connection holds a goroutine + two fds (client + egress leg)
	// indefinitely inside this long-lived `citadel work` process. Mirrors the
	// gateway's own bounded-connection posture (its http.Server.WriteTimeout).
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	srv, err := egressrelay.New(egressrelay.Options{
		Dialer:   dialer.DialContext,
		Resolver: egressRelayMeshResolver{},
		// AllowLAN is the live wrapper (network.GetNodeConfigDir()-backed), not
		// a value captured once at startup: Options.AllowLAN is documented as
		// read per-connection so a config change (via 'citadel egress-relay
		// allow-lan') takes effect on the NEXT connection through an
		// already-running relay, not only after a full worker restart --
		// unlike Enabled itself, which is only re-evaluated at startup (the
		// listener isn't torn down/rebuilt live).
		AllowLAN: resolveEgressAllowLAN,
		// MaxConns bounds concurrent relayed connections from ANY authorized
		// peer -- this is a mesh-facing listener on a node whose whole value
		// proposition is "has open egress", so one authorized-but-misbehaving
		// peer must not be able to exhaust this node's file descriptors.
		// Mirrors citadel socks's --max-conns (#786), just with a sane default
		// here since there is no interactive flag for the worker-started relay.
		MaxConns:    egressRelayMaxConns,
		DialTimeout: 30 * time.Second,
		Logf: func(format string, args ...any) {
			Debug(format, args...)
		},
	})
	if err != nil {
		Log("egress relay: failed to construct server: %v", err)
		vpnLn.Close()
		return
	}

	go func() {
		if err := srv.Serve(ctx, vpnLn); err != nil {
			fmt.Fprintf(os.Stderr, "   - ⚠️ Egress relay server error: %v\n", err)
		}
	}()

	allowLAN := resolveEgressAllowLAN()
	Log("egress relay VPN listener on %s:%s (allow_lan=%v, max_conns=%d)", vpnIP, vpnPort, allowLAN, egressRelayMaxConns)
	fmt.Printf("   - Egress relay: %s:%d (mesh-only, same-org verified peers, allow_lan=%v)\n", vpnIP, services.EgressRelayPort, allowLAN)
}

// egressRelayMaxConns bounds concurrent relayed connections. A package var
// (not a const) so a future flag/env override can set it without touching
// the call site; 256 is a generous default for a single-node relay -- well
// above realistic legitimate concurrency, but low enough to bound fd
// exhaustion from a misbehaving authorized peer.
var egressRelayMaxConns = 256
