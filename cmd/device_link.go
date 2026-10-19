package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	goversion "github.com/hashicorp/go-version"
)

// deviceLinkEnforcedFrom is the first aceteam release whose link route creates
// or adopts the device configuration (aceteam#11743). An older server answers a
// grant approved without the onboarding wizard with 404, so its refusal says
// nothing about this enrollment and must not fail it.
var deviceLinkEnforcedFrom = goversion.Must(goversion.NewVersion("1.520.0"))

// clearDeviceCredentialsFn removes the device credentials an enrollment saved
// before linking, so a fatal refusal does not leave a half-enrolled node that
// the next `citadel init` treats as configured. A seam so tests never touch
// the machine's real config.
var clearDeviceCredentialsFn = clearSavedConfig

// deviceLinkWarnFn reports a refusal that was not fatal.
var deviceLinkWarnFn = func(msg string) { fmt.Fprintln(os.Stderr, msg) }

// deviceLinker is the slice of the device-auth client enrollment linking uses.
type deviceLinker interface {
	LinkRegisteredNode(ctx context.Context, deviceCode, nodeID string) error
	ServerVersion(ctx context.Context) (string, error)
}

// linkDeviceEnrollment uses only the grant just approved and the identity the
// network actually registered. Never fall back to saved credentials/hostname/IP:
// a stale reservation or a previously connected node must fail at the server.
func linkDeviceEnrollment(result *DeviceAuthResult) error {
	return linkDeviceEnrollmentGated(result, network.GetGlobalNodeID, nexus.NewDeviceAuthClient(authServiceURL))
}

// linkDeviceEnrollmentGated links the enrollment and decides whether a refusal
// is fatal. Only a server that reports v1.520.0 or later is trusted to refuse
// for a reason that applies to this enrollment; against an older server, or one
// whose version cannot be read, the refusal is reported and enrollment goes on
// as it did before linking existed. A fatal refusal also removes the device
// credentials saved earlier in the same enrollment.
func linkDeviceEnrollmentGated(result *DeviceAuthResult, nodeID func(context.Context) string, client deviceLinker) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	linkErr := linkDeviceEnrollmentWith(ctx, result, nodeID, client.LinkRegisteredNode)
	cancel()
	if linkErr == nil {
		return nil
	}
	// A fresh budget: a slow refusal must not starve the version read.
	vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer vcancel()
	version, versionErr := client.ServerVersion(vctx)
	if versionErr == nil {
		if enforced, ok := deviceLinkEnforced(version); ok && enforced {
			clearDeviceCredentialsFn()
			return deviceEnrollmentRecoveryError(linkErr)
		} else if ok {
			deviceLinkWarnFn(fmt.Sprintf("⚠️  Device link not completed (%v). The server reports v%s, older than v%s, which cannot link this enrollment; continuing without a completed device link.", linkErr, strings.TrimPrefix(version, "v"), deviceLinkEnforcedFrom))
			return nil
		}
		versionErr = fmt.Errorf("unrecognized server version")
	}
	deviceLinkWarnFn(fmt.Sprintf("⚠️  Device link not completed (%v), and the server version could not be read (%v); continuing without a completed device link.", linkErr, versionErr))
	return nil
}

// deviceLinkEnforced reports whether the server version is at or above
// deviceLinkEnforcedFrom. ok is false when the version does not parse.
func deviceLinkEnforced(version string) (enforced, ok bool) {
	v, err := goversion.NewVersion(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if err != nil {
		return false, false
	}
	return v.GreaterThanOrEqual(deviceLinkEnforcedFrom), true
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
