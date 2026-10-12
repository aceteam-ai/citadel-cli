package usage

import "time"

// UsageRecord captures compute usage metrics for a single job.
type UsageRecord struct {
	// Database ID (set after insert)
	ID int64

	// Job identification
	JobID   string
	JobType string
	Backend string
	Model   string

	// Outcome
	Status       string // "success", "failed", "retry"
	ErrorMessage string

	// Timing
	StartedAt   time.Time
	CompletedAt time.Time
	DurationMs  int64

	// Token usage (populated by handlers that support it)
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64

	// Size metrics
	RequestBytes  int64
	ResponseBytes int64

	// Node identification
	NodeID string

	// Work attribution + typed units (aceteam#10876 C7). Origin is "dispatched"
	// (a job run under `citadel work`) or "local_cli" (`citadel rag index`).
	// OrgID is the owning org (derived from an index namespace, or a payload
	// org_id). WorkAction is the typed work action ("index", "embed_served", ...).
	// Units is the typed-unit breakdown (files/chunks/tokens/bytes/...), stored as
	// JSON. ReceiptManifestSHA256 binds the record to its signed work receipt's
	// input manifest; ReceiptSigned reports whether a receipt was actually signed.
	Origin                string
	OrgID                 string
	WorkAction            string
	Units                 map[string]int64
	ReceiptManifestSHA256 string
	ReceiptSigned         bool

	// Flow attribution (aceteam#10876 / citadel-cli#1287). Populated from the job
	// envelope so central fabric_run_work can join this node's work to a flow run.
	// FlowRunID / FlowNodeID identify the orchestrating flow run and its node;
	// DelegatedNodeID is the node a parent delegated this work to; NodeUID is the
	// fabric/platform node UID from the envelope (distinct from NodeID, the
	// Headscale numeric id). ReceiptIDs are receipt identifiers carried by the
	// envelope (stored as JSON text).
	FlowRunID       string
	FlowNodeID      string
	DelegatedNodeID string
	NodeUID         string
	ReceiptIDs      []string

	// Signed work-receipt bodies (aceteam#11146 / citadel-cli#1291). Receipts
	// carries the FULL v2 AEP work-receipt maps the handler/runner already produced
	// in job output `work_receipts` (each element is a receipt's own ToMap() shape,
	// no wrapper), so central's verify_and_store_work_receipt() has the bytes to
	// attest — previously the node sent only ReceiptManifestSHA256, so every record
	// landed attestation=unattested. ALL of a job's receipts ride this ONE record
	// (central upserts ON CONFLICT (node_id, job_id) DO NOTHING). ReceiptsTruncated
	// is the count of receipts dropped to keep the serialized record well under
	// central's ~1 MB stream bound; it is never a silent drop — central marks a
	// record with ReceiptsTruncated>0 as partially attested.
	Receipts          []map[string]any
	ReceiptsTruncated int

	// Sync status
	Synced bool
}
