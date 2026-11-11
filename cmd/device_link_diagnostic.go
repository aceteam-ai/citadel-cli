package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// Saved credentials and a connected mesh are not evidence of a completed
// current-key link. Only the request-scoped authenticated lookup below can
// suppress this display warning. It never changes admission or credentials.
const deviceLinkUnverified = "unverified (saved credentials do not prove a completed device link). If enrollment did not finish or the API refuses this node, run 'citadel logout && citadel init' for fresh browser approval."

const deviceLinkRefused = "unverified (the server could not verify this credential). The server may not support verification yet. If it does and the credential predates server verification, re-enroll to verify with 'citadel logout && citadel init' for fresh browser approval. Missing permissions or membership can also prevent verification."
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

func printDeviceLinkDiagnosticContext(ctx context.Context, w io.Writer, dc *DeviceConfig) {
	printDeviceLinkDiagnosticWith(ctx, w, dc, defaultDeviceLinkObservation())
}

type deviceLinkMeshIdentity struct {
	NodeID     string
	ControlURL string
}

type deviceLinkObservation struct {
	config     func() *DeviceConfig
	liveMesh   func(context.Context) deviceLinkMeshIdentity
	controlURL func() string
	defaultURL func() string
	lookup     func(context.Context, nexus.DeviceBindingRequest) nexus.DeviceBindingState
}

func defaultDeviceLinkObservation() deviceLinkObservation {
	return deviceLinkObservation{
		config: getDeviceConfigFromFile,
		liveMesh: func(ctx context.Context) deviceLinkMeshIdentity {
			status, err := network.GetGlobalStatus(ctx)
			if err != nil || status == nil || !status.Connected {
				return deviceLinkMeshIdentity{}
			}
			return deviceLinkMeshIdentity{NodeID: status.NodeID, ControlURL: status.ControlURL}
		},
		controlURL: network.ResolveControlURL,
		defaultURL: func() string { return authServiceURL },
		lookup:     nexus.LookupCurrentDeviceBinding,
	}
}

func printDeviceLinkDiagnosticWith(ctx context.Context, w io.Writer, dc *DeviceConfig, observation deviceLinkObservation) {
	if detail := observeDeviceLinkDiagnostic(ctx, dc, observation); detail != "" {
		fmt.Fprintf(w, "  Device link: %s\n", detail)
	}
}

func deviceLinkControlURL(value string) string {
	normalized := normalizeControlURL(value)
	u, err := url.Parse(normalized)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(normalized, "#") {
		return ""
	}
	return normalized
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
	if observation.config == nil || observation.liveMesh == nil || observation.controlURL == nil || observation.defaultURL == nil || observation.lookup == nil {
		return deviceLinkUnverified
	}
	if parent == nil {
		parent = context.Background()
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
	mesh := observation.liveMesh(ctx)
	input := nexus.DeviceBindingRequest{
		APIBaseURL: baseURL, Bearer: captured.DeviceAPIToken,
		OrgID: captured.OrgID, NodeID: mesh.NodeID,
	}
	if !input.Valid() || ctx.Err() != nil {
		return deviceLinkUnverified
	}
	controlURL := deviceLinkControlURL(observation.controlURL())
	if controlURL == "" || deviceLinkControlURL(mesh.ControlURL) != controlURL || ctx.Err() != nil {
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
	currentMesh := observation.liveMesh(ctx)
	if currentURL != baseURL || currentMesh.NodeID != input.NodeID ||
		deviceLinkControlURL(currentMesh.ControlURL) != controlURL ||
		deviceLinkControlURL(observation.controlURL()) != controlURL || ctx.Err() != nil {
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
