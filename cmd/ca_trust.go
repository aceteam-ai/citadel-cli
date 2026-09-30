package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catrust"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

// preparePrivateCATrust resolves the endpoints this invocation will actually
// use, including durable values used by unattended reconnects, then primes the
// Linux process trust pool before any HTTPS request can cache it.
func preparePrivateCATrust(cmd *cobra.Command) error {
	authURL := authServiceURL
	if !flagChanged(cmd, "auth-service") && os.Getenv("CITADEL_AUTH_HOST") == "" {
		if cfg := getDeviceConfigFromFile(); cfg != nil && strings.TrimSpace(cfg.APIBaseURL) != "" {
			authURL = cfg.APIBaseURL
		}
	}
	nexus := nexusURL
	if !flagChanged(cmd, "nexus") {
		nexus = network.ResolveControlURL()
	}
	_, err := catrust.Bootstrap(caCertPath, nodeConfigDirFn(), authURL, nexus)
	return err
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
