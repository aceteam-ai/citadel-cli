// internal/nexus/network_helpers.go
package nexus

import (
	"context"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// DebugFunc is a callback for debug logging, set by cmd package
var DebugFunc func(format string, args ...any)

// NetworkChoice represents the user's selected method for network connection.
type NetworkChoice string

const (
	// NetChoiceDevice indicates the user will use device authorization flow.
	NetChoiceDevice NetworkChoice = "device"
	// NetChoiceAuthkey indicates the user will provide a pre-generated key.
	NetChoiceAuthkey NetworkChoice = "authkey"
	// NetChoiceSkip indicates the user has chosen to skip network connection.
	NetChoiceSkip NetworkChoice = "skip"
	// NetChoiceVerified indicates the user is already online and authenticated.
	NetChoiceVerified NetworkChoice = "verified"
)

// IsNetworkConnected checks if currently connected to the AceTeam Network.
func IsNetworkConnected() bool {
	return network.IsGlobalConnected()
}

// NetworkLogout logs out of the current network connection.
func NetworkLogout() error {
	fmt.Println("   - Logging out of current network connection...")
	if err := network.Logout(); err != nil {
		return fmt.Errorf("failed to logout: %w", err)
	}
	fmt.Println("   - ✅ Successfully logged out.")
	return nil
}

// GetNetworkChoice checks the current network status and, if offline, prompts the
// user to select a connection method. It accepts the authkey from a flag as a parameter.
func GetNetworkChoice(authkey string) (choice NetworkChoice, key string, err error) {
	debug := func(format string, args ...any) {
		if DebugFunc != nil {
			DebugFunc(format, args...)
		}
	}

	if authkey != "" {
		debug("authkey provided via flag, using NetChoiceAuthkey")
		return NetChoiceAuthkey, authkey, nil
	}

	// Check if already connected
	if network.IsGlobalConnected() {
		debug("already connected to network, using NetChoiceVerified")
		return NetChoiceVerified, "", nil
	}

	// If state exists, try to reconnect silently with a generous timeout
	// tsnet startup can take 30+ seconds on slow networks or cold starts
	hasState := network.HasState()
	debug("HasState: %v", hasState)

	if hasState {
		fmt.Print("Reconnecting to AceTeam Network... ")
		debug("attempting reconnect with 45s timeout...")
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		connected, reconnectErr := network.VerifyOrReconnect(ctx)
		debug("reconnect result: connected=%v, err=%v", connected, reconnectErr)

		if connected {
			fmt.Println("done")
			return NetChoiceVerified, "", nil
		}
		fmt.Println("failed")
		fmt.Println("⚠️  Could not reconnect with existing credentials.")
		fmt.Println("   Running fresh device authentication...")
		fmt.Println("   (Note: This may assign a new network IP)")
	}

	// Default to device authorization flow
	// Use --authkey flag for automation/CI
	debug("falling through to NetChoiceDevice")
	return NetChoiceDevice, "", nil
}
