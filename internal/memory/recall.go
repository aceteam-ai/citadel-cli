package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DefaultRecallBudget bounds the injected memory block (characters). Keeps the
// pre-prompt context small so recall never floods the model's context window.
const DefaultRecallBudget = 2000

// RecallHeader prefixes the injected block so the model knows the provenance.
const RecallHeader = "## AceTeam memory (recalled)"

// Recall queries the memory substrate via memory_search and returns a compact,
// character-bounded block suitable for injection by a UserPromptSubmit hook.
// Returns "" (no error) when there are no memories, so the hook injects nothing.
func Recall(ctx context.Context, cfg *Config, scope, query string, budget int) (string, error) {
	if cfg == nil || cfg.APIKey == "" {
		return "", fmt.Errorf("no memory API key configured")
	}
	if strings.TrimSpace(query) == "" {
		// Nothing to search on; a UserPromptSubmit with an empty prompt.
		return "", nil
	}
	if budget <= 0 {
		budget = DefaultRecallBudget
	}

	client := NewMCPClient(cfg.EffectiveMCPURL(), cfg.APIKey, 3*time.Second)

	args := map[string]any{"query": query}
	if scope != "" {
		args["scope"] = scope
	}

	text, err := client.CallTool(ctx, "memory_search", args)
	if err != nil {
		return "", err
	}
	return FormatRecall(text, budget), nil
}

// FormatRecall trims and bounds raw memory_search output into an injectable
// block. Returns "" when the result is empty or indicates no matches.
func FormatRecall(raw string, budget int) string {
	body := strings.TrimSpace(raw)
	if body == "" {
		return ""
	}
	// Common "no results" phrasings — inject nothing rather than noise.
	low := strings.ToLower(body)
	if strings.Contains(low, "no memories") || strings.Contains(low, "no results") ||
		strings.Contains(low, "no matching") || body == "[]" || body == "{}" {
		return ""
	}
	if budget > 0 && len(body) > budget {
		body = strings.TrimSpace(body[:budget]) + "\n…(truncated)"
	}
	return RecallHeader + "\n" + body
}
