// cmd/whatsapp.go
//
// `citadel whatsapp` deploys and manages the WhatsApp (Baileys) bridge as a
// Citadel community module. The bridge is multi-tenant: one container serves
// many tenants and the per-tenant X-API-Key is the tenant selector. This
// command:
//
//   - up:      generate the admin secret (if unset), start the bridge + its
//     Postgres sidecar via docker compose, wait for health, mint a
//     tenant, then print the api_url / api_key / pairing QR.
//   - status:  show whether the bridge is running and reachable.
//   - qr:      re-fetch and print the pairing QR for the provisioned tenant.
//   - connect: print the api_url + api_key + the whatsapp_connect hint again.
//   - down:    stop the bridge stack (auth state survives in the named volume).
//
// The data-plane key handed to the aceteam `whatsapp_connect` MCP tool is the
// per-tenant `wab_...` key minted here, NOT the admin secret.
package cmd

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// defaultWhatsAppSource is the default module source for the WhatsApp bridge.
// It points at the maintainer's PERSONAL repo, not an aceteam-ai-owned image:
// the Baileys bridge is a reverse-engineered, ToS-gray integration, so the
// aceteam-ai/citadel-cli binary deliberately does not embed or own its compose
// or container image. The bridge ships from sunapi386/whatsapp-bridge and is
// deployed here via the generic module-source mechanism (catalog.ResolveSource).
const defaultWhatsAppSource = "sunapi386/whatsapp-bridge"

// WhatsApp's gateway-exposure defaults. The WhatsApp bridge manifest lives in the
// sunapi386/whatsapp-bridge repo (not editable here); once it declares a
// gateway: block (prefix/port_env/capability) the registry-driven path uses that
// verbatim. Until then -- and to keep existing deployments working across upgrade
// -- citadel defaults WhatsApp to these values, which match the required manifest
// block documented in the PR:
//
//	gateway:
//	  prefix: whatsapp
//	  port_env: BRIDGE_PORT
//	  capability: provision
const (
	whatsappGatewayPrefix     = "whatsapp"
	whatsappGatewayCapability = "provision"
)

// deployTimeout bounds the whole deploy edge (resolve module source via git clone
// + `docker compose up`). Both underlying steps shell out to git/docker without
// their own deadline, so an unreachable private repo or image registry could
// otherwise hang until the caller (a 180s backend job) gives up with no error.
// This bound guarantees DeployCompose fails fast with a descriptive message
// instead (aceteam-ai/citadel-cli#436 Landmine 2). Image pulls of the bridge +
// postgres over a slow link still need headroom, hence 150s (< the backend's
// 180s job timeout).
const deployTimeout = 150 * time.Second

var (
	waAPIKeyFlag    string // user-supplied ADMIN_API_KEY override
	waPortFlag      int    // host port to publish the bridge on
	waProxyFlag     string // optional per-tenant egress proxy
	waTenantFlag    string // tenant name (display only)
	waPublicURLFlag string // optional public URL for QR links
	waImageFlag     string // optional override for the bridge container image
	waSourceFlag    string // module source for the bridge compose (owner/repo[@ref] or git URL)
)

var whatsappCmd = &cobra.Command{
	Use:   "whatsapp",
	Short: "Deploy and manage the WhatsApp bridge community module",
	Long: `Deploy the multi-tenant WhatsApp (Baileys) bridge as a Citadel-managed
community module and link a phone by scanning a pairing QR.

The bridge runs as a two-container stack (bridge + embedded Postgres for
Baileys auth state) on this node and is reachable by the AceTeam backend over
the secure mesh network at http://<node-network-ip>:<port>.

Typical flow:
  citadel whatsapp up           # deploy + start + mint a tenant + show QR
  # scan the QR with WhatsApp -> Linked Devices -> Link a Device
  # then register it from aceteam with the whatsapp_connect MCP tool`,
}

var whatsappUpCmd = &cobra.Command{
	Use:   "up",
	Short: "Deploy, start, and provision the WhatsApp bridge",
	RunE:  runWhatsAppUp,
}

var whatsappStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the WhatsApp bridge status",
	RunE:  runWhatsAppStatus,
}

var whatsappQRCmd = &cobra.Command{
	Use:   "qr",
	Short: "Print the pairing QR for the provisioned tenant",
	RunE:  runWhatsAppQR,
}

var whatsappConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Print the api_url + api_key to register via whatsapp_connect",
	RunE:  runWhatsAppConnect,
}

var whatsappDownCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop the WhatsApp bridge (auth state is preserved)",
	RunE:  runWhatsAppDown,
}

func init() {
	rootCmd.AddCommand(whatsappCmd)
	whatsappCmd.AddCommand(whatsappUpCmd)
	whatsappCmd.AddCommand(whatsappStatusCmd)
	whatsappCmd.AddCommand(whatsappQRCmd)
	whatsappCmd.AddCommand(whatsappConnectCmd)
	whatsappCmd.AddCommand(whatsappDownCmd)

	whatsappUpCmd.Flags().StringVar(&waAPIKeyFlag, "api-key", "",
		"Admin secret (ADMIN_API_KEY) for the bridge control plane. A strong one is generated if omitted.")
	whatsappUpCmd.Flags().IntVar(&waPortFlag, "port", 0,
		"Host port to publish the bridge on. Default 0 auto-selects a free port "+
			"(8080 is held by the citadel agent on a live node). Pass a value to pin one.")
	whatsappUpCmd.Flags().StringVar(&waProxyFlag, "proxy", "",
		"Optional egress proxy for the tenant (socks5:// or http(s)://).")
	whatsappUpCmd.Flags().StringVar(&waTenantFlag, "tenant", "default", "Tenant name (label only).")
	whatsappUpCmd.Flags().StringVar(&waPublicURLFlag, "public-url", "",
		"Optional public base URL of the bridge, used only for copy-pasteable QR links.")
	whatsappUpCmd.Flags().StringVar(&waImageFlag, "image", "",
		"Override the bridge container image (BRIDGE_IMAGE). Defaults to the image declared by the module source.")
	whatsappUpCmd.Flags().StringVar(&waSourceFlag, "source", defaultWhatsAppSource,
		"Module source for the bridge compose: owner/repo[@ref] or a git URL. "+
			"The default (sunapi386/whatsapp-bridge) is a PRIVATE repo, so this node needs "+
			"git credentials (GITHUB_TOKEN or an SSH key) to clone it and a docker login to pull its private image.")
}

// servicesDirForNode resolves the node's services directory, creating the node
// config skeleton if needed.
func servicesDirForNode() (string, error) {
	_, configDir, err := findOrCreateManifest()
	if err != nil {
		return "", fmt.Errorf("initialize node configuration: %w", err)
	}
	return filepath.Join(configDir, "services"), nil
}

// bridgeBaseURL returns the loopback base URL the CLI uses to talk to the
// locally running bridge (the CLI runs on the same host as the container).
func bridgeBaseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// The api_url the aceteam backend uses to reach a provisioned bridge is now the
// gateway-route mesh URL (whatsappMeshAPIURL in cmd/provisioned_gateway.go), not
// a raw http://<vpn-ip>:<bridge-port> that nothing on the tsnet stack listens on
// (aceteam-ai/citadel-cli#447).

// deployWhatsAppCompose resolves the bridge module source (git clone of the
// private repo), materializes its compose into the node's services dir, and
// starts the stack with `docker compose up -d --env-file`. It is the effectful
// deployment edge injected into whatsapp.Provision. source/image are captured
// from the caller (the CLI flags or the handler's defaults).
//
// GIT_TERMINAL_PROMPT=0 is set so a private-repo clone with missing credentials
// fails fast with a clear error instead of blocking on an interactive prompt
// (which would hang a headless node job).
// deployReport carries the NON-FATAL outcomes of a deploy back out to
// whatsappProvisionDeps, which exposes them to whatsapp.Provision through the
// optional ImagePullError dep (the DeployCompose dep itself returns only an
// error, and its signature is shared with every existing caller and test stub).
//
// Today it carries one thing: the image-pull error. A pull failure must not fail
// the provision -- a node that never ran `docker login` for the private registry
// can still serve from its cached image, and failing would regress re-provisions
// that work today -- but it must not vanish either, because "provisioned fine,
// silently still on the old image" is exactly the false-green #718 is about.
//
// The mutex is load-bearing: deployWhatsAppCompose runs the deploy in a goroutine
// and abandons it on deployTimeout, so the writer can still be running when the
// reader looks.
type deployReport struct {
	mu      sync.Mutex
	pullErr string
}

func (r *deployReport) setPullError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.pullErr = ""
		return
	}
	r.pullErr = err.Error()
}

// PullError returns the recorded pull error text ("" when the pull succeeded or
// was never attempted). It is the whatsapp.ProvisionDeps.ImagePullError edge.
func (r *deployReport) PullError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pullErr
}

func deployWhatsAppCompose(source, image string, report *deployReport) func(servicesDir string, env map[string]string) error {
	return func(servicesDir string, env map[string]string) error {
		// Fail fast on a private-repo clone with missing credentials instead of
		// blocking on an interactive git prompt (which would hang a headless job).
		if os.Getenv("GIT_TERMINAL_PROMPT") == "" {
			_ = os.Setenv("GIT_TERMINAL_PROMPT", "0")
		}
		// Bound git's own TCP connect+overall time so an UNREACHABLE host (not just
		// a missing credential) can't wedge the clone. catalog.ResolveSource shells
		// out to `git` without a context, so this env var is how we cap it; it pairs
		// with the wall-clock deployTimeout below as defense in depth.
		if os.Getenv("GIT_HTTP_LOW_SPEED_LIMIT") == "" {
			_ = os.Setenv("GIT_HTTP_LOW_SPEED_LIMIT", "1000")
			_ = os.Setenv("GIT_HTTP_LOW_SPEED_TIME", "30")
		}

		// Run the whole effectful edge (resolve source via git clone + docker
		// compose up) under a hard deadline so a missing-creds / unreachable-registry
		// deploy reports a bounded, descriptive error rather than hanging until the
		// caller's own timeout (aceteam-ai/citadel-cli#436 Landmine 2).
		ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
		defer cancel()

		type result struct{ err error }
		done := make(chan result, 1)
		go func() {
			done <- result{err: deployWhatsAppComposeOnce(ctx, source, image, servicesDir, env, report)}
		}()

		select {
		case r := <-done:
			return r.err
		case <-ctx.Done():
			// The git/docker subprocess may still be draining in the background, but
			// the caller gets a prompt, actionable error instead of a silent hang.
			return fmt.Errorf("deploying the WhatsApp bridge timed out after %s: the private module repo or its image registry is unreachable, or Docker is not responding. Check network/credentials (GITHUB_TOKEN or SSH, and `docker login`) and that `docker info` works", deployTimeout)
		}
	}
}

// deployWhatsAppComposeOnce performs the actual resolve + write + compose-up. It
// is split out so deployWhatsAppCompose can run it under a hard deadline.
func deployWhatsAppComposeOnce(ctx context.Context, source, image, servicesDir string, env map[string]string, report *deployReport) error {
	if source == "" {
		source = defaultWhatsAppSource
	}
	src, err := catalog.ParseSource(source)
	if err != nil {
		return fmt.Errorf("invalid module source %q: %w", source, err)
	}
	resolved, err := catalog.ResolveSource(src)
	if err != nil {
		// ResolveSource's clone error already explains the private-repo
		// credential requirement (GITHUB_TOKEN / SSH / docker login).
		return fmt.Errorf("resolve WhatsApp bridge module (needs Docker + private-repo git credentials): %w", err)
	}
	composeBytes, err := os.ReadFile(resolved.ComposePath)
	if err != nil {
		return fmt.Errorf("read resolved bridge compose %s: %w", resolved.ComposePath, err)
	}
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return fmt.Errorf("create services dir: %w", err)
	}
	composePath := whatsapp.ComposePath(servicesDir)
	if err := os.WriteFile(composePath, composeBytes, 0600); err != nil {
		return fmt.Errorf("write compose file: %w", err)
	}
	if image != "" {
		env["BRIDGE_IMAGE"] = image
	}
	// Persist env before compose pull/up so --env-file sees the admin key + port.
	// The compose declares `ADMIN_API_KEY: ${ADMIN_API_KEY:?...}`, so even `pull`
	// (which parses the file) fails without it.
	if err := whatsapp.SaveEnv(servicesDir, env); err != nil {
		return fmt.Errorf("write bridge config: %w", err)
	}
	return startBridgeStack(ctx, whatsapp.ProjectName(servicesDir), composePath, whatsapp.EnvPath(servicesDir), report)
}

// startBridgeStack refreshes the bridge image and then starts the stack. Pull
// THEN up, in that order, is the fix for aceteam-ai/citadel-cli#718: the compose
// pins the floating tag `ghcr.io/sunapi386/whatsapp-bridge:latest`, and when that
// tag is already present locally a bare `up -d` sees an unchanged config and an
// unchanged image ID and does nothing -- so a provisioned bridge could never be
// upgraded through the API, while `Provision` still reported success.
//
// A pull failure is recorded and logged but NOT fatal (see deployReport): the
// cached image still runs, and the caller learns the truth from
// ProvisionResult.ImagePullError plus the unchanged image IDs.
func startBridgeStack(ctx context.Context, project, composePath, envPath string, report *deployReport) error {
	pullErr := bridgeComposePull(ctx, project, composePath, envPath)
	if report != nil {
		report.setPullError(pullErr)
	}
	if pullErr != nil {
		// Loud, but not fatal. Provision surfaces this in its result too.
		fmt.Fprintf(os.Stderr, "   ⚠️ could not refresh the bridge image; starting the locally cached one instead: %v\n", pullErr)
	}
	return bridgeComposeUp(ctx, project, composePath, envPath)
}

// whatsappProvisionDeps builds the ProvisionDeps for the local CLI, wiring the
// real catalog / docker / network edges. source/image are the CLI flag values.
func whatsappProvisionDeps(source, image string) whatsapp.ProvisionDeps {
	// One report per deps set, so the pull outcome recorded by DeployCompose is
	// the one ImagePullError reads back for this provision.
	report := &deployReport{}
	return whatsapp.ProvisionDeps{
		ServicesDir:   servicesDirForNode,
		DeployCompose: deployWhatsAppCompose(source, image, report),
		// Image identity before/after the deploy, so an `already_linked` result
		// can be told apart from a real upgrade (#718).
		BridgeImageID:  bridgeImageIDForNode,
		ImagePullError: report.PullError,
		NewBridgeClient: func(port int, adminKey string) whatsapp.BridgeClient {
			return whatsapp.NewClient(bridgeBaseURL(port), adminKey)
		},
		// The bridge is reached by the backend through the gateway route on the
		// mesh (not a raw host port nothing listens on) -- aceteam-ai/citadel-cli
		// #447. whatsappMeshAPIURL returns the gateway-route URL,
		// exposeWhatsAppGatewayRoute wires that route to the bridge's host port,
		// and verifyBridgeReachable fails loud if the end-to-end mesh path is not
		// actually reachable (instead of a false-green).
		MeshAPIURL:         whatsappMeshAPIURL,
		ExposeGatewayRoute: exposeWhatsAppGatewayRoute,
		VerifyReachable:    verifyBridgeReachable,
		// Hand the backend the gateway cert to trust (the api_url is an https
		// gateway route) plus the plaintext URL to re-fetch it from on rotation.
		GatewayCertPEM: gatewayCertPEM,
		CertRefreshURL: certRefreshURL,
		Log: func(format string, args ...any) {
			fmt.Printf("   - "+format+"\n", args...)
		},
	}
}

func runWhatsAppUp(cmd *cobra.Command, args []string) error {
	// The bridge is deliberately NOT added to the node manifest. The generic
	// `citadel run`/`citadel work` start path runs `docker compose up` without
	// an --env-file, but this stack hard-requires ADMIN_API_KEY (and docker
	// compose only auto-loads a file literally named ".env"). Registering it
	// would make those commands fail. The bridge has its own lifecycle via
	// `citadel whatsapp up/down`; status discovery uses the compose-file
	// presence + container state, not the manifest.
	if waPortFlag > 0 {
		fmt.Printf("--- 🚀 Provisioning WhatsApp bridge on port %d ---\n", waPortFlag)
	} else {
		fmt.Println("--- 🚀 Provisioning WhatsApp bridge (auto-selecting a free host port) ---")
	}

	deps := whatsappProvisionDeps(waSourceFlag, waImageFlag)
	// The CLI also honors an explicit --api-key admin override. Provision reads
	// the stored/admin key itself, so seed it into the env file first if the
	// operator supplied one.
	if waAPIKeyFlag != "" {
		if servicesDir, err := servicesDirForNode(); err == nil {
			env, _ := whatsapp.LoadEnv(servicesDir)
			env["ADMIN_API_KEY"] = waAPIKeyFlag
			_ = whatsapp.SaveEnv(servicesDir, env)
		}
	}

	res, err := whatsapp.Provision(cmd.Context(), whatsapp.ProvisionRequest{
		Tenant:    waTenantFlag,
		Proxy:     waProxyFlag,
		PublicURL: waPublicURLFlag,
		Port:      waPortFlag,
	}, deps)
	if err != nil {
		return err
	}

	// Say what the deploy actually did to the image. A re-run that changed
	// nothing must not look like an upgrade (#718).
	printImageOutcome(res)

	// Show the connect details + pairing QR. Use the port Provision actually
	// published on (res.Port) -- with auto-selection waPortFlag is 0.
	printConnectDetails(res.Port, res.APIKey)
	fmt.Println()
	if res.AlreadyLinked {
		fmt.Println("✅ This tenant is already linked to WhatsApp -- no QR needed.")
		return nil
	}
	if res.QR == "" {
		fmt.Fprintln(os.Stderr, "   ⚠️ Could not fetch the pairing QR yet.")
		fmt.Println("   Run 'citadel whatsapp qr' in a moment to retry.")
		return nil
	}
	bold := color.New(color.Bold)
	bold.Println("Scan this QR to link WhatsApp:")
	fmt.Println("  (WhatsApp -> Settings -> Linked Devices -> Link a Device)")
	fmt.Println()
	fmt.Println(whatsapp.RenderQRANSI(res.QR))
	return nil
}

func runWhatsAppStatus(cmd *cobra.Command, args []string) error {
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return err
	}
	if !whatsapp.IsDeployed(servicesDir) {
		fmt.Println("WhatsApp bridge is not deployed.")
		fmt.Println("   Hint: run 'citadel whatsapp up' to deploy it.")
		return nil
	}
	env, _ := whatsapp.LoadEnv(servicesDir)
	port := portFromEnv(env)

	bold := color.New(color.Bold)
	bold.Println("WhatsApp bridge")

	running := bridgeContainerRunning(whatsapp.ProjectName(servicesDir))
	if running {
		fmt.Printf("  Container: %s\n", color.GreenString("running"))
	} else {
		fmt.Printf("  Container: %s\n", color.YellowString("stopped"))
		fmt.Println("   Hint: run 'citadel whatsapp up' to (re)start it.")
		return nil
	}

	client := whatsapp.NewClient(bridgeBaseURL(port), env["ADMIN_API_KEY"])
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	if err := client.Root(ctx); err != nil {
		fmt.Printf("  Reachable: %s (%v)\n", color.YellowString("no"), err)
		return nil
	}
	fmt.Printf("  Reachable: %s\n", color.GreenString("yes"))

	if key := env["TENANT_API_KEY"]; key != "" {
		if h, err := client.Health(ctx, key); err == nil {
			loggedIn := color.YellowString("not linked (scan QR)")
			if h.LoggedIn {
				loggedIn = color.GreenString("linked")
			}
			fmt.Printf("  WhatsApp:  %s\n", loggedIn)
		}
	}

	if api := whatsappMeshAPIURL(port); api != "" {
		fmt.Printf("  api_url:   %s\n", api)
	}
	return nil
}

func runWhatsAppQR(cmd *cobra.Command, args []string) error {
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return err
	}
	env, _ := whatsapp.LoadEnv(servicesDir)
	key := env["TENANT_API_KEY"]
	if key == "" {
		return fmt.Errorf("no provisioned tenant found; run 'citadel whatsapp up' first")
	}
	port := portFromEnv(env)
	client := whatsapp.NewClient(bridgeBaseURL(port), env["ADMIN_API_KEY"])
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	return printQR(ctx, client, key)
}

func runWhatsAppConnect(cmd *cobra.Command, args []string) error {
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return err
	}
	env, _ := whatsapp.LoadEnv(servicesDir)
	key := env["TENANT_API_KEY"]
	if key == "" {
		return fmt.Errorf("no provisioned tenant found; run 'citadel whatsapp up' first")
	}
	printConnectDetails(portFromEnv(env), key)
	return nil
}

func runWhatsAppDown(cmd *cobra.Command, args []string) error {
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return err
	}
	composePath := whatsapp.ComposePath(servicesDir)
	if !whatsapp.IsDeployed(servicesDir) {
		fmt.Println("WhatsApp bridge is not deployed; nothing to stop.")
		return nil
	}
	fmt.Println("--- 🛑 Stopping WhatsApp bridge ---")
	// Must use the SAME `-p` project the stack was brought up under, otherwise
	// compose targets a different (empty) project and the containers survive.
	// Left UNSCOPED (no service argument) on purpose: `down` should take the
	// Postgres sidecar with it, and compose limits `down` to the services of the
	// loaded -f file, so the sibling modules sharing this project are untouched.
	out, err := runBridgeCompose(context.Background(),
		bridgeComposeArgs(whatsapp.ProjectName(servicesDir), composePath, whatsapp.EnvPath(servicesDir), "down")...)
	if err != nil {
		return fmt.Errorf("docker compose down failed:\n%s", strings.TrimSpace(string(out)))
	}
	fmt.Println("✅ WhatsApp bridge stopped. Auth state is preserved in the 'whatsapp_pgdata' volume.")
	return nil
}

// bridgeComposeUp runs `docker compose -p <project> -f <compose> --env-file <env>
// up -d bridge`.
//
// The explicit `-p <project>` (whatsapp.ProjectName(servicesDir)) is load-bearing:
// the module compose no longer hardcodes `container_name`, so compose derives each
// container's name from the project (`<project>-<service>-<index>`). Pinning the
// project explicitly (rather than relying on compose's implicit default) keeps
// up/down and the status check in agreement, and is the exact project the status
// check queries with `docker compose -p <project> ps`. The value equals the old
// implicit default (the services-dir basename) so existing volumes are preserved.
//
// It runs under ctx so a wedged `docker compose up` (e.g. pulling an image from an
// unreachable registry) fails fast with a bounded, descriptive error instead of
// hanging the caller (aceteam-ai/citadel-cli#436 Landmine 2).
func bridgeComposeUp(ctx context.Context, project, composePath, envPath string) error {
	// Scope to the bridge service. `db` still comes up: the compose declares
	// `depends_on: db: {condition: service_healthy}`, so compose starts and waits
	// on it. Naming the service keeps this invocation from ever acting on the
	// sibling modules that share the `services` compose project.
	out, err := runBridgeCompose(ctx, bridgeComposeArgs(project, composePath, envPath,
		"up", "-d", whatsapp.BridgeService)...)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("docker compose up timed out after %s (is the image registry reachable and is Docker running? check with 'docker info'):\n%s",
				deployTimeout, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("docker compose up failed:\n%s\n   Hint: is Docker running? Check with 'docker info'", strings.TrimSpace(string(out)))
	}
	return nil
}

// bridgeComposePull refreshes the bridge image from the registry (`docker compose
// ... pull bridge`), the step whose absence made an upgrade impossible (#718).
//
// It is scoped to the `bridge` service and deliberately does NOT pass
// `--include-deps`: an unscoped pull would also refresh `postgres:16-alpine`, and
// if that tag has moved the following `up` recreates the Postgres sidecar that
// holds the Baileys auth state. That is gratuitous risk for a bridge upgrade.
//
// `--ignore-pull-failures` is likewise deliberately NOT used: it makes compose
// exit 0 on a failed pull, which would destroy the very signal this change
// exists to surface. The failure is handled by the caller instead
// (startBridgeStack), which records it and proceeds with the cached image.
func bridgeComposePull(ctx context.Context, project, composePath, envPath string) error {
	out, err := runBridgeCompose(ctx, bridgeComposeArgs(project, composePath, envPath,
		"pull", whatsapp.BridgeService)...)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("docker compose pull timed out after %s (is the image registry reachable? check with 'docker info' and, for the private bridge image, 'docker login ghcr.io'):\n%s",
				deployTimeout, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("docker compose pull failed (is the image registry reachable and is this node logged in? the bridge image is private -- try 'docker login ghcr.io'):\n%s",
			strings.TrimSpace(string(out)))
	}
	return nil
}

// bridgeComposeArgs builds the argv for a docker compose invocation against the bridge
// stack. Every bridge compose call must carry the same `-p <project> -f <compose>
// --env-file <env>` triple: the project keeps up/pull/ps/down in agreement, and
// the env file carries ADMIN_API_KEY, which the compose file interpolates with
// `:?` (so even `pull` fails to parse without it).
func bridgeComposeArgs(project, composePath, envPath string, rest ...string) []string {
	args := []string{"compose", "-p", project, "-f", composePath, "--env-file", envPath}
	return append(args, rest...)
}

// bridgeComposeEnv is the environment for bridge compose subprocesses. It does
// NOT reuse cmd/service.go's composeEnv: that one injects the citadel-owned
// host-port and workspace vars the embedded inference composes guard with `:?`,
// none of which the bridge compose declares.
//
// COMPOSE_IGNORE_ORPHANS silences the orphan warning. The bridge shares the
// `services` compose project with every other module on the node (citadel-tei,
// citadel-unlimited-ocr, and friends, each from its own sibling .yml), and
// compose computes orphans from the services of the LOADED file -- so naming the
// `bridge` service on the command line does not suppress it. Never pass
// `--remove-orphans` here: in this shared project it would delete those modules.
func bridgeComposeEnv() []string {
	return append(os.Environ(), "COMPOSE_IGNORE_ORPHANS=true")
}

// runBridgeCompose executes `docker <args...>` and returns its combined output.
// It is a package-level var (the same seam as internal/catalog's
// runCosignVerify) so tests can assert the exact argv -- including that a pull
// precedes the up -- without a Docker daemon.
var runBridgeCompose = func(ctx context.Context, args ...string) ([]byte, error) {
	dc := exec.CommandContext(ctx, "docker", args...)
	dc.Env = bridgeComposeEnv()
	return dc.CombinedOutput()
}

// bridgeContainerRunning reports whether the bridge app container of the given
// compose project is in the running state. It resolves the container by compose
// project (NOT a hardcoded name), so it stays correct now that the module compose
// derives `<project>-bridge-<index>` names. Best-effort: false if Docker is
// unavailable or no such container exists.
//
// It uses `docker compose -p <project> ps` scoped to the bridge service, then
// confirms the state via `docker inspect` so a "created but not running"
// container reads as stopped (matching the old inspect-based semantics).
func bridgeContainerRunning(project string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := bridgeContainerID(ctx, project)
	if err != nil || id == "" {
		return false
	}
	return containerRunning(id)
}

// bridgeContainerID returns the container ID of the bridge app service in the
// given compose project, or "" if the stack is not up. It asks compose to list
// the `bridge` service's container id(s); `-q` prints ids of containers for the
// project (running ones), so an empty result means the stack is down.
func bridgeContainerID(ctx context.Context, project string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "compose", "-p", project,
		"ps", "-q", whatsapp.BridgeService).Output()
	if err != nil {
		return "", err
	}
	// There is a single bridge replica; take the first non-empty line.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			return id, nil
		}
	}
	return "", nil
}

// bridgeImageIDForNode reports the docker image ID (sha256:...) the bridge
// container is CURRENTLY running, or "" when the bridge is not running, Docker
// is unavailable, or the node config cannot be resolved.
//
// It reads the RUNNING CONTAINER's image (`docker inspect {{.Image}}`), NOT the
// local `:latest` tag. That distinction is the whole point: after a pull the tag
// already resolves to the new image while the container keeps running the old one
// until compose recreates it, so a tag-derived value would claim an upgrade that
// never happened (aceteam-ai/citadel-cli#718).
//
// Best-effort by contract: every failure returns "", which Provision reads as
// "unknown" and never as "upgraded".
func bridgeImageIDForNode() string {
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := bridgeContainerID(ctx, whatsapp.ProjectName(servicesDir))
	if err != nil || id == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Image}}", id).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// shortImageID trims a docker image ID to the 12-hex-char form docker itself
// prints, for human-facing output. Non-sha256 or short values pass through.
func shortImageID(id string) string {
	trimmed := strings.TrimPrefix(id, "sha256:")
	if len(trimmed) > 12 {
		return trimmed[:12]
	}
	return trimmed
}

// containerRunning reports whether a container (by name or ID) is in the running
// state.
func containerRunning(nameOrID string) bool {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", nameOrID).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "running"
}

// portFromEnv extracts the configured bridge port, defaulting to DefaultPort.
func portFromEnv(env map[string]string) int {
	if v := env["BRIDGE_PORT"]; v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			return p
		}
	}
	return whatsapp.DefaultPort
}

// printImageOutcome reports whether the deploy actually moved the bridge onto a
// new image. Before #718 a re-run on a stale image was indistinguishable from a
// real upgrade -- both printed the same success -- so the no-op case is stated
// explicitly rather than left silent.
func printImageOutcome(res *whatsapp.ProvisionResult) {
	switch {
	case res.Upgraded:
		fmt.Printf("   - bridge image upgraded: %s -> %s\n",
			shortImageID(res.ImageIDBefore), shortImageID(res.ImageIDAfter))
	case res.ImagePullError != "":
		fmt.Fprintf(os.Stderr, "   ⚠️ bridge image NOT refreshed (%s); it is running the locally cached image %s\n",
			res.ImagePullError, shortImageID(res.ImageIDAfter))
	case res.ImageIDBefore != "" && res.ImageIDAfter != "":
		fmt.Printf("   - bridge image unchanged (%s); already up to date\n", shortImageID(res.ImageIDAfter))
	}
}

// printConnectDetails prints the api_url + api_key and the whatsapp_connect hint.
func printConnectDetails(port int, apiKey string) {
	bold := color.New(color.Bold)
	fmt.Println()
	bold.Println("Register this bridge in AceTeam:")

	apiURL := whatsappMeshAPIURL(port)
	if apiURL == "" {
		gf := gatewayFactsForURL()
		gwPort := gf.Port
		scheme := "https"
		if !gf.UseTLS {
			scheme = "http"
		}
		apiURL = fmt.Sprintf("%s://<this-node-network-ip>:%d%s", scheme, gwPort, gateway.ModuleRoutePath(whatsappGatewayPrefix))
		fmt.Println(color.YellowString("  (node is not connected to the AceTeam Network -- substitute its mesh IP below)"))
	}
	fmt.Printf("  api_url:  %s\n", color.CyanString(apiURL))
	fmt.Printf("  api_key:  %s\n", color.CyanString(apiKey))
	fmt.Println()
	fmt.Println("  In AceTeam, call the whatsapp_connect MCP tool with the values above:")
	fmt.Printf("    whatsapp_connect(api_url=%q, api_key=%q)\n", apiURL, apiKey)
	fmt.Println()
	if whatsappNonMeshPrivateHost(apiURL) {
		fmt.Println(color.YellowString("  Note: this api_url is a non-mesh private host, so the AceTeam backend must"))
		fmt.Println(color.YellowString("        run with WHATSAPP_ALLOW_PRIVATE_NETWORK=true to dial it. The flag is"))
		fmt.Println(color.YellowString("        only for non-mesh private hosts, not for the mesh."))
	} else {
		fmt.Println("  The bridge is reachable over the mesh by default; no backend flag is needed.")
	}
}

// whatsappNonMeshPrivateHost reports whether the host in apiURL is a private
// RFC1918 address that is NOT on the mesh (100.64.0.0/10, CGNAT). The aceteam
// backend's SSRF guard allows the mesh range by default, so only a non-mesh
// private host needs WHATSAPP_ALLOW_PRIVATE_NETWORK=true. Unparseable hosts
// (empty, hostname, or the off-mesh placeholder) count as non-private, so no
// flag note is shown.
func whatsappNonMeshPrivateHost(apiURL string) bool {
	if apiURL == "" {
		return false
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return false
	}
	if _, mesh, err := net.ParseCIDR("100.64.0.0/10"); err == nil && mesh.Contains(ip) {
		return false
	}
	return ip.IsPrivate()
}

// buildWhatsAppCallbacks wires the WhatsApp page's lifecycle hooks to the same
// node-config + bridge-client logic the CLI uses, so the TUI and CLI stay in
// lockstep. All hooks are best-effort and never panic.
func buildWhatsAppCallbacks() controlcenter.WhatsAppCallbacks {
	return controlcenter.WhatsAppCallbacks{
		Status: func() controlcenter.WhatsAppStatus {
			st := controlcenter.WhatsAppStatus{}
			servicesDir, err := servicesDirForNode()
			if err != nil {
				st.Err = err.Error()
				return st
			}
			st.Deployed = whatsapp.IsDeployed(servicesDir)
			if !st.Deployed {
				return st
			}
			env, _ := whatsapp.LoadEnv(servicesDir)
			port := portFromEnv(env)
			st.APIKey = env["TENANT_API_KEY"]
			st.APIURL = whatsappMeshAPIURL(port)
			st.Running = bridgeContainerRunning(whatsapp.ProjectName(servicesDir))
			if !st.Running {
				return st
			}
			client := whatsapp.NewClient(bridgeBaseURL(port), env["ADMIN_API_KEY"])
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := client.Root(ctx); err != nil {
				return st
			}
			st.Reachable = true
			if st.APIKey != "" {
				if h, err := client.Health(ctx, st.APIKey); err == nil {
					st.LoggedIn = h.LoggedIn
				}
			}
			return st
		},
		Deploy: func() (string, string, error) {
			// Reset deploy flags to their declared defaults so a TUI-driven
			// deploy is deterministic regardless of any prior CLI invocation.
			waAPIKeyFlag = ""
			waPortFlag = 0 // auto-select a free host port (issue #438)
			waProxyFlag = ""
			waTenantFlag = "default"
			waPublicURLFlag = ""
			waSourceFlag = defaultWhatsAppSource

			if err := runWhatsAppUp(whatsappUpCmd, nil); err != nil {
				return "", "", err
			}
			servicesDir, err := servicesDirForNode()
			if err != nil {
				return "", "", err
			}
			env, _ := whatsapp.LoadEnv(servicesDir)
			return whatsappMeshAPIURL(portFromEnv(env)), env["TENANT_API_KEY"], nil
		},
		Stop: func() error {
			return runWhatsAppDown(whatsappDownCmd, nil)
		},
		QRBlocks: func() (string, error) {
			servicesDir, err := servicesDirForNode()
			if err != nil {
				return "", err
			}
			env, _ := whatsapp.LoadEnv(servicesDir)
			key := env["TENANT_API_KEY"]
			if key == "" {
				return "", fmt.Errorf("no provisioned tenant")
			}
			port := portFromEnv(env)
			client := whatsapp.NewClient(bridgeBaseURL(port), env["ADMIN_API_KEY"])
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			payload, err := client.QRString(ctx, key)
			if err != nil {
				return "", err
			}
			return whatsapp.RenderQRBlocks(payload), nil
		},
	}
}

// printQR fetches and renders the pairing QR for a tenant.
func printQR(ctx context.Context, client *whatsapp.Client, apiKey string) error {
	payload, err := client.QRString(ctx, apiKey)
	if err != nil {
		return err
	}
	if payload == "" {
		fmt.Println("✅ This tenant is already linked to WhatsApp -- no QR needed.")
		return nil
	}
	bold := color.New(color.Bold)
	bold.Println("Scan this QR to link WhatsApp:")
	fmt.Println("  (WhatsApp -> Settings -> Linked Devices -> Link a Device)")
	fmt.Println()
	fmt.Println(whatsapp.RenderQRANSI(payload))
	return nil
}
