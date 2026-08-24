package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/services"
)

// startCobrowseStreamServer starts the interactive browser-session stream server
// (citadel-cli#794) for the worker's lifetime. It serves a WebSocket that
// screencasts a running co-browse session (#793) and forwards viewer input back,
// exposed over the tsnet mesh exactly like the terminal / VNC / deskstream
// servers -- reusing the node's existing transport and NAT-traversal relay
// rather than standing up a new one.
//
// It is best-effort and unconditional: a viewer for a session can attach at any
// time, and a bound-but-idle listener costs nothing, so we never fail the worker
// on a stream-server error. The server binds localhost plus (when connected) the
// assigned VPN IP so the platform relay can dial ws://<vpn_ip>:<port>/cobrowse/stream.
func startCobrowseStreamServer() *cobrowsestream.Server {
	srv := cobrowsestream.NewServer(
		cobrowsestream.Config{Port: services.CobrowseStreamPort},
		cobrowsestream.NewManagerSession(),
	)

	// Attach the VPN listener up front (before Start) so it begins serving on the
	// mesh in the same pass, mirroring the deskstream/VNC exposure. Bind the
	// explicit assigned VPN IP (not ":port") so relay-dialed connections match.
	if network.IsGlobalConnected() {
		vpnPort := fmt.Sprintf("%d", services.CobrowseStreamPort)
		if vpnLn, vpnIP, err := network.ListenVPN("tcp", vpnPort); err != nil {
			Log("cobrowse stream server VPN listener failed (localhost-only): %v", err)
		} else {
			srv.AddListener(vpnLn)
			Log("cobrowse stream server VPN listener on %s:%s", vpnIP, vpnPort)
		}
	}

	go func() {
		if err := srv.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "   - ⚠️ Cobrowse stream server error: %v\n", err)
		}
	}()
	return srv
}
