// cmd/terminal_server.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/spf13/cobra"
)

var (
	terminalPort        int
	terminalOrgID       string
	terminalIdleTimeout int
	terminalShell       string
	terminalMaxConns    int
	terminalTestMode    bool
)

var terminalServerCmd = &cobra.Command{
	Use:   "terminal-server",
	Short: "Starts the WebSocket terminal server for remote terminal access",
	Long: `Starts a WebSocket server that enables browser-based terminal access
through the AceTeam web application. The server authenticates connections
using tokens validated against the AceTeam API.

The terminal server creates PTY (pseudo-terminal) sessions for each
authenticated connection, streaming input and output over WebSocket.`,
	Example: `  # Start using org-id from manifest (set during 'citadel init')
  citadel terminal-server

  # Start with explicit organization ID
  citadel terminal-server --org-id my-org-id

  # Start on a custom port
  citadel terminal-server --port 8080

  # Start with custom idle timeout (in minutes)
  citadel terminal-server --idle-timeout 60

  # Start in test mode (accepts any token, for development)
  citadel terminal-server --test`,

	PreRunE: func(cmd *cobra.Command, args []string) error {
		// Check platform support
		if runtime.GOOS == "windows" {
			return fmt.Errorf("terminal server is not yet supported on Windows (PTY support requires ConPTY)")
		}

		// Test mode skips org-id validation but requires explicit opt-in
		if terminalTestMode {
			// Require environment variable to prevent accidental production use
			if os.Getenv("CITADEL_ALLOW_TEST_MODE") != "true" {
				return fmt.Errorf("--test mode requires CITADEL_ALLOW_TEST_MODE=true environment variable (security safeguard)")
			}
			terminalOrgID = "test-org"
			return nil
		}

		// Try to get org-id from manifest if not provided via flag
		if terminalOrgID == "" {
			if manifest, _, err := findAndReadManifest(); err == nil {
				terminalOrgID = manifest.Node.OrgID
			}
		}

		// Validate org-id is available
		if terminalOrgID == "" {
			return fmt.Errorf("--org-id is required (or configure via 'citadel init')")
		}

		return nil
	},

	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- Starting Citadel Terminal Server ---")

		// Build configuration
		config := terminal.DefaultConfig()
		config.OrgID = terminalOrgID
		config.AuthServiceURL = authServiceURL

		if terminalPort != 0 {
			config.Port = terminalPort
		}
		if terminalIdleTimeout != 0 {
			config.IdleTimeout = time.Duration(terminalIdleTimeout) * time.Minute
		}
		if terminalShell != "" {
			config.Shell = terminalShell
		}
		if terminalMaxConns != 0 {
			config.MaxConnections = terminalMaxConns
		}

		// Validate configuration
		if err := config.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Configuration error: %v\n", err)
			os.Exit(1)
		}

		// Create the token validator
		var auth terminal.TokenValidator
		var cachingAuth *terminal.CachingTokenValidator

		if terminalTestMode {
			// Test mode: accept any token
			mockAuth := terminal.NewMockTokenValidator()
			mockAuth.AddValidToken("test-token", &terminal.TokenInfo{
				UserID:    "test-user",
				OrgID:     config.OrgID,
				ExpiresAt: time.Now().Add(24 * time.Hour),
			})
			auth = mockAuth
			fmt.Println("   - ⚠️  TEST MODE: Accepting test token (not for production)")
		} else {
			// Production mode: use caching validator
			cachingAuth = terminal.NewCachingTokenValidator(
				config.AuthServiceURL,
				config.OrgID,
				config.TokenRefreshInterval,
			)
			auth = cachingAuth
		}

		// Create and start the server
		server := terminal.NewServer(config, auth)

		// Start the caching validator's background refresh (if not in test mode)
		if cachingAuth != nil {
			if err := cachingAuth.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Failed to start token cache: %v\n", err)
				os.Exit(1)
			}
		}

		if err := server.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to start server: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("   - Port: %d\n", config.Port)
		fmt.Printf("   - Shell: %s\n", config.Shell)
		fmt.Printf("   - Idle timeout: %v\n", config.IdleTimeout)
		fmt.Printf("   - Max connections: %d\n", config.MaxConnections)
		fmt.Printf("   - Auth service: %s\n", config.AuthServiceURL)
		fmt.Printf("   - Organization: %s\n", config.OrgID)
		fmt.Println("   - ✅ Terminal server is running")
		fmt.Println()
		fmt.Printf("WebSocket endpoint: ws://localhost:%d/terminal\n", config.Port)
		fmt.Printf("Health endpoint:    http://localhost:%d/health\n", config.Port)
		fmt.Println()
		fmt.Println("Press Ctrl+C to stop the server")

		// Wait for shutdown signal
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs

		fmt.Println("\n--- Shutting down terminal server ---")

		// Stop the caching validator
		if cachingAuth != nil {
			cachingAuth.Stop()
		}

		// Graceful shutdown with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Stop(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "   - Warning: shutdown error: %v\n", err)
		}

		fmt.Println("   - ✅ Terminal server stopped")
	},
}

func init() {
	rootCmd.AddCommand(terminalServerCmd)

	terminalServerCmd.Flags().IntVar(&terminalPort, "port", 0, "Port to listen on (default: 7860)")
	terminalServerCmd.Flags().StringVar(&terminalOrgID, "org-id", "", "Organization ID for token validation (reads from manifest if not provided)")
	terminalServerCmd.Flags().IntVar(&terminalIdleTimeout, "idle-timeout", 0, "Idle timeout in minutes (default: 30)")
	terminalServerCmd.Flags().StringVar(&terminalShell, "shell", "", "Shell to use for terminal sessions")
	terminalServerCmd.Flags().IntVar(&terminalMaxConns, "max-connections", 0, "Maximum concurrent connections (default: 10)")
	terminalServerCmd.Flags().BoolVar(&terminalTestMode, "test", false, "Test mode - accepts any token (for development)")
}
