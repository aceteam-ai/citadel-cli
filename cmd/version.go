// cmd/version.go
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version will be set at build time
var Version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of Citadel CLI",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Citadel CLI version %s\n", Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
