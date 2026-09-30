package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catrust"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var deviceConfigForCATrustFn = getDeviceConfigFromFile

// preparePrivateCATrust resolves the endpoints this invocation will actually
// use, including durable values used by unattended reconnects, then primes the
// Linux process trust pool before any HTTPS request can cache it.
func preparePrivateCATrust(cmd *cobra.Command) error {
	nodeDir := nodeConfigDirFn()
	nexus := nexusURL
	if !flagChanged(cmd, "nexus") {
		nexus = network.ResolveControlURL()
	}
	authURL := authServiceURL
	if !flagChanged(cmd, "auth-service") && os.Getenv("CITADEL_AUTH_HOST") == "" {
		if cfg := deviceConfigForCATrustFn(); cfg != nil && strings.TrimSpace(cfg.APIBaseURL) != "" {
			authURL = cfg.APIBaseURL
		} else {
			storedAuth, err := catrust.AuthOriginForNexus(nodeDir, nexus)
			if err != nil {
				return err
			}
			if storedAuth != "" {
				authURL = storedAuth
			}
		}
	}
	if _, err := catrust.Bootstrap(caCertPath, nodeDir, authURL, nexus); err != nil {
		return err
	}

	// Keep the rest of this invocation on the same endpoint pair we just
	// validated. In particular, an authkey-only reconnect has no device config
	// from which later commands could recover the self-hosted auth origin.
	authServiceURL = authURL
	nexusURL = nexus
	return nil
}

func flagChanged(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	if f := cmd.Flags().Lookup(name); f != nil {
		return f.Changed
	}
	if f := cmd.InheritedFlags().Lookup(name); f != nil {
		return f.Changed
	}
	return false
}

// persistPrivateCATrust is called only after the mesh connection succeeds.
// This avoids pinning an unusable CA or endpoint pair after a failed attempt.
func persistPrivateCATrust(authURL, controlURL string) error {
	wrote, err := catrust.PersistActive(nodeConfigDirFn(), authURL, controlURL)
	if err != nil {
		return fmt.Errorf("persist private CA trust: %w", err)
	}
	if wrote {
		fixStatePermissionsFn()
	}
	return nil
}
