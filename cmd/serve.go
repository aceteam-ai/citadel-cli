// cmd/serve.go
package cmd

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/tlscert"
	"github.com/spf13/cobra"
)

var (
	servePort          int
	serveStatusPort    int
	serveTermPort      int
	serveVNCPort       int
	serveEmbeddingPort int
	serveNoTLS         bool
	serveCertDir       string
	serveBind          string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTPS gateway for this node (standalone)",
	Long: `Starts an HTTPS reverse proxy that consolidates all Citadel node services
behind a single TLS endpoint.

RECOMMENDED: Use 'citadel work' instead, which runs the gateway in-process
alongside the worker by default. This avoids competing tsnet instances and
is the preferred way to expose the gateway on the VPN.

This standalone command is still useful when you need to run the gateway
separately from the worker (e.g., on a different machine or for testing).

The gateway proxies to the local status server (started by 'citadel work
--status-port'), terminal server, and optionally a websockify instance
for VNC WebSocket access.

Routes:
  /health          -> Node health check
  /status          -> Full node status
  /ping            -> Lightweight ping
  /services        -> List registered services
  /api/screenshot       -> Desktop screenshot (auth required)
  /api/actions          -> Desktop input actions (auth required)
  /ssh/authorized-keys  -> SSH key deployment (VPN or auth required)
  /vnc/...              -> VNC WebSocket proxy (requires websockify on --vnc-port)
  /terminal/...    -> Terminal WebSocket server

TLS certificates are self-signed and generated automatically on first run.
They are stored in the Citadel config directory (typically ~/.citadel-cli/tls/).

If the node is connected to the AceTeam Network, the VPN IP (100.64.x.x) is
included as a Subject Alternative Name in the certificate.

Note: Headscale (nexus.aceteam.ai) does not currently support the Tailscale
certificate provisioning API, so automatic Let's Encrypt certs via the VPN
are not available. The self-signed certificate approach is the default.
When Headscale adds cert support, the gateway can be upgraded to use
tsnet.ListenTLS for automatic public certs.`,
	Example: `  # PREFERRED: Run gateway in-process with the worker (enabled by default)
  citadel work

  # Start standalone gateway with default settings (HTTPS on port 8443)
  citadel serve

  # Start on a custom port
  citadel serve --port 443

  # Custom upstream ports
  citadel serve --status-port 9090

  # Combine with the work command (in separate terminals):
  # Terminal 1: citadel work --status-port 8080
  # Terminal 2: citadel serve`,
	RunE: runServe,
}

func runServe(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\n   - Received shutdown signal...")
		cancel()
	}()

	fmt.Println("--- Citadel Gateway ---")

	// Resolve node name and VPN IP for cert SANs
	var nodeName string
	var vpnIPs []net.IP

	Debug("verifying network connection...")
	if connected, err := network.VerifyOrReconnect(ctx); err != nil {
		Debug("network reconnect failed: %v", err)
	} else if connected {
		Debug("network connected")
		if status, err := network.GetGlobalStatus(ctx); err == nil && status.Connected {
			nodeName = status.Hostname
			if status.IPv4 != "" {
				if ip := net.ParseIP(status.IPv4); ip != nil {
					vpnIPs = append(vpnIPs, ip)
				}
			}
			if status.IPv6 != "" {
				if ip := net.ParseIP(status.IPv6); ip != nil {
					vpnIPs = append(vpnIPs, ip)
				}
			}
		}
	}

	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}
	fmt.Printf("   - Node: %s\n", nodeName)

	// Set up TLS
	var tlsConfig *tls.Config
	if !serveNoTLS {
		cert, err := tlscert.EnsureCert(tlscert.Config{
			Hostname:    nodeName,
			IPAddresses: vpnIPs,
			CertDir:     serveCertDir,
		})
		if err != nil {
			return fmt.Errorf("TLS certificate error: %w", err)
		}

		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		}

		fmt.Printf("   - TLS: self-signed (cert: %s)\n", tlscert.CertPath(serveCertDir))
	} else {
		fmt.Println("   - TLS: disabled (--no-tls)")
	}

	// Validate that gateway port does not collide with any upstream port
	upstreamPorts := map[int]string{
		serveStatusPort:    "status-port",
		serveTermPort:      "terminal-port",
		serveVNCPort:       "vnc-port",
		serveEmbeddingPort: "embedding-port",
	}
	if name, collision := upstreamPorts[servePort]; collision {
		return fmt.Errorf("gateway port %d collides with --%s; choose a different --port", servePort, name)
	}

	// Build upstream address strings
	statusAddr := fmt.Sprintf("127.0.0.1:%d", serveStatusPort)
	termAddr := fmt.Sprintf("127.0.0.1:%d", serveTermPort)
	vncAddr := fmt.Sprintf("127.0.0.1:%d", serveVNCPort)
	embeddingAddr := fmt.Sprintf("127.0.0.1:%d", serveEmbeddingPort)

	// Add VPN listener so the gateway is reachable over the tsnet VPN.
	// For HTTPS mode, the raw tsnet listener must be TLS-wrapped so that
	// clients connecting over the VPN get the same TLS termination.
	var vpnGatewayListener net.Listener
	if network.IsGlobalConnected() {
		vpnPort := fmt.Sprintf("%d", servePort)
		rawLn, vpnIP, err := network.ListenVPN("tcp", vpnPort)
		if err != nil {
			Log("gateway VPN listener failed (LAN-only): %v", err)
			fmt.Fprintf(os.Stderr, "   - ⚠️ Gateway VPN listener failed (LAN-only): %v\n", err)
		} else {
			if tlsConfig != nil {
				vpnGatewayListener = tls.NewListener(rawLn, tlsConfig)
			} else {
				vpnGatewayListener = rawLn
			}
			Log("gateway VPN listener on %s:%s", vpnIP, vpnPort)
		}
	}

	// Create gateway
	gw := gateway.NewServer(gateway.Config{
		Port:          servePort,
		ListenAddress: fmt.Sprintf("%s:%d", serveBind, servePort),
		TLSConfig:     tlsConfig,
		NodeName:      nodeName,
	})

	// Load and apply permissions
	perms := config.LoadPermissions(platform.ConfigDir())
	gw.SetPermissions(perms)

	// Service ingress / exposure (issue #598): wire the identity resolver
	// (private/org visibility) and the per-node link-token signing key so an
	// exposed service under /expose/<name>/ can be gated by mesh identity or a
	// signed link token. Harmless when nothing is exposed.
	gw.SetMeshResolver(gatewayMeshResolver{})
	if key, err := config.LoadOrCreateExposeSigningKey(platform.ConfigDir()); err != nil {
		Log("warning: gateway link-token signing disabled: %v", err)
	} else {
		gw.SetExposeSigningKey(key)
	}

	// Register upstreams — status server endpoints
	// These route to the status server started by 'citadel work --status-port'
	gw.AddUpstream("/health", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/status", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/ping", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/services", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/api/screenshot", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/api/actions", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/ssh/authorized-keys", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/provision", &gateway.Upstream{Address: statusAddr})

	// Embedding upstream (issue #351): route the already-metered
	// /v1/embeddings path to the local TEI service (default 127.0.0.1:8102).
	// TEI exposes the OpenAI-compatible endpoint at the same path, so no prefix
	// stripping is needed.
	gw.AddUpstream("/v1/embeddings", &gateway.Upstream{Address: embeddingAddr})

	// Chat routing (issue #581): expose /v1/chat/completions (+ /v1/completions
	// and /v1/models) with model->engine resolution so mesh-direct chat to this
	// node reaches whichever local engine serves the requested model.
	gw.SetChatRouter(newLocalChatLister())

	// VNC WebSocket proxy (requires websockify running on vnc-port)
	gw.AddUpstream("/vnc", &gateway.Upstream{
		Address:     vncAddr,
		StripPrefix: true,
		WebSocket:   true,
	})

	// Terminal WebSocket
	gw.AddUpstream("/terminal", &gateway.Upstream{
		Address:     termAddr,
		StripPrefix: false,
		WebSocket:   true,
	})

	// Attach VPN listener if available
	if vpnGatewayListener != nil {
		gw.AddListener(vpnGatewayListener)
	}

	// Print route table
	scheme := "https"
	if serveNoTLS {
		scheme = "http"
	}
	listenAddr := fmt.Sprintf("%s:%d", serveBind, servePort)
	fmt.Printf("   - Gateway: %s://%s\n", scheme, listenAddr)
	fmt.Println("   - Routes:")
	fmt.Printf("     /health, /status, /ping  -> %s (status server)\n", statusAddr)
	fmt.Printf("     /api/screenshot, /api/actions -> %s\n", statusAddr)
	fmt.Printf("     /ssh/authorized-keys     -> %s (SSH key deploy)\n", statusAddr)
	fmt.Printf("     /v1/embeddings           -> %s (TEI embeddings)\n", embeddingAddr)
	fmt.Printf("     /v1/chat/completions     -> local engine by model (#581)\n")
	fmt.Printf("     /vnc/...                 -> %s (websockify)\n", vncAddr)
	fmt.Printf("     /terminal/...            -> %s (terminal)\n", termAddr)

	if len(vpnIPs) > 0 {
		fmt.Printf("   - VPN access: %s://%s:%d\n", scheme, vpnIPs[0], servePort)
	}

	fmt.Println("--- Gateway started ---")

	// Block until shutdown
	if err := gw.Start(ctx); err != nil {
		if err != context.Canceled {
			return fmt.Errorf("gateway error: %w", err)
		}
	}

	fmt.Println("--- Gateway stopped ---")
	return nil
}

func init() {
	rootCmd.AddCommand(serveCmd)

	serveCmd.Flags().IntVar(&servePort, "port", 8443, "HTTPS gateway port")
	serveCmd.Flags().StringVar(&serveBind, "bind", "0.0.0.0", "Bind address for the gateway")
	serveCmd.Flags().BoolVar(&serveNoTLS, "no-tls", false, "Disable TLS (plain HTTP, for testing only)")
	serveCmd.Flags().StringVar(&serveCertDir, "cert-dir", "", "Custom directory for TLS certificates")

	// Upstream port defaults — these match the defaults in 'citadel work'
	serveCmd.Flags().IntVar(&serveStatusPort, "status-port", 8080, "Port of the local status server (from 'citadel work --status-port')")
	serveCmd.Flags().IntVar(&serveTermPort, "terminal-port", 7860, "Port of the local terminal server")
	serveCmd.Flags().IntVar(&serveVNCPort, "vnc-port", 6080, "Port of websockify (VNC WebSocket bridge)")
	serveCmd.Flags().IntVar(&serveEmbeddingPort, "embedding-port", 8102, "Port of the local TEI embedding service (/v1/embeddings upstream)")

	// Mark --no-tls as hidden (not recommended for production)
	serveCmd.Flags().MarkHidden("no-tls")
}
