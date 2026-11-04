//go:build !linux

package cmd

import (
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
	"github.com/spf13/cobra"
)

func declaredTerminateSignal() os.Signal { return os.Interrupt }

func runDeclaredAttach(*cobra.Command, string) error {
	return declaredsession.ErrUnsupported
}
