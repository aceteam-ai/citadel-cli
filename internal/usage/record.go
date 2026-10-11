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

	// Sync status
	Synced bool
}
