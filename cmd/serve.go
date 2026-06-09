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

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/tlscert"
	"github.com/spf13/cobra"
)

var (
	servePort       int
	serveStatusPort int
	serveFabricPort int
	serveTermPort   int
	serveVNCPort    int
	serveNoTLS      bool
	serveCertDir    string
	serveBind       string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTPS gateway for this node",
	Long: `Starts an HTTPS reverse proxy that consolidates all Citadel node services
behind a single TLS endpoint.

Routes:
  /health          -> Node health check
  /status          -> Full node status
  /ping            -> Lightweight ping
  /services        -> List registered services
  /api/screenshot  -> Desktop screenshot (auth required)
  /api/actions     -> Desktop input actions (auth required)
  /api/...         -> Fabric server API
  /vnc/...         -> VNC WebSocket proxy (requires websockify on --vnc-port)
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
	Example: `  # Start gateway with default settings (HTTPS on port 8443)
  citadel serve

  # Start on a custom port
  citadel serve --port 443

  # Start without TLS (HTTP only, for testing)
  citadel serve --no-tls

  # Custom upstream ports
  citadel serve --status-port 9090 --fabric-port 8080

  # Combine with the work command (in separate terminals):
  # Terminal 1: citadel work --status-port 9090
  # Terminal 2: citadel serve --status-port 9090`,
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
		}

		certDir := serveCertDir
		if certDir == "" {
			certDir = tlscert.CertPath("")
		}
		fmt.Printf("   - TLS: self-signed (cert: %s)\n", tlscert.CertPath(serveCertDir))
	} else {
		fmt.Println("   - TLS: disabled (--no-tls)")
	}

	// Build upstream address strings
	statusAddr := fmt.Sprintf("127.0.0.1:%d", serveStatusPort)
	fabricAddr := fmt.Sprintf("127.0.0.1:%d", serveFabricPort)
	termAddr := fmt.Sprintf("127.0.0.1:%d", serveTermPort)
	vncAddr := fmt.Sprintf("127.0.0.1:%d", serveVNCPort)

	// Create gateway
	gw := gateway.NewServer(gateway.Config{
		Port:          servePort,
		ListenAddress: fmt.Sprintf("%s:%d", serveBind, servePort),
		TLSConfig:     tlsConfig,
		NodeName:      nodeName,
	})

	// Register upstreams — status server endpoints
	gw.AddUpstream("/health", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/status", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/ping", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/services", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/api/screenshot", &gateway.Upstream{Address: statusAddr})
	gw.AddUpstream("/api/actions", &gateway.Upstream{Address: statusAddr})

	// Fabric server API (catch-all for /api/...)
	gw.AddUpstream("/api", &gateway.Upstream{Address: fabricAddr})

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
	fmt.Printf("     /api/...                 -> %s (fabric server)\n", fabricAddr)
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

	// Upstream port defaults — these should match the ports used by 'citadel work'
	serveCmd.Flags().IntVar(&serveStatusPort, "status-port", 8080, "Port of the local status server")
	serveCmd.Flags().IntVar(&serveFabricPort, "fabric-port", 8443, "Port of the local fabric server")
	serveCmd.Flags().IntVar(&serveTermPort, "terminal-port", 7860, "Port of the local terminal server")
	serveCmd.Flags().IntVar(&serveVNCPort, "vnc-port", 6080, "Port of websockify (VNC WebSocket bridge)")

	// Mark --no-tls as hidden (not recommended for production)
	serveCmd.Flags().MarkHidden("no-tls")
}
