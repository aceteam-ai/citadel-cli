package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/spf13/cobra"
)

var (
	gatewayPort      int
	gatewayUpstream  string
	gatewayModelTier string
	gatewayAPIToken  string
	gatewayPlatform  string
)

var gatewayCmd = &cobra.Command{
	Use:   "gateway",
	Short: "Start the inference gateway proxy with ACET metering",
	Long: `Starts a local HTTP reverse proxy that sits in front of an inference engine
(vLLM, Ollama, etc.) and meters all OpenAI-compatible API requests.

Metered endpoints:
  /v1/chat/completions   - Chat completions
  /v1/completions        - Text completions
  /v1/embeddings         - Embeddings

All other paths are proxied through without metering.

Token usage is extracted from OpenAI-compatible responses (both streaming and
non-streaming) and recorded to ~/.citadel-cli/gateway/transactions.jsonl.

ACET pricing tiers:
  small   (0-8B params)    1 ACET per 1K tokens  ($0.001)
  medium  (8-70B params)   5 ACET per 1K tokens  ($0.005)
  large   (70-400B params) 25 ACET per 1K tokens ($0.025)
  xlarge  (400B+ params)   100 ACET per 1K tokens ($0.100)

Revenue split: 80% operator / 20% platform.

Examples:
  # Proxy to local Ollama
  citadel gateway --upstream http://localhost:11434

  # Proxy to vLLM with custom port and large model tier
  citadel gateway --port 9090 --upstream http://localhost:8000 --model-tier large

  # With ACET settlement enabled
  citadel gateway --upstream http://localhost:11434 --platform https://aceteam.ai/api --api-token act_xxx`,
	RunE: runGateway,
}

func init() {
	gatewayCmd.Flags().IntVar(&gatewayPort, "port", 8080, "Port to listen on")
	gatewayCmd.Flags().StringVar(&gatewayUpstream, "upstream", "http://localhost:11434", "Upstream inference engine URL")
	gatewayCmd.Flags().StringVar(&gatewayModelTier, "model-tier", "medium", "Pricing tier: small, medium, large, xlarge")
	gatewayCmd.Flags().StringVar(&gatewayAPIToken, "api-token", "", "API token for ACET settlement (optional)")
	gatewayCmd.Flags().StringVar(&gatewayPlatform, "platform", "", "Platform base URL for ACET settlement (optional)")
	rootCmd.AddCommand(gatewayCmd)
}

func runGateway(cmd *cobra.Command, args []string) error {
	// Validate tier
	tier, ok := gateway.TierByName(gatewayModelTier)
	if !ok {
		return fmt.Errorf("unknown model tier %q (valid: small, medium, large, xlarge)", gatewayModelTier)
	}

	// Parse upstream URL
	upstreamURL, err := url.Parse(gatewayUpstream)
	if err != nil {
		return fmt.Errorf("invalid upstream URL: %w", err)
	}

	// Set up ledger
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	baseDir := filepath.Join(homeDir, ".citadel-cli")
	ledger := gateway.NewLedger(baseDir)

	// Set up ACET client (optional)
	var acet *gateway.ACETClient
	if gatewayPlatform != "" && gatewayAPIToken != "" {
		acet = gateway.NewACETClient(gatewayPlatform, gatewayAPIToken)
		log.Printf("[Gateway] ACET settlement enabled (platform: %s)", gatewayPlatform)
	}

	// Create reverse proxy
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[Gateway] upstream error: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":"upstream unavailable: %v"}`, err), http.StatusBadGateway)
		},
	}

	// Wrap with metering middleware
	metered := gateway.NewMeteringMiddleware(proxy, ledger, acet, tier)

	// Create server
	addr := fmt.Sprintf("0.0.0.0:%d", gatewayPort)
	server := &http.Server{
		Addr:         addr,
		Handler:      metered,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Handle shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("[Gateway] Shutting down...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
	}()

	// Periodic offline queue flush
	if acet != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if settled, remaining := acet.FlushOfflineQueue(); settled > 0 {
						log.Printf("[Gateway] Flushed %d queued settlements (%d remaining)", settled, remaining)
					}
				}
			}
		}()
	}

	log.Printf("[Gateway] Inference gateway starting on %s", addr)
	log.Printf("[Gateway] Upstream: %s", gatewayUpstream)
	log.Printf("[Gateway] Pricing tier: %s (%d ACET/1K tokens)", tier.Name, tier.ACETPer1K)
	log.Printf("[Gateway] Transaction log: %s/gateway/transactions.jsonl", baseDir)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("gateway server: %w", err)
	}

	return nil
}
