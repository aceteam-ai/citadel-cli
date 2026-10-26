package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// Saved credentials and a connected mesh are not evidence of a completed
// current-key link. Only the request-scoped authenticated lookup below can
// suppress this display warning. It never changes admission or credentials.
const deviceLinkUnverified = "unverified (saved credentials do not prove a completed device link). If enrollment did not finish or the API refuses this node, run 'citadel logout && citadel init' for fresh browser approval."

const deviceLinkRefused = "unverified (the server could not verify this credential). If the credential predates server verification, re-enroll to verify with 'citadel logout && citadel init' for fresh browser approval. Missing permissions or membership can also prevent verification."
const deviceLinkNotLinked = "unverified (the current credential is not linked). Run 'citadel logout && citadel init' for fresh browser approval."
const deviceLinkUnsupported = "unverified (this server does not support current-key binding verification). Saved credentials do not prove a completed device link."

// The ordinary collector is intentionally offline: dashboard refreshes must
// neither poll the lookup nor retain an earlier render's confirmation.
func deviceLinkDiagnostic(dc *DeviceConfig) string {
	if dc == nil || dc.DeviceAPIToken == "" {
		return ""
	}
	return deviceLinkUnverified
}

func printDeviceLinkDiagnostic(w io.Writer, dc *DeviceConfig) {
	printDeviceLinkDiagnosticWith(w, dc, defaultDeviceLinkObservation())
}

func printDeviceLinkDiagnosticContext(ctx context.Context, w io.Writer, dc *DeviceConfig) {
	if detail := observeDeviceLinkDiagnostic(ctx, dc, defaultDeviceLinkObservation()); detail != "" {
		fmt.Fprintf(w, "  Device link: %s\n", detail)
	}
}

type deviceLinkObservation struct {
	config     func() *DeviceConfig
	liveNodeID func(context.Context) string
	defaultURL func() string
	lookup     func(context.Context, nexus.DeviceBindingRequest) nexus.DeviceBindingState
}

func defaultDeviceLinkObservation() deviceLinkObservation {
	return deviceLinkObservation{
		config: getDeviceConfigFromFile, liveNodeID: network.GetGlobalNodeID,
		defaultURL: func() string { return authServiceURL },
		lookup:     nexus.LookupCurrentDeviceBinding,
	}
}

func printDeviceLinkDiagnosticWith(w io.Writer, dc *DeviceConfig, observation deviceLinkObservation) {
	if detail := observeDeviceLinkDiagnostic(context.Background(), dc, observation); detail != "" {
		fmt.Fprintf(w, "  Device link: %s\n", detail)
	}
}

func sameDeviceLinkConfig(before DeviceConfig, after *DeviceConfig) bool {
	return after != nil && after.DeviceAPIToken != "" &&
		sha256.Sum256([]byte(before.DeviceAPIToken)) == sha256.Sum256([]byte(after.DeviceAPIToken)) &&
		before.OrgID == after.OrgID && before.APIBaseURL == after.APIBaseURL && before.NexusURL == after.NexusURL
}

// One invocation owns the observation: no marker, memo, global confirmation or
// server-time freshness window. Input rechecks do not claim an all-writer lock
// or durable ABA detection. Refusal text is static and never parses API bodies.
func observeDeviceLinkDiagnostic(parent context.Context, dc *DeviceConfig, observation deviceLinkObservation) string {
	if dc == nil || dc.DeviceAPIToken == "" {
		return ""
	}
	if observation.config == nil || observation.liveNodeID == nil || observation.defaultURL == nil || observation.lookup == nil {
		return deviceLinkUnverified
	}
	ctx, cancel := context.WithTimeout(parent, nexus.CurrentDeviceBindingTimeout)
	defer cancel()
	captured := *dc
	if !sameDeviceLinkConfig(captured, observation.config()) || ctx.Err() != nil {
		return deviceLinkUnverified
	}
	baseURL := captured.APIBaseURL
	if baseURL == "" {
		baseURL = observation.defaultURL()
	}
	input := nexus.DeviceBindingRequest{
		APIBaseURL: baseURL, Bearer: captured.DeviceAPIToken,
		OrgID: captured.OrgID, NodeID: observation.liveNodeID(ctx),
	}
	if !input.Valid() || ctx.Err() != nil {
		return deviceLinkUnverified
	}
	state := observation.lookup(ctx, input)
	current := observation.config()
	if !sameDeviceLinkConfig(captured, current) || ctx.Err() != nil {
		return deviceLinkUnverified
	}
	currentURL := current.APIBaseURL
	if currentURL == "" {
		currentURL = observation.defaultURL()
	}
	if currentURL != baseURL || observation.liveNodeID(ctx) != input.NodeID || ctx.Err() != nil {
		return deviceLinkUnverified
	}
	switch state {
	case nexus.BindingConfirmed:
		return ""
	case nexus.BindingNotLinked:
		return deviceLinkNotLinked
	case nexus.BindingRefused:
		return deviceLinkRefused
	case nexus.BindingUnsupported:
		return deviceLinkUnsupported
	default:
		return deviceLinkUnverified
	}
}
