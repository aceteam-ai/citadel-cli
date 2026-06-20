//go:build !darwin

package cmd

import (
	"context"
)

// startCaffeinate is a no-op on non-macOS platforms.
func startCaffeinate(_ context.Context) func() {
	return func() {}
}
