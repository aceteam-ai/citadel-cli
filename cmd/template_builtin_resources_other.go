//go:build !linux

package cmd

import (
	"context"
	"fmt"
	"runtime"
)

// Rendering fails closed off Linux until an equivalent inheritable job object
// or sandbox is implemented. Running without the promised OS ceiling would be
// a silent security downgrade.
func newRenderLimitedCommand(context.Context, []string, string, ...string) (*renderCommand, error) {
	return nil, fmt.Errorf("render resource isolation is unsupported on %s", runtime.GOOS)
}
