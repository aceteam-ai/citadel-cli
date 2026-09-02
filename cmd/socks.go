// cmd/socks.go
package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/socks"
	"github.com/spf13/cobra"
)

var (
	socksBind     string
	socksAuth     string
	socksMaxConns int
	socksVerbose  bool
)

var socksCmd = &cobra.Command{
	Use:   "socks [port]",
	Short: "Run a local SOCKS5 proxy that dials out over the AceTeam Network",
	Long: `Starts a local SOCKS5 dynamic-forward proxy, the same idea as 'ssh -D'.

Point any SOCKS5-aware client (a browser, curl, an app's proxy settings) at
this local port and every connection it opens is dialed through the same
AceTeam Network (tsnet) connection this node already uses for 'citadel
connect'/'citadel proxy' — mesh hostnames (MagicDNS) and mesh IPs both work,
and there's no need to set up a per-service port forward first.

Unlike 'citadel proxy' (one fixed local port -> one fixed remote target),
a SOCKS5 proxy lets the CLIENT pick a different destination per connection.

The proxy runs until interrupted (Ctrl+C).`,
	Example: `  # Start a SOCKS5 proxy on the default port (1080)
  citadel socks

  # Custom local port
  citadel socks 1081

  # Require SOCKS5 username/password auth
  citadel socks --auth alice:s3cret

  # Then point a client at it, e.g.:
  curl --socks5-hostname 127.0.0.1:1080 http://gpu-node-1:11434/v1/models

  # Bind beyond localhost (requires --auth; see the warning this prints)
  citadel socks --bind 0.0.0.0 --auth alice:s3cret`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		port := 1080
		if len(args) == 1 {
			p, err := strconv.Atoi(args[0])
			if err != nil || p < 1 || p > 65535 {
				badColor.Printf("Invalid port: %s\n", args[0])
				os.Exit(1)
			}
			port = p
		}

		// Ensure network connection
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := ensureNetworkConnectedFn(ctx); err != nil {
			cancel()
			badColor.Println(err)
			os.Exit(1)
		}
		cancel()

		var username, password string
		if socksAuth != "" {
			u, p, ok := strings.Cut(socksAuth, ":")
			if !ok || u == "" || p == "" {
				badColor.Println("Invalid --auth value: expected user:pass")
				os.Exit(1)
			}
			username, password = u, p
		}

		if !isLocalBindAddr(socksBind) {
			warnColor.Println("WARNING: --bind is not localhost. This exposes a SOCKS5 proxy onto")
			warnColor.Println("         the AceTeam Network mesh to anyone who can reach this host on")
			warnColor.Println("         your LAN/network, letting them dial out through it as this node.")
			if username == "" {
				warnColor.Println("         No --auth is set, so ANYONE who can reach this port can use it.")
				warnColor.Println("         Pass --auth user:pass to require credentials.")
			}
			fmt.Println()
		}

		srv, err := socks.New(socks.Options{
			Dialer:   network.Dial,
			Username: username,
			Password: password,
			MaxConns: socksMaxConns,
			Logf: func(format string, args ...any) {
				if socksVerbose {
					fmt.Printf(format+"\n", args...)
				}
			},
		})
		if err != nil {
			badColor.Printf("Failed to configure SOCKS5 proxy: %v\n", err)
			os.Exit(1)
		}

		addr := fmt.Sprintf("%s:%d", socksBind, port)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			badColor.Printf("Failed to listen on %s: %v\n", addr, err)
			os.Exit(1)
		}
		defer listener.Close()

		fmt.Println()
		goodColor.Println("SOCKS5 proxy started!")
		fmt.Printf("  Listening: %s\n", addr)
		if username != "" {
			fmt.Println("  Auth:      username/password required")
		} else {
			fmt.Println("  Auth:      none")
		}
		fmt.Println()
		fmt.Println("Press Ctrl+C to stop.")
		fmt.Println()

		runCtx, runCancel := context.WithCancel(context.Background())
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Println("\nShutting down...")
			runCancel()
		}()

		if err := srv.Serve(runCtx, listener); err != nil {
			badColor.Printf("SOCKS5 proxy error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("SOCKS5 proxy stopped.")
	},
}

// isLocalBindAddr reports whether bind is a loopback address (localhost
// only). Mirrors the intent of citadel proxy's --bind flag (cmd/proxy.go):
// 127.0.0.1 by default, with anything else treated as an intentional,
// louder-warned opt-in to a wider bind.
func isLocalBindAddr(bind string) bool {
	if bind == "" || strings.EqualFold(bind, "localhost") {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

func init() {
	rootCmd.AddCommand(socksCmd)
	socksCmd.Flags().StringVar(&socksBind, "bind", "127.0.0.1", "Address to bind to (default localhost only)")
	socksCmd.Flags().StringVar(&socksAuth, "auth", "", "Require SOCKS5 username/password auth: user:pass")
	socksCmd.Flags().IntVar(&socksMaxConns, "max-conns", 0, "Maximum concurrent connections (0 = unlimited)")
	socksCmd.Flags().BoolVarP(&socksVerbose, "verbose", "v", false, "Show connection activity")
}
