// cmd/whatsapp.go
//
// `citadel whatsapp` deploys and manages the WhatsApp (Baileys) bridge as a
// Citadel community module. The bridge is multi-tenant: one container serves
// many tenants and the per-tenant X-API-Key is the tenant selector. This
// command:
//
//   - up:      generate the admin secret (if unset), start the bridge + its
//              Postgres sidecar via docker compose, wait for health, mint a
//              tenant, then print the api_url / api_key / pairing QR.
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	waAPIKeyFlag    string // user-supplied ADMIN_API_KEY override
	waPortFlag      int    // host port to publish the bridge on
	waProxyFlag     string // optional per-tenant egress proxy
	waTenantFlag    string // tenant name (display only)
	waPublicURLFlag string // optional public URL for QR links
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
	whatsappUpCmd.Flags().IntVar(&waPortFlag, "port", whatsapp.DefaultPort, "Host port to publish the bridge on.")
	whatsappUpCmd.Flags().StringVar(&waProxyFlag, "proxy", "",
		"Optional egress proxy for the tenant (socks5:// or http(s)://).")
	whatsappUpCmd.Flags().StringVar(&waTenantFlag, "tenant", "default", "Tenant name (label only).")
	whatsappUpCmd.Flags().StringVar(&waPublicURLFlag, "public-url", "",
		"Optional public base URL of the bridge, used only for copy-pasteable QR links.")
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

// meshAPIURL returns the api_url the aceteam backend should use: the node's
// network (mesh) IP and the published port. Falls back to a placeholder hint
// if the node is not connected to the network.
func meshAPIURL(port int) string {
	ip, err := network.GetGlobalIPv4()
	if err != nil || ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, port)
}

func runWhatsAppUp(cmd *cobra.Command, args []string) error {
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return err
	}

	// 1. Materialize the embedded compose file.
	composePath := whatsapp.ComposePath(servicesDir)
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return fmt.Errorf("create services dir: %w", err)
	}
	if err := os.WriteFile(composePath, []byte(services.WhatsAppBridgeCompose), 0600); err != nil {
		return fmt.Errorf("write compose file: %w", err)
	}

	// 2. Load existing env (preserves a previously generated admin key) and fill
	//    in defaults / overrides.
	env, err := whatsapp.LoadEnv(servicesDir)
	if err != nil {
		return fmt.Errorf("read bridge config: %w", err)
	}

	adminKey := waAPIKeyFlag
	if adminKey == "" {
		adminKey = env["ADMIN_API_KEY"]
	}
	if adminKey == "" {
		adminKey, err = whatsapp.GenerateAdminKey()
		if err != nil {
			return err
		}
		fmt.Println("🔑 Generated a new admin secret for the bridge control plane.")
	}
	env["ADMIN_API_KEY"] = adminKey
	env["BRIDGE_PORT"] = fmt.Sprintf("%d", waPortFlag)
	if waProxyFlag != "" {
		env["DEFAULT_PROXY_URL"] = waProxyFlag
	}
	if waPublicURLFlag != "" {
		env["PUBLIC_URL"] = waPublicURLFlag
	}
	if err := whatsapp.SaveEnv(servicesDir, env); err != nil {
		return fmt.Errorf("write bridge config: %w", err)
	}

	// 3. Start the stack. We invoke compose directly with --env-file so the
	//    generated secret and port are sourced (docker compose does not
	//    auto-load a service-named env file).
	fmt.Printf("--- 🚀 Starting WhatsApp bridge on port %d ---\n", waPortFlag)
	if err := composeUp(composePath, whatsapp.EnvPath(servicesDir)); err != nil {
		return err
	}

	// 4. Register the service in the node manifest for tracking/lifecycle.
	if err := addServiceToManifest(filepath.Dir(servicesDir), whatsapp.ServiceName); err != nil {
		// Non-fatal: the container is up regardless of manifest bookkeeping.
		fmt.Fprintf(os.Stderr, "   ⚠️ Could not record service in manifest: %v\n", err)
	}

	// 5. Wait for the bridge to answer.
	client := whatsapp.NewClient(bridgeBaseURL(waPortFlag), adminKey)
	fmt.Print("   ⏳ Waiting for the bridge to become ready...")
	ctx, cancel := context.WithTimeout(cmd.Context(), 90*time.Second)
	defer cancel()
	if err := client.WaitReady(ctx, 90*time.Second); err != nil {
		fmt.Println()
		return fmt.Errorf("bridge did not become ready: %w\n   Hint: check logs with 'docker logs citadel-whatsapp-bridge'", err)
	}
	fmt.Println(" ready.")

	// 6. Mint a tenant (the data-plane api key for whatsapp_connect).
	tenant, err := client.CreateTenant(ctx, waTenantFlag, waProxyFlag)
	if err != nil {
		return fmt.Errorf("provision tenant: %w", err)
	}
	// Persist the tenant key locally so `status`/`qr`/`connect` can reuse it.
	env["TENANT_API_KEY"] = tenant.APIKey
	env["TENANT_ID"] = tenant.ID
	env["TENANT_NAME"] = tenant.Name
	if err := whatsapp.SaveEnv(servicesDir, env); err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️ Could not save tenant key locally: %v\n", err)
	}

	// 7. Show the connect details + pairing QR.
	printConnectDetails(waPortFlag, tenant.APIKey)
	fmt.Println()
	if err := printQR(ctx, client, tenant.APIKey); err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️ Could not fetch pairing QR yet: %v\n", err)
		fmt.Println("   Run 'citadel whatsapp qr' in a moment to retry.")
	}
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

	running := containerRunning("citadel-whatsapp-bridge")
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

	if api := meshAPIURL(port); api != "" {
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
	dc := exec.Command("docker", "compose", "-f", composePath, "--env-file", whatsapp.EnvPath(servicesDir), "down")
	out, err := dc.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose down failed:\n%s", strings.TrimSpace(string(out)))
	}
	fmt.Println("✅ WhatsApp bridge stopped. Auth state is preserved in the 'whatsapp_pgdata' volume.")
	return nil
}

// composeUp runs `docker compose -f <compose> --env-file <env> up -d`.
func composeUp(composePath, envPath string) error {
	dc := exec.Command("docker", "compose", "-f", composePath, "--env-file", envPath, "up", "-d")
	out, err := dc.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up failed:\n%s\n   Hint: is Docker running? Check with 'docker info'", strings.TrimSpace(string(out)))
	}
	return nil
}

// containerRunning reports whether a named container is in the running state.
func containerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", name).Output()
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

// printConnectDetails prints the api_url + api_key and the whatsapp_connect hint.
func printConnectDetails(port int, apiKey string) {
	bold := color.New(color.Bold)
	fmt.Println()
	bold.Println("Register this bridge in AceTeam:")

	apiURL := meshAPIURL(port)
	if apiURL == "" {
		apiURL = fmt.Sprintf("http://<this-node-network-ip>:%d", port)
		fmt.Println(color.YellowString("  (node is not connected to the AceTeam Network -- substitute its mesh IP below)"))
	}
	fmt.Printf("  api_url:  %s\n", color.CyanString(apiURL))
	fmt.Printf("  api_key:  %s\n", color.CyanString(apiKey))
	fmt.Println()
	fmt.Println("  In AceTeam, call the whatsapp_connect MCP tool with the values above:")
	fmt.Printf("    whatsapp_connect(api_url=%q, api_key=%q)\n", apiURL, apiKey)
	fmt.Println()
	fmt.Println(color.YellowString("  Note: the bridge is on the private mesh, so the AceTeam backend must run with"))
	fmt.Println(color.YellowString("        WHATSAPP_ALLOW_PRIVATE_NETWORK=true to dial it. If you expose the bridge"))
	fmt.Println(color.YellowString("        on a public hostname instead, that backend flag is not needed."))
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
