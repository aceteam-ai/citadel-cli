package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// CaptureNote persists a durable note to the memory substrate via memory_write.
// name is a kebab-case slug; scope defaults to "global" server-side when empty.
// Returns the tool output (best-effort).
func CaptureNote(ctx context.Context, cfg *Config, name, content, description, scope string) (string, error) {
	if err := cfg.ValidateCredential(); err != nil {
		return "", err
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("empty content; nothing to capture")
	}
	if err := ValidateCaptureContent(content); err != nil {
		return "", err
	}
	client := NewMCPClient(cfg.EffectiveMCPURL(), cfg.APIKey, 8*time.Second)

	args := map[string]any{
		"name":    Slugify(name),
		"content": content,
		"source":  "claude-code",
		// Stable names make SessionEnd retries update one memory. The explicit
		// key also lets the backend collapse a network retry before re-running
		// its write path.
		"idempotency_key": "citadel-claude-session-" + Slugify(name),
	}
	if description != "" {
		args["description"] = description
	}
	if scope != "" {
		args["scope"] = scope
	}
	return client.CallTool(ctx, "memory_write", args)
}

// ValidateCaptureContent fails closed before any memory_write request when
// the local deterministic detector finds credential material. It deliberately
// does not scrub or rewrite prose: the caller can decide what safe summary to
// provide instead, while the matched secret is never included in the error.
func ValidateCaptureContent(content string) error {
	if findings := trust.CheckSecrets("", content); len(findings) > 0 {
		return fmt.Errorf("refused content containing a detected secret (%s)", findings[0].Type)
	}
	return nil
}

// Slugify converts arbitrary text into a bounded kebab-case slug suitable for a
// memory name.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = "note"
	}
	return slug
}
