// cmd/ingress.go
//
// citadel #1055: mesh-ingress reverse proxy mode. `citadel ingress` is an
// in-process, mesh-joined reverse proxy that fronts hosted web apps (per-node
// pods) at <slug>.<apps-domain> over public HTTPS. It reuses the SAME tsnet
// userspace mesh join every other citadel node uses (VerifyOrReconnect, with
// recoverStaleVPN on stale state -- see cmd/egress_relay_serve.go, the template
// for this command's mesh precondition and signal/teardown shape), so the same
// binary can run as a public ingress anywhere with a public IP, unprivileged,
// with no host-networking changes.
//
// THE ROUTES MAP IS THE AUTHORIZATION BOUNDARY: the ingress only dials pods the
// control-plane routes feed names with a valid mesh IP. Node mesh IPs arrive in
// that map, so the ingress holds ZERO mesh-control secrets -- only a routes
// bearer, an ACME/DNS credential, and its own (self-healing) node identity.
//
// The command is deliberately structured around a testable core (ingressServe)
// with every external dependency injected: the routes client, the TLS cert
// provider, the mesh dialer, and the listener factory. That is what lets the
// happy start-then-shutdown path and the not-logged-in path run under `go test`
// without touching the live mesh or binding a public port (see ingress_test.go).
package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/ingress"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

// Flag-backed values (each with a CITADEL_INGRESS_* env fallback resolved in
// runIngress; the flag wins when both are set).
var (
	ingressRoutesURL    string
	ingressAppsDomain   string
	ingressACMEEmail    string
	ingressACMECAURL    string
	ingressListenAddr   string
	ingressHTTPListen   string
	ingressPollInterval time.Duration
	ingressAuthKey      string
)

// Seams (package vars) so the production defaults can be swapped in tests
// without touching the live node -- mirrors egressRelayIsConnected/
// egressRelayListenVPN. ingressListen binds the PUBLIC port in production; a
// test swaps it for a 127.0.0.1:0 loopback listener so no public bind ever
// happens under `go test`.
var (
	ingressIsConnected = network.IsGlobalConnected
	ingressListen      = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
	ingressDialContext = network.Dial
)

var ingressCmd = &cobra.Command{
	Use:   "ingress",
	Short: "Run a public mesh-ingress reverse proxy for hosted apps",
	Long: `Run this node as a public ingress: it joins the AceTeam Network, pulls a
routes map from the control plane, and reverse-proxies public HTTPS traffic at
<slug>.<apps-domain> to the corresponding app pod over the mesh.

The routes map is the authorization boundary -- an unknown or torn-down slug is
a 404 and nothing is dialed. Gated apps get a server-to-server authorization
check before proxying; inbound session cookies and trust headers are stripped
before the pod ever sees the request.

Wildcard TLS is served from either a mounted, already-issued certificate
(recommended for the first single ingress -- avoids every instance racing to
issue the same wildcard) or via ACME DNS-01 against a delegated challenge zone.

The node must already be logged in ('citadel login' or 'citadel init'); pass
--authkey for a first-boot mesh join. Configuration is entirely via flags and
CITADEL_INGRESS_* environment variables; nothing is written to the repo and
nothing is persisted except the network state and the certificate cache.

Examples:
  # Static (mounted) wildcard cert -- the recommended first deployment:
  CITADEL_INGRESS_ROUTES_TOKEN=... \
  CITADEL_INGRESS_CERT_FILE=/certs/wildcard.crt \
  CITADEL_INGRESS_KEY_FILE=/certs/wildcard.key \
    citadel ingress --routes-url https://cp.example.com/ingress/routes \
                    --apps-domain apps.example.com

  # ACME DNS-01 against a delegated zone (RFC 2136 / TSIG):
  CITADEL_INGRESS_ROUTES_TOKEN=... \
  CITADEL_INGRESS_ACME_DNS_ZONE=acme-delegated.example.net \
  CITADEL_INGRESS_DNS_RFC2136_SERVER=ns1.example.net:53 \
  CITADEL_INGRESS_DNS_RFC2136_KEY_NAME=ingress. \
  CITADEL_INGRESS_DNS_RFC2136_KEY_ALG=hmac-sha256. \
  CITADEL_INGRESS_DNS_RFC2136_KEY=<base64-tsig-secret> \
    citadel ingress --routes-url https://cp.example.com/ingress/routes \
                    --apps-domain apps.example.com --acme-email ops@example.com`,
	Args: cobra.NoArgs,
	RunE: runIngress,
}

func runIngress(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	opts, err := resolveIngressOptions(ctx)
	if err != nil {
		return err
	}
	return ingressServe(ctx, cancel, network.HasState, network.VerifyOrReconnect, sigs, opts)
}

// ingressOptions is the fully-resolved dependency set ingressServe operates on.
// Everything external is here so the core is exercisable without the mesh.
type ingressOptions struct {
	appsDomain  string
	listenAddr  string
	httpListen  string // optional :80 HTTP->HTTPS redirect bind; empty = disabled
	cookieNames []string

	routes      *ingress.Client
	authorizer  ingress.Authorizer
	cert        ingress.CertProvider
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	isConnected func() bool
	listen      func(addr string) (net.Listener, error)
	logf        func(format string, args ...any)

	// authKey is used only for a first-boot mesh join when no state exists.
	authKey string
}

// resolveIngressOptions turns flags + CITADEL_INGRESS_* env into the concrete
// production dependency set (real routes HTTP client, real cert provider, real
// mesh dialer/listener). It NEVER connects to anything -- that is ingressServe's
// job -- so it is safe to call before the mesh precondition.
func resolveIngressOptions(_ context.Context) (ingressOptions, error) {
	routesURL := flagOrEnv(ingressRoutesURL, "CITADEL_INGRESS_ROUTES_URL")
	appsDomain := strings.ToLower(flagOrEnv(ingressAppsDomain, "CITADEL_INGRESS_APPS_DOMAIN"))
	token := os.Getenv("CITADEL_INGRESS_ROUTES_TOKEN")
	listenAddr := flagOrEnvDefault(ingressListenAddr, "CITADEL_INGRESS_LISTEN", ":443")

	if routesURL == "" {
		return ingressOptions{}, errors.New("ingress: --routes-url (or CITADEL_INGRESS_ROUTES_URL) is required")
	}
	if appsDomain == "" {
		return ingressOptions{}, errors.New("ingress: --apps-domain (or CITADEL_INGRESS_APPS_DOMAIN) is required")
	}

	poll := ingressPollInterval
	if poll <= 0 {
		if v := os.Getenv("CITADEL_INGRESS_POLL_INTERVAL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				poll = d
			}
		}
	}

	cert, err := resolveIngressCertProvider(appsDomain)
	if err != nil {
		return ingressOptions{}, err
	}

	src := ingress.NewHTTPRoutesSource(routesURL, token, nil)
	routes := ingress.NewClient(ingress.ClientConfig{
		Source:       src,
		PollInterval: poll,
		Logf:         Log,
	})

	authzURL, err := ingress.DeriveAuthzURL(routesURL)
	if err != nil {
		return ingressOptions{}, fmt.Errorf("ingress: cannot derive authz URL from routes URL: %w", err)
	}
	authorizer := ingress.NewHTTPAuthorizer(authzURL, token, nil)

	cookieName := os.Getenv("CITADEL_INGRESS_SESSION_COOKIE")
	var cookieNames []string
	if cookieName != "" {
		cookieNames = []string{cookieName}
	}

	return ingressOptions{
		appsDomain:  appsDomain,
		listenAddr:  listenAddr,
		httpListen:  flagOrEnv(ingressHTTPListen, "CITADEL_INGRESS_HTTP_LISTEN"),
		cookieNames: cookieNames,
		routes:      routes,
		authorizer:  authorizer,
		cert:        cert,
		dialContext: ingressDialContext,
		isConnected: ingressIsConnected,
		listen:      ingressListen,
		logf:        Log,
		authKey:     flagOrEnv(ingressAuthKey, "CITADEL_INGRESS_AUTHKEY"),
	}, nil
}

// resolveIngressCertProvider picks the cert provider: a mounted static wildcard
// (recommended v1) when CERT_FILE+KEY_FILE are set, otherwise ACME DNS-01.
func resolveIngressCertProvider(appsDomain string) (ingress.CertProvider, error) {
	certFile := os.Getenv("CITADEL_INGRESS_CERT_FILE")
	keyFile := os.Getenv("CITADEL_INGRESS_KEY_FILE")
	if certFile != "" && keyFile != "" {
		return &ingress.StaticFileCertProvider{CertFile: certFile, KeyFile: keyFile}, nil
	}

	dnsZone := os.Getenv("CITADEL_INGRESS_ACME_DNS_ZONE")
	if dnsZone == "" {
		return nil, errors.New("ingress: no TLS material configured -- set CITADEL_INGRESS_CERT_FILE+CITADEL_INGRESS_KEY_FILE (mounted wildcard) or CITADEL_INGRESS_ACME_DNS_ZONE + RFC2136 credentials (ACME DNS-01)")
	}

	// certmagic cert cache lives next to the tsnet state so it inherits the same
	// state-dir conventions. GetNodeConfigDir is resolved HERE (cmd layer), never
	// inside internal/ingress (CLAUDE.md #787: on a box that also runs a real
	// node it resolves to that node's live dir).
	storageDir := filepath.Join(network.GetNodeConfigDir(), "ingress", "certs")

	return ingress.NewACMECertProvider(ingress.ACMEConfig{
		AppsDomain: appsDomain,
		Email:      flagOrEnv(ingressACMEEmail, "CITADEL_INGRESS_ACME_EMAIL"),
		CA:         flagOrEnv(ingressACMECAURL, "CITADEL_INGRESS_ACME_CA"),
		StorageDir: storageDir,
		DNSZone:    dnsZone,
		RFC2136: ingress.RFC2136Config{
			Server:  os.Getenv("CITADEL_INGRESS_DNS_RFC2136_SERVER"),
			KeyName: os.Getenv("CITADEL_INGRESS_DNS_RFC2136_KEY_NAME"),
			KeyAlg:  os.Getenv("CITADEL_INGRESS_DNS_RFC2136_KEY_ALG"),
			Key:     os.Getenv("CITADEL_INGRESS_DNS_RFC2136_KEY"),
		},
	})
}

// ingressServe is the testable core. It:
//  1. confirms the node is on the mesh via the SAME path `citadel work` uses
//     (VerifyOrReconnect, recoverStaleVPN on ErrStaleState); a not-logged-in
//     node gets a clear, actionable error and no work is done,
//  2. starts the cert provider (blocking until the wildcard is present or
//     failing loudly -- never a self-signed fallback in production),
//  3. starts the routes poller and assembles the proxy + public HTTPS server,
//  4. serves on a listener obtained from opts.listen until a signal arrives,
//     then cancels ctx (graceful shutdown) and returns. It NEVER Logout()s --
//     like `citadel work`, the node identity must persist across restarts.
func ingressServe(
	ctx context.Context,
	cancel context.CancelFunc,
	hasState func() bool,
	verify func(context.Context) (bool, error),
	sigs <-chan os.Signal,
	opts ingressOptions,
) error {
	if !hasState() {
		if opts.authKey != "" {
			// First-boot convenience: join via the SAME network.Connect path
			// `citadel login --authkey` uses (cmd/login.go) -- Hostname +
			// ControlURL (nexusURL, from root.go) + AuthKey. Not a second join
			// implementation. NOTE: this branch is production-only; the test uses
			// hasState=true so it never runs under `go test`.
			if _, err := network.Connect(ctx, network.ServerConfig{
				Hostname:   getWorkHostname(),
				ControlURL: nexusURL,
				AuthKey:    opts.authKey,
			}); err != nil {
				return fmt.Errorf("ingress: first-boot mesh join failed: %w", err)
			}
		} else {
			return errors.New("this node is not connected to the AceTeam Network.\n" +
				"  The ingress is mesh-joined and needs a network identity -- run 'citadel login'\n" +
				"  (or 'citadel init', or pass --authkey) first, then re-run 'citadel ingress'.")
		}
	}

	connected, err := verify(ctx)
	if err != nil && errors.Is(err, network.ErrStaleState) {
		// Production-only path: recoverStaleVPN reads the real node's device
		// config and re-mints a key. The test's verify fake NEVER returns
		// ErrStaleState, so this branch does not run under `go test`.
		Log("network state is stale after retries, attempting auto-recovery...")
		deviceConfig := getDeviceConfigFromFile()
		apiBaseURL := authServiceURL
		if deviceConfig != nil && deviceConfig.APIBaseURL != "" {
			apiBaseURL = deviceConfig.APIBaseURL
		}
		result := recoverStaleVPN(ctx, deviceConfig, getWorkHostname(), apiBaseURL)
		connected = result.Connected
		if !result.Connected {
			msg := "could not restore the AceTeam Network connection automatically"
			if result.Err != nil {
				msg = fmt.Sprintf("%s: %v", msg, result.Err)
			}
			return fmt.Errorf("%s.\n"+
				"  Run 'citadel reconnect' or 'citadel login --authkey <key>' to fix, then re-run.", msg)
		}
	} else if err != nil {
		return fmt.Errorf("ingress: failed to connect to the AceTeam Network: %w", err)
	}

	if !connected || !opts.isConnected() {
		return errors.New("could not establish an AceTeam Network connection.\n" +
			"  Run 'citadel reconnect' or 'citadel login --authkey <key>', then re-run 'citadel ingress'.")
	}

	// Start TLS (blocks until the wildcard is present, or fails loudly).
	fmt.Printf("Starting ingress: obtaining TLS material for %s...\n", opts.appsDomain)
	if err := opts.cert.Start(ctx); err != nil {
		return fmt.Errorf("ingress: TLS startup failed: %w", err)
	}

	// Start the routes poller.
	go opts.routes.Poll(ctx)

	proxy := ingress.NewProxy(ingress.ProxyConfig{
		AppsDomain:         opts.appsDomain,
		Resolver:           opts.routes,
		Authorizer:         opts.authorizer,
		DialContext:        opts.dialContext,
		DefaultCookieNames: opts.cookieNames,
		Logf:               opts.logf,
	})
	server := ingress.NewServer(ingress.ServerConfig{
		AppsDomain:  opts.appsDomain,
		Proxy:       proxy,
		Cert:        opts.cert,
		IsConnected: opts.isConnected,
		RoutesReady: opts.routes.FetchedOnce,
		Logf:        opts.logf,
	})

	ln, err := opts.listen(opts.listenAddr)
	if err != nil {
		return fmt.Errorf("ingress: cannot bind %s: %w", opts.listenAddr, err)
	}

	fmt.Printf("   - Ingress serving on %s for *.%s (Ctrl-C to stop)\n", ln.Addr(), opts.appsDomain)

	// Optional :80 HTTP->HTTPS redirect listener. Bound via the same seam so a
	// test could exercise it; disabled (empty) by default and in tests.
	if opts.httpListen != "" {
		if httpLn, herr := opts.listen(opts.httpListen); herr == nil {
			redirectSrv := &http.Server{
				Handler:           http.HandlerFunc(ingress.RedirectToHTTPS),
				ReadHeaderTimeout: 10 * time.Second,
			}
			go func() { _ = redirectSrv.Serve(httpLn) }()
			go func() {
				<-ctx.Done()
				sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer scancel()
				_ = redirectSrv.Shutdown(sctx)
			}()
		} else {
			opts.logf("[ingress] could not bind HTTP redirect listener %s: %v", opts.httpListen, herr)
		}
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx, ln) }()

	select {
	case <-sigs:
		fmt.Println("\n   - Received shutdown signal; stopping ingress...")
		cancel()
		<-serveErr // wait for graceful shutdown
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("ingress: server error: %w", err)
		}
		return nil
	}
}

// flagOrEnv returns the flag value if non-empty, else the env var.
func flagOrEnv(flagVal, env string) string {
	if strings.TrimSpace(flagVal) != "" {
		return flagVal
	}
	return os.Getenv(env)
}

// flagOrEnvDefault is flagOrEnv with a final fallback default.
func flagOrEnvDefault(flagVal, env, def string) string {
	if v := flagOrEnv(flagVal, env); v != "" {
		return v
	}
	return def
}

func init() {
	ingressCmd.Flags().StringVar(&ingressRoutesURL, "routes-url", "", "Control-plane routes endpoint URL (env: CITADEL_INGRESS_ROUTES_URL)")
	ingressCmd.Flags().StringVar(&ingressAppsDomain, "apps-domain", "", "Apps domain, e.g. apps.example.com (env: CITADEL_INGRESS_APPS_DOMAIN)")
	ingressCmd.Flags().StringVar(&ingressACMEEmail, "acme-email", "", "ACME account contact email (env: CITADEL_INGRESS_ACME_EMAIL)")
	ingressCmd.Flags().StringVar(&ingressACMECAURL, "acme-ca", "", "ACME directory URL; empty = Let's Encrypt production (env: CITADEL_INGRESS_ACME_CA)")
	ingressCmd.Flags().StringVar(&ingressListenAddr, "listen", "", "Public HTTPS bind address, default :443 (env: CITADEL_INGRESS_LISTEN)")
	ingressCmd.Flags().StringVar(&ingressHTTPListen, "http-listen", "", "Optional :80 HTTP->HTTPS redirect bind address (env: CITADEL_INGRESS_HTTP_LISTEN)")
	ingressCmd.Flags().DurationVar(&ingressPollInterval, "poll-interval", 0, "Routes poll cadence, default 10s (env: CITADEL_INGRESS_POLL_INTERVAL)")
	ingressCmd.Flags().StringVar(&ingressAuthKey, "authkey", "", "First-boot mesh join key (env: CITADEL_INGRESS_AUTHKEY)")
	rootCmd.AddCommand(ingressCmd)
}
