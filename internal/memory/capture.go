package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CaptureNote persists a durable note to the memory substrate via memory_write.
// name is a kebab-case slug; scope defaults to "global" server-side when empty.
// Returns the tool output (best-effort).
func CaptureNote(ctx context.Context, cfg *Config, name, content, description, scope string) (string, error) {
	if cfg == nil || cfg.APIKey == "" {
		return "", fmt.Errorf("no memory API key configured")
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("empty content; nothing to capture")
	}
	client := NewMCPClient(cfg.EffectiveMCPURL(), cfg.APIKey, 8*time.Second)

	args := map[string]any{
		"name":    Slugify(name),
		"content": content,
		"source":  "claude-code",
	}
	if description != "" {
		args["description"] = description
	}
	if scope != "" {
		args["scope"] = scope
	}
	return client.CallTool(ctx, "memory_write", args)
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
