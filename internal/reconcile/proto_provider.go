package reconcile

import (
	"context"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/protocol"
	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DesiredStateFetcher fetches this node's raw, binary-protobuf-encoded
// DesiredState from the control plane, authenticated by the node's device
// identity. It is an interface so the reconcile package does NOT import
// internal/redisapi (keeping the engine decoupled and unit-testable); the live
// redisapi.Client satisfies it via GetDesiredState.
type DesiredStateFetcher interface {
	// GetDesiredState GETs the node's DesiredState as raw octet-stream protobuf.
	GetDesiredState(ctx context.Context, nodeID string) ([]byte, error)
}

// ActualStateReporter posts a serialized, binary-protobuf-encoded ActualState
// upstream (the SAME device-authed binary endpoint the report-only emitter
// uses). redisapi.Client.PostNodeState satisfies it.
type ActualStateReporter interface {
	PostNodeState(ctx context.Context, body []byte) error
}

// ProtoProvider is the LIVE DesiredStateProvider for the pull reconcile loop
// (aceteam#4273). It fetches the control-plane-assigned DesiredState as protobuf
// over the device-authed HTTP transport, adapts it to the engine's internal
// types, and reports converged ActualState back as protobuf with the applied
// revision stamped (the revision handshake).
//
// Fetch and Report go through the injected transport interfaces so the provider
// carries no HTTP/auth details of its own — the transport (redisapi.Client)
// owns the base URL, device token, and headers.
type ProtoProvider struct {
	Fetcher  DesiredStateFetcher
	Reporter ActualStateReporter
	// NodeID is the Headscale numeric node ID (e.g. "1084"): the fetch path
	// parameter AND the identity stamped into the report envelope. It must match
	// `fabric_node_status.node_id`, which the backend keys by the Headscale
	// numeric ID — the desired-state serve endpoint matches rows by a raw
	// `.eq("node_id", NodeID)` with no hostname resolution, so a hostname here
	// never matches any desired row (aceteam#535).
	NodeID string
	// Version is the citadel-cli version, reported as agent_version.
	Version string
}

// NewProtoProvider builds a ProtoProvider.
func NewProtoProvider(fetcher DesiredStateFetcher, reporter ActualStateReporter, nodeID, version string) *ProtoProvider {
	return &ProtoProvider{Fetcher: fetcher, Reporter: reporter, NodeID: nodeID, Version: version}
}

// Fetch pulls and decodes the node's protobuf DesiredState, then adapts it to
// the engine's internal DesiredState (carrying the revision for the handshake).
func (p *ProtoProvider) Fetch(ctx context.Context) (DesiredState, error) {
	body, err := p.Fetcher.GetDesiredState(ctx, p.NodeID)
	if err != nil {
		return DesiredState{}, fmt.Errorf("fetch desired-state: %w", err)
	}
	var pb fabricpb.DesiredState
	if err := proto.Unmarshal(body, &pb); err != nil {
		return DesiredState{}, fmt.Errorf("decode desired-state: %w", err)
	}
	return adaptDesiredState(&pb), nil
}

// Report encodes the converged ActualState as protobuf (stamping the applied
// revision) and posts it upstream.
func (p *ProtoProvider) Report(ctx context.Context, actual ActualState) error {
	body, err := proto.Marshal(p.buildActualStateProto(actual))
	if err != nil {
		return fmt.Errorf("encode actual-state: %w", err)
	}
	return p.Reporter.PostNodeState(ctx, body)
}

// buildActualStateProto maps the internal ActualState onto the proto ActualState
// wire type, including the applied_revision handshake field. node_id defaults to
// the provider's NodeID when the report left it empty.
func (p *ProtoProvider) buildActualStateProto(actual ActualState) *fabricpb.ActualState {
	now := timestamppb.Now()
	nodeID := actual.Node
	if nodeID == "" {
		nodeID = p.NodeID
	}
	pb := &fabricpb.ActualState{
		ProtocolVersion: uint32(protocol.FabricProtocolVersion),
		NodeId:          nodeID,
		AppliedRevision: actual.AppliedRevision,
		AgentVersion:    p.Version,
		ReportedAt:      now,
	}
	for _, m := range actual.Modules {
		status, health := healthToProto(m.Health)
		pb.Modules = append(pb.Modules, &fabricpb.ActualModule{
			Source:           m.Source,
			InstalledVersion: installedVersionFromModule(m),
			ImageDigest:      firstDigest(m.ImageDigests),
			Status:           status,
			Health:           health,
			Error:            m.Error,
			UpdatedAt:        now,
		})
	}
	return pb
}

// installedVersionFromModule picks the most specific resolved identifier the
// internal InstalledModule carries: the requested ref, else the resolved commit.
func installedVersionFromModule(m InstalledModule) string {
	if m.Ref != "" {
		return m.Ref
	}
	return m.Commit
}

// firstDigest returns the first non-empty image digest, or "". The proto carries
// a single image_digest while a module may deploy several images.
func firstDigest(digests []string) string {
	for _, d := range digests {
		if d != "" {
			return d
		}
	}
	return ""
}

// Compile-time check that the provider satisfies the transport interface.
var _ DesiredStateProvider = (*ProtoProvider)(nil)
