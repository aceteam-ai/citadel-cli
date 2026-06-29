// cmd/version.go
package cmd

import (
	"fmt"
	"runtime/debug"

	"github.com/aceteam-ai/citadel-cli/internal/protocol"
	"github.com/spf13/cobra"
)

// version is set via ldflags at build time
var version = ""

// Version returns the CLI version, preferring ldflags, then build info, then "dev"
var Version = func() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return "dev"
}()

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of Citadel CLI",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Citadel CLI version %s\n", Version)
		fmt.Printf("Fabric protocol: v%d\n", protocol.FabricProtocolVersion)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
