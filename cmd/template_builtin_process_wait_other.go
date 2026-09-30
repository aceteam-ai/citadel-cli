//go:build !linux

package cmd

import (
	"fmt"
	"os"
)

func waitForPapercraftProcessExit(*os.Process) error {
	return fmt.Errorf("render process observation is unsupported on this platform")
}
