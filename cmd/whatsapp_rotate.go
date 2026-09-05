// cmd/whatsapp_rotate.go
//
// `citadel whatsapp rotate-key` and the shared rotation wiring (citadel#624
// part 3). Rotating the bridge's ADMIN_API_KEY is ONE implementation
// (whatsapp.RotateAdminKey) reached from two entry points: this operator CLI
// command, and the WHATSAPP_PROVISION job's `rotate_admin_key: true` flag
// (internal/worker/whatsapp_provision.go, wired in cmd/nodejobs.go). Both share
// whatsappRotateDeps below so they can never diverge on how they resolve the
// env file, recreate the bridge, or verify the new key.
//
// Machine-convergence: rotation resolves the services dir through
// servicesDirForNodeRead (findAndReadManifest, a READ-only path), never
// findOrCreateManifest. On any node where the bridge has actually been deployed
// (the only node where rotation is valid), findOrCreateManifest returns exactly
// what findAndReadManifest returns -- it only bootstraps when the read fails --
// so the CLI (this file) and the job path (whose Provision deps resolve via the
// create-capable servicesDirForNode) converge on the SAME env file. Rotation
// deliberately never bootstraps: rotating on a node that has not deployed the
// bridge is an error, not a reason to create an empty node skeleton.
package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var waRotateDryRun bool

var whatsappRotateKeyCmd = &cobra.Command{
	Use:   "rotate-key",
	Short: "Rotate the WhatsApp bridge admin secret (ADMIN_API_KEY)",
	Long: `Generate a fresh admin secret for the bridge control plane, atomically
rewrite its 0600 env file (preserving the tenant keys and every other setting),
recreate the bridge so it picks up the new key, and verify the new key
authenticates against the bridge's admin API.

Only the ADMIN key is rotated. The per-tenant data-plane keys the AceTeam
platform stores are left untouched, so an already-linked WhatsApp session and
its registered api_key keep working.

The admin key bytes are never printed; only the old and new key fingerprints
(sha256:...) are shown.`,
	RunE: runWhatsAppRotateKey,
}

func init() {
	whatsappCmd.AddCommand(whatsappRotateKeyCmd)
	whatsappRotateKeyCmd.Flags().BoolVar(&waRotateDryRun, "dry-run", false,
		"Show what would rotate (env file path, current fingerprint) without touching anything.")
}

// servicesDirForNodeRead resolves the node's services directory via the
// READ-only manifest path (findAndReadManifest), never create-if-missing. Used
// by rotation so it fails cleanly on a node with no configured manifest rather
// than bootstrapping an empty node skeleton. See this file's package doc for
// why this still converges with the job path's create-capable resolver.
func servicesDirForNodeRead() (string, error) {
	_, configDir, err := findAndReadManifest()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "services"), nil
}

// recreateBridgeForRotation recreates the already-materialized bridge stack so
// it starts with the env file's (rotated) ADMIN_API_KEY. It reuses
// startBridgeStack -- the SAME pull-before-up machinery the provision path uses
// (#718) -- against the existing compose file, rather than the heavier
// DeployCompose (which re-clones the private module repo on every call).
func recreateBridgeForRotation(servicesDir string) error {
	composePath := whatsapp.ComposePath(servicesDir)
	if !whatsapp.IsDeployed(servicesDir) {
		return fmt.Errorf("bridge compose not found at %s; run `citadel whatsapp up` to deploy the bridge first", composePath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	return startBridgeStack(ctx, whatsapp.ProjectName(servicesDir), composePath, whatsapp.EnvPath(servicesDir), nil)
}

// whatsappRotateDeps builds the RotateDeps shared by the CLI and the
// WHATSAPP_PROVISION job's rotate branch. ServicesDir is the READ-only resolver;
// log routes to the caller.
func whatsappRotateDeps(log func(format string, args ...any)) whatsapp.RotateDeps {
	return whatsapp.RotateDeps{
		ServicesDir:    servicesDirForNodeRead,
		RecreateBridge: recreateBridgeForRotation,
		NewBridgeClient: func(port int, adminKey string) whatsapp.RotateBridgeClient {
			return whatsapp.NewClient(bridgeBaseURL(port), adminKey)
		},
		Log: log,
	}
}

func runWhatsAppRotateKey(cmd *cobra.Command, args []string) error {
	servicesDir, err := servicesDirForNodeRead()
	if err != nil {
		return err
	}

	if waRotateDryRun {
		envPath := whatsapp.EnvPath(servicesDir)
		if !whatsapp.IsDeployed(servicesDir) {
			fmt.Println("WhatsApp bridge is not deployed; nothing to rotate.")
			fmt.Println("   Hint: run 'citadel whatsapp up' to deploy it first.")
			return nil
		}
		env, _ := whatsapp.LoadEnv(servicesDir)
		current := whatsapp.AdminKeyFingerprint(env["ADMIN_API_KEY"])
		if current == "" {
			current = "(none set)"
		}
		bold := color.New(color.Bold)
		bold.Println("WhatsApp bridge admin-key rotation (dry run)")
		fmt.Printf("  env file:            %s\n", envPath)
		fmt.Printf("  current fingerprint: %s\n", current)
		fmt.Println("  would: generate a new admin key, atomically rewrite the env file (0600,")
		fmt.Println("         preserving tenant keys), recreate the bridge, and verify the new key.")
		fmt.Println("  --dry-run: nothing was changed.")
		return nil
	}

	fmt.Println("--- 🔑 Rotating WhatsApp bridge admin key ---")
	res, err := whatsapp.RotateAdminKey(cmd.Context(), whatsappRotateDeps(func(format string, a ...any) {
		fmt.Printf("   - "+format+"\n", a...)
	}))
	if err != nil {
		return err
	}

	old := res.OldFingerprint
	if old == "" {
		old = "(none)"
	}
	bold := color.New(color.Bold)
	bold.Println("✅ Admin key rotated")
	fmt.Printf("  old fingerprint: %s\n", old)
	fmt.Printf("  new fingerprint: %s\n", res.NewFingerprint)
	fmt.Println("  bridge recreated and the new key verified against the control plane.")
	fmt.Println("  Tenant keys and the linked WhatsApp session are unchanged.")
	return nil
}
