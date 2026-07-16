package reconcile

import (
	"context"
	"errors"
	"testing"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/proto"
)

// fakeTransport is an in-memory DesiredStateFetcher + ActualStateReporter. It
// serves a settable protobuf DesiredState and captures the last posted body so
// tests can decode it and assert the report shape (incl. applied_revision).
type fakeTransport struct {
	desired     *fabricpb.DesiredState
	fetchErr    error
	fetchNodeID string // records the node id the fetch was called with

	postErr  error
	postBody []byte
	posts    int
}

func (f *fakeTransport) GetDesiredState(ctx context.Context, nodeID string) ([]byte, error) {
	f.fetchNodeID = nodeID
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return proto.Marshal(f.desired)
}

func (f *fakeTransport) PostNodeState(ctx context.Context, body []byte) error {
	f.posts++
	if f.postErr != nil {
		return f.postErr
	}
	f.postBody = body
	return nil
}

func TestProtoProviderFetchDecodesAndAdapts(t *testing.T) {
	tr := &fakeTransport{desired: &fabricpb.DesiredState{
		Revision: "rev-42",
		Modules: []*fabricpb.DesiredModule{
			{Source: "owner/repo@^1.2", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING},
			{Source: "gone", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_ABSENT},
		},
	}}
	p := NewProtoProvider(tr, tr, "node-abc", "v9.9.9")

	ds, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if tr.fetchNodeID != "node-abc" {
		t.Errorf("fetch called with node id %q, want node-abc", tr.fetchNodeID)
	}
	if ds.Revision != "rev-42" {
		t.Errorf("revision not carried: %q", ds.Revision)
	}
	// ABSENT dropped -> only the running module survives.
	if len(ds.Modules) != 1 || ds.Modules[0].Source != "owner/repo@^1.2" {
		t.Fatalf("unexpected modules: %+v", ds.Modules)
	}
}

func TestProtoProviderFetchError(t *testing.T) {
	tr := &fakeTransport{fetchErr: errors.New("boom")}
	p := NewProtoProvider(tr, tr, "n", "v1")
	if _, err := p.Fetch(context.Background()); err == nil {
		t.Fatal("want error from failed fetch")
	}
}

func TestProtoProviderReportStampsAppliedRevision(t *testing.T) {
	tr := &fakeTransport{}
	p := NewProtoProvider(tr, tr, "node-xyz", "v2.0.0")

	err := p.Report(context.Background(), ActualState{
		Node:            "node-xyz",
		AppliedRevision: "rev-42",
		Modules: []InstalledModule{
			{Name: "embedding", Source: "embedding", Ref: "v1.2.0", Health: HealthRunning},
			{Name: "broken", Source: "owner/broken@v1", Health: HealthError, Error: "install failed"},
		},
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if tr.posts != 1 {
		t.Fatalf("want 1 post, got %d", tr.posts)
	}

	var got fabricpb.ActualState
	if err := proto.Unmarshal(tr.postBody, &got); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	if got.GetAppliedRevision() != "rev-42" {
		t.Errorf("applied_revision = %q, want rev-42", got.GetAppliedRevision())
	}
	if got.GetNodeId() != "node-xyz" {
		t.Errorf("node_id = %q, want node-xyz", got.GetNodeId())
	}
	if got.GetAgentVersion() != "v2.0.0" {
		t.Errorf("agent_version = %q, want v2.0.0", got.GetAgentVersion())
	}
	if got.GetProtocolVersion() == 0 {
		t.Errorf("protocol_version must be set")
	}
	if len(got.GetModules()) != 2 {
		t.Fatalf("want 2 modules, got %d", len(got.GetModules()))
	}
	// Health mapping: running -> RUNNING/HEALTHY, error -> UNSPECIFIED/ERROR + error string.
	byName := map[string]*fabricpb.ActualModule{}
	for _, m := range got.GetModules() {
		byName[m.GetSource()] = m
	}
	if run := byName["embedding"]; run == nil ||
		run.GetStatus() != fabricpb.ModuleStatus_MODULE_STATUS_RUNNING ||
		run.GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY ||
		run.GetInstalledVersion() != "v1.2.0" {
		t.Errorf("running module mapped wrong: %+v", run)
	}
	if brk := byName["owner/broken@v1"]; brk == nil ||
		brk.GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_ERROR ||
		brk.GetError() != "install failed" {
		t.Errorf("error module mapped wrong: %+v", brk)
	}
}

func TestProtoProviderReportDefaultsNodeID(t *testing.T) {
	tr := &fakeTransport{}
	p := NewProtoProvider(tr, tr, "fallback-node", "v1")
	if err := p.Report(context.Background(), ActualState{}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	var got fabricpb.ActualState
	if err := proto.Unmarshal(tr.postBody, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.GetNodeId() != "fallback-node" {
		t.Errorf("empty report node must default to provider NodeID, got %q", got.GetNodeId())
	}
}

// TestProtoProviderRevisionHandshakeThroughReconcileOnce exercises the full
// loop: Fetch a revision, converge via a fake ModuleOps, and assert the reported
// ActualState echoes that revision as applied_revision.
func TestProtoProviderRevisionHandshakeThroughReconcileOnce(t *testing.T) {
	tr := &fakeTransport{desired: &fabricpb.DesiredState{
		Revision: "rev-100",
		Modules: []*fabricpb.DesiredModule{
			{Source: "embedding", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING},
		},
	}}
	provider := NewProtoProvider(tr, tr, "node-1", "v1")
	rec := NewReconciler(provider, newFakeOps(), "node-1")

	if _, _, err := rec.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	var got fabricpb.ActualState
	if err := proto.Unmarshal(tr.postBody, &got); err != nil {
		t.Fatalf("decode posted report: %v", err)
	}
	if got.GetAppliedRevision() != "rev-100" {
		t.Fatalf("handshake broken: applied_revision = %q, want rev-100", got.GetAppliedRevision())
	}
}

// TestReconcileUsesHeadscaleNodeIDNotHostname is the regression guard for
// aceteam#535: the pull loop must fetch desired-state AND report actual-state
// under the Headscale numeric node ID, not the hostname. The desired-state serve
// endpoint matches rows by a raw `.eq("node_id", <path param>)` against
// `fabric_node_status.node_id` (keyed by the Headscale numeric ID), so fetching
// by hostname never matches any desired row and the loop applies nothing.
//
// It drives a full ReconcileOnce with the numeric ID threaded exactly as
// newReconcileLoop wires it (ProtoProvider.NodeID + Reconciler.Node), then
// asserts the fetch path param and the reported envelope both carry the numeric
// ID — never a hostname.
func TestReconcileUsesHeadscaleNodeIDNotHostname(t *testing.T) {
	const headscaleNodeID = "1084" // fabric_node_status.node_id — Headscale numeric ID
	const hostname = "ubuntu-gpu"  // the WRONG key the loop used before #535

	tr := &fakeTransport{desired: &fabricpb.DesiredState{
		Revision: "rev-535",
		Modules: []*fabricpb.DesiredModule{
			{Source: "embedding", DesiredStatus: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING},
		},
	}}
	provider := NewProtoProvider(tr, tr, headscaleNodeID, "v1")
	rec := NewReconciler(provider, newFakeOps(), headscaleNodeID)

	if _, _, err := rec.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	// Fetch must request the numeric ID, so the serve endpoint's raw node_id match hits.
	if tr.fetchNodeID != headscaleNodeID {
		t.Errorf("fetch keyed by %q, want Headscale numeric ID %q", tr.fetchNodeID, headscaleNodeID)
	}
	if tr.fetchNodeID == hostname {
		t.Errorf("fetch keyed by hostname %q — this is the #535 bug", hostname)
	}

	// Report must stamp the same numeric ID so node_module_state keys correctly.
	var got fabricpb.ActualState
	if err := proto.Unmarshal(tr.postBody, &got); err != nil {
		t.Fatalf("decode posted report: %v", err)
	}
	if got.GetNodeId() != headscaleNodeID {
		t.Errorf("report node_id = %q, want Headscale numeric ID %q", got.GetNodeId(), headscaleNodeID)
	}
	if got.GetNodeId() == hostname {
		t.Errorf("report node_id = hostname %q — fetch/report identity must match the desired-row key", hostname)
	}
}
