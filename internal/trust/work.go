package trust

import (
	"fmt"
	"sort"
	"strings"
)

// WorkCheckResult is the advisory, deterministic verdict of CheckWork over a
// work record's typed units (aceteam#10876 C7). It is NOT folded into a signed
// receipt's verdict_hash — a work receipt is an event attestation, like
// app_deploy, with an empty verdict. CheckWork is a cheap sanity gate: it flags
// a record whose units are implausible (negative, or zero where the action
// requires non-zero), which the handler logs and surfaces on the usage record
// so "a FILE_INDEX that embedded nothing" or "an embedding job that metered zero
// tokens" is visible rather than silently recorded as healthy.
type WorkCheckResult struct {
	// OK is true when every unit is non-negative and the per-action minimums
	// hold. A true result is NOT a correctness proof — only that the units are
	// plausible for the declared action.
	OK bool
	// Reason is a short, content-free explanation when OK is false (unit names
	// and the action only — never a path, text, or embedding value).
	Reason string
}

// Work action identifiers CheckWork recognizes. They mirror aep's work-action
// constants (kept as string literals here so internal/trust stays a leaf and
// does not import internal/aep, which already imports internal/trust).
const (
	workActionIndex       = "index"
	workActionEmbedServed = "embed_served"
)

// CheckWork validates a work record's typed units deterministically. It never
// performs I/O and never inspects content. Contract:
//
//   - Any negative unit quantity ⇒ not OK (a count can never be negative).
//   - For an embedding-serving record (embed_served): total_tokens AND one of
//     request_bytes/response_bytes must be > 0 — the "tokens and bytes are
//     non-zero for embedding jobs" acceptance (aceteam#10876 C7). A served
//     embedding that metered zero tokens/bytes is a metering bug, flagged here.
//   - For an index record: a non-cancelled run that indexed files must have
//     upserted chunks; files_indexed>0 with chunks_upserted==0 is flagged. An
//     index run that indexed zero files (everything unchanged/skipped) is OK —
//     a no-op reindex is legitimate work, not a failure.
//
// units is the same typed-unit map the handler attaches to the usage record; a
// missing key is treated as 0.
func CheckWork(action string, units map[string]int64) WorkCheckResult {
	// Negatives are never valid, regardless of action.
	var bad []string
	for k, v := range units {
		if v < 0 {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return WorkCheckResult{OK: false, Reason: fmt.Sprintf("negative unit(s): %s", strings.Join(bad, ","))}
	}

	switch action {
	case workActionEmbedServed:
		if units["total_tokens"] <= 0 {
			return WorkCheckResult{OK: false, Reason: "embed_served recorded zero tokens"}
		}
		if units["request_bytes"] <= 0 && units["response_bytes"] <= 0 {
			return WorkCheckResult{OK: false, Reason: "embed_served recorded zero bytes"}
		}
	case workActionIndex:
		if units["files_indexed"] > 0 && units["chunks_upserted"] <= 0 {
			return WorkCheckResult{OK: false, Reason: "index recorded files but zero chunks"}
		}
	}
	return WorkCheckResult{OK: true}
}
