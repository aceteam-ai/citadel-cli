// cmd/borrow.go
//
// cmd-side wiring for the SERVING half of peer-service borrowing
// (citadel-cli#1289). It builds the gateway.BorrowServeConfig: the two first
// adopters (embedding -> loopback TEI, transcription -> loopback whisper) share
// ONE gate and ONE receipt/record path, so the second adopter needs no new gate
// code. The aep signing + usage recording (which internal/gateway must not
// import) live here as the injected Preflight/Record hooks, reusing #1272/#1287's
// jobs.WorkReceiptConfig exactly (never a parallel receipt path).
//
// The requester half's production transport is serviceborrow.NewMeshGateway
// (verified mesh identity + pinned leaf), wired by #1270's first borrowing
// caller as the documented two-line network.WhoIsPeer/network.Dial adapter; it
// is intentionally not given a live caller here (FILE_INDEX borrowing is #1270).
package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/serviceborrow"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	"github.com/aceteam-ai/citadel-cli/services"
)

// borrowServerDeps are everything buildBorrowServeConfig needs from runWork. A
// nil Signer/InsertUsage/Providers get safe defaults; a test injects fakes so no
// real node identity, usage DB, or loopback service is touched.
type borrowServerDeps struct {
	// Signer signs serving work receipts (the machine-convergent node identity).
	Signer aep.Signer
	// NodeID is the preferred receipt node_id (fabric id); empty => signer
	// fingerprint (aep.ResolveNodeID).
	NodeID string
	// NodeName is the usage-record NodeID (matches runWork's jobRecordFn).
	NodeName string
	// InsertUsage writes a work record (nil => serving still signs + echoes, just
	// no local ledger row — e.g. a node with usage tracking disabled).
	InsertUsage func(usage.UsageRecord) error
	// AllowedLogins reads the platform-infra allowlist (same platform-exposure.json
	// the gateway's platform exposures use). nil => same-owner only.
	AllowedLogins func() (map[string]bool, error)
	// Providers overrides the default adopters (embedding + transcription). Tests
	// inject providers pointed at httptest upstreams.
	Providers map[string]*gateway.BorrowProvider
	// Now is the clock for usage records (nil => time.Now).
	Now func() time.Time
}

// buildBorrowServeConfig assembles the serving config. Preflight fails the serve
// closed when the node cannot sign (never serve unsigned dispatched work);
// Record signs a v2 work receipt + writes a typed-unit work record for a served
// call, returning the serving receipt's node_id and a receipt identifier to echo
// to the requester.
func buildBorrowServeConfig(deps borrowServerDeps) *gateway.BorrowServeConfig {
	providers := deps.Providers
	if providers == nil {
		providers = defaultBorrowProviders()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	preflightCfg := jobs.WorkReceiptConfig{Signer: deps.Signer, NodeID: deps.NodeID, Dispatched: true, Origin: jobs.OriginDispatched}

	return &gateway.BorrowServeConfig{
		AllowedLogins: deps.AllowedLogins,
		Providers:     providers,
		Preflight:     preflightCfg.Preflight,
		Record: func(kind, action, jobID string, peer serviceborrow.PeerIdentity, reqBody, respBody []byte) (string, string, error) {
			t0 := now()
			cfg := jobs.WorkReceiptConfig{Signer: deps.Signer, NodeID: deps.NodeID, Dispatched: true, Origin: jobs.OriginDispatched}
			// The input manifest BINDS the verified requester's stable device +
			// owner id (plus kind + request hash), mirroring how the index manifest
			// binds its namespace: two different requesters' byte-identical requests
			// produce distinct receipts, and the receipt attests WHO borrowed.
			inputManifest := borrowInputManifest{
				Kind:              kind,
				Action:            action,
				RequesterStableID: peer.StableID,
				RequesterOwnerID:  peer.OwnerID,
				RequestSHA256:     borrowBodyHash(reqBody),
			}
			outputManifest := borrowOutputManifest{ResponseSHA256: borrowBodyHash(respBody)}
			units := map[string]int64{
				"requests":       1,
				"request_bytes":  int64(len(reqBody)),
				"response_bytes": int64(len(respBody)),
			}
			out := map[string]any{}
			if err := cfg.AttachRunnerWorkReceipt(out, jobID, action, inputManifest, outputManifest, units); err != nil {
				return "", "", err // fail closed (ErrReceiptSigningUnavailable propagates)
			}
			servingNodeID, err := aep.ResolveNodeID(deps.Signer, deps.NodeID)
			if err != nil {
				return "", "", err
			}
			receiptID := borrowReceiptID(out)

			// Write the serving-side work record. DelegatedNodeID is empty: THIS
			// node did the work (it is the requester's delegate, not a further
			// delegator). JobID == the serving receipt's job id == one entry in the
			// requester's receipt lineage, so both ledgers reference this unit by id.
			if deps.InsertUsage != nil {
				rec := usage.UsageRecord{
					JobID:                 jobID,
					JobType:               "borrow:" + kind,
					Status:                "success",
					NodeID:                deps.NodeName,
					StartedAt:             t0,
					CompletedAt:           now(),
					Origin:                jobs.OriginDispatched,
					WorkAction:            action,
					Units:                 units,
					ReceiptManifestSHA256: stringFromMap(out, "work_manifest_sha256"),
					ReceiptSigned:         true,
					RequestBytes:          int64(len(reqBody)),
					ResponseBytes:         int64(len(respBody)),
				}
				if err := deps.InsertUsage(rec); err != nil {
					Debug("borrow: serving usage insert error: %v", err)
				}
			}
			return servingNodeID, receiptID, nil
		},
	}
}

// defaultBorrowProviders are the two first adopters, pointed at their loopback
// services. The advertised model is intentionally unset here (the generic
// contract advertises kind/version); #1270 wires embedding's real /info-derived
// model + dtype/dimension checks. Ready is a bounded loopback TCP dial.
func defaultBorrowProviders() map[string]*gateway.BorrowProvider {
	embeddingUpstream := fmt.Sprintf("127.0.0.1:%d", services.TEIEmbeddingPort)
	transcribeUpstream := fmt.Sprintf("127.0.0.1:%d", services.TranscribePort)
	return map[string]*gateway.BorrowProvider{
		"embedding": {
			Kind:            "embedding",
			Upstream:        embeddingUpstream,
			AllowedSubpaths: map[string]bool{"/v1/embeddings": true},
			WorkAction:      aep.WorkActionEmbedServed,
			Identity:        func() serviceborrow.ServiceIdentity { return serviceborrow.ServiceIdentity{Kind: "embedding"} },
			Capacity:        defaultBorrowCapacity,
			Ready:           func() bool { return loopbackReady(embeddingUpstream) },
		},
		"transcription": {
			Kind:            "transcription",
			Upstream:        transcribeUpstream,
			AllowedSubpaths: map[string]bool{"/transcribe": true},
			WorkAction:      aep.WorkActionTranscribe,
			Identity:        func() serviceborrow.ServiceIdentity { return serviceborrow.ServiceIdentity{Kind: "transcription"} },
			Capacity:        defaultBorrowCapacity,
			Ready:           func() bool { return loopbackReady(transcribeUpstream) },
		},
	}
}

// defaultBorrowCapacity advertises a coarse device hint (GPU presence) so a
// requester's auto-select can prefer a GPU peer. Queue depth is not tracked yet.
func defaultBorrowCapacity() serviceborrow.Capacity {
	device := "cpu"
	if platform.GetGPUCountSimple() > 0 {
		device = "cuda"
	}
	return serviceborrow.Capacity{Device: device}
}

// loopbackReady reports whether a loopback service accepts a TCP connection
// within a short bound. A crude liveness probe; a requester never borrows from a
// not-ready peer.
func loopbackReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// borrowInputManifest / borrowOutputManifest are the per-call manifest structs
// whose deterministic JSON is hashed into the serving receipt's
// input_sha256/output_sha256 (opaque to the verifier, which checks the signature
// over the canonical receipt). Field order is fixed by declaration.
type borrowInputManifest struct {
	Kind              string `json:"kind"`
	Action            string `json:"action"`
	RequesterStableID string `json:"requester_stable_id"`
	RequesterOwnerID  string `json:"requester_owner_id"`
	RequestSHA256     string `json:"request_sha256"`
}

type borrowOutputManifest struct {
	ResponseSHA256 string `json:"response_sha256"`
}

// borrowReceiptID derives a stable identifier for the signed serving receipt
// from its signature, so the requester can carry it in receipt_ids. Empty when
// no signed receipt is present (should not happen on the dispatched success path).
func borrowReceiptID(out map[string]any) string {
	recs, _ := out["work_receipts"].([]map[string]any)
	if len(recs) == 0 {
		return ""
	}
	sig, _ := recs[0]["signature"].(string)
	if sig == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sig))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// borrowBodyHash returns "sha256:<hex>" over a body, for the opaque manifest
// digests. It reuses the package-level sha256Hex (bare hex) from cmd/app_ops.go.
func borrowBodyHash(b []byte) string {
	return "sha256:" + sha256Hex(string(b))
}

func stringFromMap(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
