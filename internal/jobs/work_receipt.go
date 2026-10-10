// internal/jobs/work_receipt.go
//
// Signed work receipts + typed-unit work records for dispatched node work
// (aceteam#10876 C7, citadel-cli#1272). A unit of work a node performs under
// `citadel work` — indexing (FILE_INDEX) or serving an embedding (embedding) —
// attaches a v2 AEP receipt (aep.BuildSignedWorkReceipt) and a typed-unit
// `usage` record to its job output, so the fabric can meter and audit it.
//
// FAIL-CLOSED, and ONLY for dispatched work. This is the DELIBERATE OPPOSITE of
// the chat-completion AEP receipt (CITADEL_SIGN_AEP_RECEIPTS: opt-in, fail-open).
// A dispatched work action signs by DEFAULT (no env flag), and if signing is
// unavailable (no node identity key, a Sign error) the job FAILS with
// ErrReceiptSigningUnavailable rather than silently proceeding unsigned. The
// LOCAL CLI path (`citadel rag index`, origin=local_cli) is a trusted operator,
// NOT dispatched work: it records to the same ledger but a missing key there is
// not a hard failure. The distinction is explicit: Dispatched=true + a Signer is
// set only by the worker construction sites; the zero value (what rag.Service and
// the C1/C2 tests get) never signs and never fails closed.
package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// ErrReceiptSigningUnavailable is a TERMINAL failure for a dispatched work job
// whose receipt could not be signed (no node identity key, or a Sign error). It
// mirrors ErrFilesDisabled: a retry cannot make a missing key appear, so the
// runner routes it to source.Fail (DLQ) with reason "receipt_signing_unavailable"
// and publishes exactly one terminal error event — never a silent unsigned
// success. The message is a JSON object so the backend can branch on `reason`.
var ErrReceiptSigningUnavailable = errors.New(`{"reason":"receipt_signing_unavailable","message":"This node could not sign a work receipt (no node identity key available). Dispatched work requires a signed receipt. Re-run 'citadel init' to provision the node identity, then retry."}`)

// Work-action identifiers re-exported from internal/aep so handlers reference
// one authority without each importing aep.
const (
	WorkActionIndex       = aep.WorkActionIndex
	WorkActionEmbedServed = aep.WorkActionEmbedServed
)

// Work origin identifiers carried on the usage record (attribution).
const (
	// OriginDispatched marks work executed under `citadel work` from a dispatched
	// job. It signs its receipt and fails closed when signing is unavailable.
	OriginDispatched = "dispatched"
	// OriginLocalCLI marks work driven in-process by the trusted local operator
	// (`citadel rag index`). It records to the same ledger but does not sign and
	// never fails closed.
	OriginLocalCLI = "local_cli"
)

// WorkReceiptConfig carries everything a handler needs to produce a signed work
// receipt and a typed-unit record. The zero value is the LOCAL / untracked
// posture: no signer, Dispatched=false — no receipt is produced and signing can
// never fail the job. Worker construction sites set Signer + Dispatched=true.
type WorkReceiptConfig struct {
	// Signer signs the receipt. nil on the local/zero-value path. On the
	// dispatched path a nil (or failing) signer is a terminal job failure.
	Signer aep.Signer
	// NodeID is the preferred receipt node_id (the fabric/platform node id). Empty
	// falls back to the signer's own public-key fingerprint (aep.ResolveNodeID).
	NodeID string
	// Dispatched gates the fail-closed posture. true only on the worker path.
	Dispatched bool
	// Origin is the attribution written to the usage record (OriginDispatched or
	// OriginLocalCLI).
	Origin string
	// OrgID is the owning org for the work (attribution). Derived from the index
	// namespace when present, else a payload org_id.
	OrgID string
	// now is a test seam for the receipt's issued_at; nil uses time.Now.
	now func() time.Time
}

// nowFn returns the configured clock or time.Now.
func (c WorkReceiptConfig) nowFn() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Preflight fails fast (before any expensive work) when dispatched work cannot
// sign: a nil signer, or a signer that cannot even resolve its public-key
// fingerprint. A keyless node then fails a FILE_INDEX in milliseconds rather
// than after an hour of embedding. Non-dispatched callers are never gated.
func (c WorkReceiptConfig) Preflight() error {
	if !c.Dispatched {
		return nil
	}
	if c.Signer == nil {
		return ErrReceiptSigningUnavailable
	}
	if _, err := c.Signer.PublicKeyFingerprint(); err != nil {
		return ErrReceiptSigningUnavailable
	}
	return nil
}

// manifestSHA256 returns "sha256:<hex>" over the deterministic JSON encoding of
// v. v must be a struct (field order fixed by declaration) with any slices
// pre-sorted by the caller, so the encoding is stable. The digest is the
// receipt's opaque input_sha256 / output_sha256 — the verifier does not
// recompute it from v, only checks the signature over the canonical receipt, so
// this just needs to be internally consistent and deterministic.
func manifestSHA256(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// attachWorkRecord computes the input/output manifest hashes, (when dispatched)
// signs a work receipt, runs the advisory trust.CheckWork sanity gate, and
// writes the additive `usage` / `work_manifest_sha256` / `work_receipts` keys
// into out. It NEVER touches an existing key in out (the C1/C2 result-shape
// contract). It returns ErrReceiptSigningUnavailable when dispatched work cannot
// sign — the handler must propagate that as its error (terminal, fail-closed).
//
// inputManifest / outputManifest are the per-stage manifest structs (the input
// manifest MUST bind the index namespace so two orgs' byte-identical content
// produce distinct receipts). units is the typed-unit map the ledger records.
func (c WorkReceiptConfig) attachWorkRecord(out map[string]any, jobID, action string, inputManifest, outputManifest any, units map[string]int64) error {
	inputSHA, err := manifestSHA256(inputManifest)
	if err != nil {
		return err
	}
	outputSHA, err := manifestSHA256(outputManifest)
	if err != nil {
		return err
	}

	signed := false
	if c.Signer != nil {
		nodeID, err := aep.ResolveNodeID(c.Signer, c.NodeID)
		if err != nil {
			if c.Dispatched {
				return ErrReceiptSigningUnavailable
			}
		} else {
			receipt, err := aep.BuildSignedWorkReceipt(c.Signer, nodeID, jobID, action, inputSHA, outputSHA, c.nowFn())
			if err != nil {
				if c.Dispatched {
					return ErrReceiptSigningUnavailable
				}
			} else {
				m, err := receipt.ToMap()
				if err != nil {
					if c.Dispatched {
						return ErrReceiptSigningUnavailable
					}
				} else {
					out["work_receipts"] = []map[string]any{m}
					signed = true
				}
			}
		}
	} else if c.Dispatched {
		// Dispatched work with no signer at all: fail closed. (Preflight should
		// already have caught this; this is the belt-and-suspenders at emit time.)
		return ErrReceiptSigningUnavailable
	}

	check := trust.CheckWork(action, units)
	// Merge INTO an existing `usage` object when the handler pre-seeded one (the
	// embedding handler carries prompt_tokens/total_tokens/request_bytes/
	// response_bytes there, which must survive back-compat). Otherwise start
	// fresh. We only ADD the attribution/units/receipt keys; pre-existing keys
	// are left intact.
	usage, _ := out["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
	}
	usage["action"] = action
	usage["origin"] = c.Origin
	usage["units"] = unitsToAny(units)
	usage["manifest_sha256"] = inputSHA
	usage["signed"] = signed
	usage["check_ok"] = check.OK
	if c.OrgID != "" {
		usage["org_id"] = c.OrgID
	}
	if !check.OK {
		usage["check_reason"] = check.Reason
	}
	out["usage"] = usage
	out["work_manifest_sha256"] = inputSHA
	return nil
}

// unitsToAny converts a typed-unit map to map[string]any so it round-trips
// through JSON as plain numbers (the shape buildUsageRecord decodes).
func unitsToAny(units map[string]int64) map[string]any {
	m := make(map[string]any, len(units))
	for k, v := range units {
		m[k] = v
	}
	return m
}

// orgFromNamespace extracts the `org_<id>` prefix of a coordinator-set index
// namespace (`org_<id>/<name>`) for attribution. Returns "" when ns is empty or
// not namespace-shaped.
func orgFromNamespace(ns string) string {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return ""
	}
	if i := strings.IndexByte(ns, '/'); i > 0 {
		head := ns[:i]
		if strings.HasPrefix(head, "org_") {
			return head
		}
	}
	return ""
}
