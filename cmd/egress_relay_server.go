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

	allowLAN := resolveEgressAllowLAN()

	vpnPort := fmt.Sprintf("%d", services.EgressRelayPort)
	vpnLn, vpnIP, err := network.ListenVPN("tcp", vpnPort)
	if err != nil {
		Log("egress relay VPN listener failed: %v", err)
		fmt.Fprintf(os.Stderr, "   - ⚠️ Egress relay enabled but its VPN listener failed: %v\n", err)
		return
	}

	dialer := (&net.Dialer{}).DialContext
	srv, err := egressrelay.New(egressrelay.Options{
		Dialer:      dialer,
		Resolver:    egressRelayMeshResolver{},
		AllowLAN:    func() bool { return allowLAN },
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

	Log("egress relay VPN listener on %s:%s (allow_lan=%v)", vpnIP, vpnPort, allowLAN)
	fmt.Printf("   - Egress relay: %s:%d (mesh-only, same-org verified peers, allow_lan=%v)\n", vpnIP, services.EgressRelayPort, allowLAN)
}
