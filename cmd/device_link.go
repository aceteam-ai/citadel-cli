package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// linkDeviceEnrollment uses only the grant just approved and the identity the
// network actually registered. Never fall back to saved credentials/hostname/IP:
// a stale reservation or a previously connected node must fail at the server.
func linkDeviceEnrollment(result *DeviceAuthResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := linkDeviceEnrollmentWith(ctx, result, network.GetGlobalNodeID, func(ctx context.Context, code, nodeID string) error {
		return nexus.NewDeviceAuthClient(authServiceURL).LinkRegisteredNode(ctx, code, nodeID)
	})
	if err != nil {
		return deviceEnrollmentRecoveryError(err)
	}
	return nil
}

func deviceEnrollmentRecoveryError(err error) error {
	return fmt.Errorf("%w; saved credentials do not complete device binding. Run 'citadel logout && citadel init' to register with a fresh authorization", err)
}

func linkDeviceEnrollmentWith(ctx context.Context, result *DeviceAuthResult, nodeID func(context.Context) string, link func(context.Context, string, string) error) error {
	if result == nil || result.Token == nil || result.DeviceCode == "" || result.Token.Authkey == "" {
		return fmt.Errorf("device enrollment cannot be linked without its current authorization")
	}
	id := nodeID(ctx)
	if id == "" {
		return fmt.Errorf("device enrollment cannot be linked before network registration")
	}
	return link(ctx, result.DeviceCode, id)
}

// ccConnectDeviceEnrollmentWith keeps the modal's current grant across network
// registration. A refused link disconnects and reaches the UI as an error,
// preventing its success callback from starting a worker for an unlinked grant.
func ccConnectDeviceEnrollmentWith(authkey, deviceCode string, connect func() error, link func(*DeviceAuthResult) error, disconnect func() error) error {
	if authkey == "" || deviceCode == "" {
		return deviceEnrollmentRecoveryError(fmt.Errorf("current device authorization is missing"))
	}
	if err := connect(); err != nil {
		return err
	}
	if err := link(&DeviceAuthResult{DeviceCode: deviceCode, Token: &nexus.TokenResponse{Authkey: authkey}}); err != nil {
		_ = disconnect()
		return err
	}
	return nil
}
