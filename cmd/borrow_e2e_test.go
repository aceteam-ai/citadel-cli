package cmd

import (
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/serviceborrow"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// e2eFakeSigner is a fully in-memory aep.Signer: it signs deterministically and
// reports a fixed fingerprint, so the e2e never touches this box's real node
// identity (the CLAUDE.md #787 hermeticity rule).
type e2eFakeSigner struct{}

func (e2eFakeSigner) Sign(p []byte) ([]byte, error) { h := sha256.Sum256(p); return h[:], nil }
func (e2eFakeSigner) PublicKeyFingerprint() (string, error) {
	return "sha256:e2e-fake-fingerprint", nil
}

// e2eSameOwnerResolver resolves every caller to a fixed same-owner verified
// identity (the mesh identity the serving gate trusts).
type e2eSameOwnerResolver struct{}

func (e2eSameOwnerResolver) ResolvePeer(_ context.Context, _ string) (*gateway.MeshPeerIdentity, error) {
	return &gateway.MeshPeerIdentity{
		NodeName:  "requester",
		LoginName: "owner@x.co",
		StableID:  "req-stable-id",
		OwnerID:   "req-owner-id",
		SameOwner: true,
	}, nil
}

// e2eUpstream is a loopback-service stand-in that echoes its request body.
func e2eUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

// e2eOneConnGateway is a serviceborrow.MeshGateway that returns a connection to a
// single fixed base URL (the running gateway under test) — the httptest stand-in
// for the verified mesh transport.
type e2eOneConnGateway struct{ baseURL string }

func (g e2eOneConnGateway) Connect(_ context.Context, meshIP string) (*serviceborrow.PeerConn, error) {
	return &serviceborrow.PeerConn{
		HTTP:     &http.Client{Timeout: 10 * time.Second},
		BaseURL:  g.baseURL,
		MeshIP:   meshIP,
		StableID: "req-stable-id",
		OwnerID:  "req-owner-id",
	}, nil
}

func addrFromURL(t *testing.T, raw string) string {
	t.Helper()
	// "http://127.0.0.1:PORT" -> "127.0.0.1:PORT"
	return raw[len("http://"):]
}

// TestBorrowE2E_RequesterBorrowsTwoServicesThroughOneGate is the hermetic
// end-to-end proof for citadel-cli#1289's acceptance:
//
//   - A requester borrows EMBEDDING from a peer over the (fake) mesh transport;
//     the peer serves it loopback, signs a serving receipt, and writes a work
//     record. The requester reads the serving node id + receipt id + job id.
//   - The requester then borrows TRANSCRIPTION through the SAME client and the
//     SAME gate with NO new gate code — only a second provider registration.
//   - Both ledgers reference each borrow by its per-call job id, and the serving
//     ledger shows one record PER call (counts match; the per-call-unique job id
//     prevents INSERT OR IGNORE from collapsing them).
func TestBorrowE2E_RequesterBorrowsTwoServicesThroughOneGate(t *testing.T) {
	embedUp := e2eUpstream(t)
	transcribeUp := e2eUpstream(t)

	servingStore, err := usage.OpenStore(filepath.Join(t.TempDir(), "serving-usage.db"))
	if err != nil {
		t.Fatalf("open serving usage store: %v", err)
	}
	t.Cleanup(func() { _ = servingStore.Close() })

	// Two providers through ONE gate/receipt path (the "no new gate code" proof).
	providers := map[string]*gateway.BorrowProvider{
		"embedding": {
			Kind:            "embedding",
			Upstream:        addrFromURL(t, embedUp.URL),
			AllowedSubpaths: map[string]bool{"/v1/embeddings": true},
			WorkAction:      "embed_served",
			Identity: func() serviceborrow.ServiceIdentity {
				return serviceborrow.ServiceIdentity{Kind: "embedding", Model: "gte"}
			},
			Capacity: func() serviceborrow.Capacity { return serviceborrow.Capacity{Device: "cuda"} },
			Ready:    func() bool { return true },
		},
		"transcription": {
			Kind:            "transcription",
			Upstream:        addrFromURL(t, transcribeUp.URL),
			AllowedSubpaths: map[string]bool{"/transcribe": true},
			WorkAction:      "transcribe",
			Identity: func() serviceborrow.ServiceIdentity {
				return serviceborrow.ServiceIdentity{Kind: "transcription", Model: "whisper-large"}
			},
			Capacity: func() serviceborrow.Capacity { return serviceborrow.Capacity{Device: "cpu"} },
			Ready:    func() bool { return true },
		},
	}

	cfg := buildBorrowServeConfig(borrowServerDeps{
		Signer:        e2eFakeSigner{},
		NodeID:        "fabric-node-77",
		NodeName:      "serving-node",
		InsertUsage:   func(r usage.UsageRecord) error { return servingStore.Insert(r) },
		AllowedLogins: nil, // same-owner only
		Providers:     providers,
	})

	gw := gateway.NewServer(gateway.Config{Port: 0, NodeName: "serving-node"}) // no TLS -> plain HTTP
	gw.SetMeshResolver(e2eSameOwnerResolver{})
	gw.SetBorrowServer(cfg)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gw.AddListener(ln)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = gw.Start(ctx) }()

	baseURL := "http://" + ln.Addr().String()
	client := serviceborrow.NewClient(e2eOneConnGateway{baseURL: baseURL}, nil)

	conn, err := e2eOneConnGateway{baseURL: baseURL}.Connect(ctx, "100.64.0.9")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// --- Embedding: two calls with distinct per-call job ids ---
	var embedJobIDs []string
	for seq := 0; seq < 2; seq++ {
		jobID := serviceborrow.MintJobID("index-parent-1", seq)
		embedJobIDs = append(embedJobIDs, jobID)
		resp, err := client.Borrow(ctx, conn, serviceborrow.BorrowRequest{
			Kind: "embedding", Subpath: "/v1/embeddings",
			Body: []byte(`{"input":["hello"]}`), ContentType: "application/json", JobID: jobID,
		})
		if err != nil {
			t.Fatalf("borrow embedding: %v", err)
		}
		if resp.Status != http.StatusOK {
			t.Fatalf("embedding status = %d", resp.Status)
		}
		if resp.ServingNodeID != "fabric-node-77" {
			t.Fatalf("serving node id = %q, want fabric-node-77 (the delegated_node_id the requester records)", resp.ServingNodeID)
		}
		if resp.ReceiptID == "" {
			t.Fatal("expected a serving receipt id")
		}
		if resp.JobID != jobID {
			t.Fatalf("echoed job id = %q, want %q", resp.JobID, jobID)
		}
	}

	// --- Transcription: SAME client, SAME gate, no new gate code ---
	transcribeJobID := serviceborrow.MintJobID("index-parent-1", 99)
	resp, err := client.Borrow(ctx, conn, serviceborrow.BorrowRequest{
		Kind: "transcription", Subpath: "/transcribe",
		Body: []byte("audio-bytes"), ContentType: "application/octet-stream", JobID: transcribeJobID,
	})
	if err != nil {
		t.Fatalf("borrow transcription: %v", err)
	}
	if resp.Status != http.StatusOK || resp.ServingNodeID != "fabric-node-77" || resp.ReceiptID == "" {
		t.Fatalf("transcription borrow = status %d serving %q receipt %q", resp.Status, resp.ServingNodeID, resp.ReceiptID)
	}

	// --- Serving ledger: one record per call, counts match, cross-ref by id ---
	records, err := servingStore.QueryUnsynced(100)
	if err != nil {
		t.Fatalf("query serving usage: %v", err)
	}
	byJob := map[string]usage.UsageRecord{}
	for _, r := range records {
		byJob[r.JobID] = r
	}
	if len(records) != 3 {
		t.Fatalf("serving ledger has %d records, want 3 (2 embedding + 1 transcription — per-call job ids must not collapse)", len(records))
	}
	for _, jobID := range embedJobIDs {
		r, ok := byJob[jobID]
		if !ok {
			t.Fatalf("serving ledger missing embedding job %q", jobID)
		}
		if r.JobType != "borrow:embedding" || r.WorkAction != "embed_served" || !r.ReceiptSigned {
			t.Fatalf("embedding record = %+v", r)
		}
		if r.DelegatedNodeID != "" {
			t.Fatalf("serving record delegated_node_id = %q, want empty (this node did the work)", r.DelegatedNodeID)
		}
	}
	if r, ok := byJob[transcribeJobID]; !ok {
		t.Fatalf("serving ledger missing transcription job %q", transcribeJobID)
	} else if r.JobType != "borrow:transcription" || r.WorkAction != "transcribe" || !r.ReceiptSigned {
		t.Fatalf("transcription record = %+v (second service served through the same gate/receipt path)", r)
	}

	// --- Requester ledger: records delegated_node_id + receipt_ids, keyed by the
	// same per-call job id, so both ledgers reference the same unit of work. This
	// is what a requester handler writes (carried onto the record by the
	// applyUsageMap extension, pinned in internal/worker). ---
	reqStore, err := usage.OpenStore(filepath.Join(t.TempDir(), "requester-usage.db"))
	if err != nil {
		t.Fatalf("open requester usage store: %v", err)
	}
	t.Cleanup(func() { _ = reqStore.Close() })
	reqRec := usage.UsageRecord{
		JobID:           embedJobIDs[0],
		JobType:         "file_index",
		Status:          "success",
		NodeID:          "requester-node",
		Origin:          "dispatched",
		WorkAction:      "index",
		DelegatedNodeID: resp.ServingNodeID, // == the serving receipt's node_id
		ReceiptIDs:      []string{resp.ReceiptID},
		StartedAt:       time.Now(),
		CompletedAt:     time.Now(),
	}
	if err := reqStore.Insert(reqRec); err != nil {
		t.Fatalf("insert requester record: %v", err)
	}
	got, err := reqStore.QueryUnsynced(10)
	if err != nil || len(got) != 1 {
		t.Fatalf("requester ledger query = %d records, err %v", len(got), err)
	}
	if got[0].DelegatedNodeID != "fabric-node-77" {
		t.Fatalf("requester delegated_node_id = %q, want the serving node id", got[0].DelegatedNodeID)
	}
	if len(got[0].ReceiptIDs) != 1 || got[0].ReceiptIDs[0] != resp.ReceiptID {
		t.Fatalf("requester receipt_ids = %v, want the serving receipt", got[0].ReceiptIDs)
	}
}
