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

	// Sync status
	Synced bool
}
