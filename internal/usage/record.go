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

	// Sync status
	Synced bool
}
