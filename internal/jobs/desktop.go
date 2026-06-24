// internal/jobs/desktop.go
package jobs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png" // register PNG decoder for image.DecodeConfig
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/desktop"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// ScreenshotHandler captures the node's display and returns a base64-encoded
// PNG. It backs the FILE_SCREENSHOT and VNC_SCREENSHOT job types, which the
// aceteam `desktop_screenshot` / `vnc_screenshot` MCP tools dispatch over the
// Redis mesh (issue #4179). Without this handler those tools time out (~15s)
// despite the UI looking production-grade -- the "demo trap" the epic #4168
// scoping pass identified.
//
// Both job types capture the same X11 framebuffer via the existing
// internal/desktop capture path; there is no separate VNC-framebuffer source.
type ScreenshotHandler struct{}

// Execute captures a screenshot and returns it base64-encoded.
//
// Payload fields (advisory; capture is always PNG):
//   - format:  requested image format (ignored; always PNG)
//   - quality: requested JPEG quality (ignored; PNG is lossless)
//
// Response JSON:
//   - image:    standard base64 of the raw PNG bytes (the field the
//     coordinator/MCP tool reads as result["image"])
//   - encoding: always "base64"
//   - format:   always "png"
//   - width:    decoded pixel width as a string (best-effort)
//   - height:   decoded pixel height as a string (best-effort)
func (h *ScreenshotHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	// JobContext carries no context.Context; CaptureScreenshot applies its own
	// internal timeout, so Background is safe here.
	png, err := desktop.CaptureScreenshot(context.Background())
	if err != nil {
		return nil, fmt.Errorf("screenshot capture failed: %w", err)
	}
	if len(png) == 0 {
		return nil, fmt.Errorf("screenshot capture returned no image data")
	}

	ctx.Log("info", "     - [Job %s] %s captured (%d bytes)", job.ID, job.Type, len(png))

	result := map[string]any{
		"image":    base64.StdEncoding.EncodeToString(png),
		"encoding": "base64",
		"format":   "png",
	}

	// Best-effort dimensions to match the documented wire contract
	// ({"image","width","height"}). DecodeConfig reads only the header, so this
	// is cheap and never fails the job if the format is unexpected.
	if cfg, _, derr := image.DecodeConfig(bytes.NewReader(png)); derr == nil {
		result["width"] = fmt.Sprintf("%d", cfg.Width)
		result["height"] = fmt.Sprintf("%d", cfg.Height)
	}

	return json.Marshal(result)
}

// Ensure ScreenshotHandler implements JobHandler.
var _ JobHandler = (*ScreenshotHandler)(nil)

// TypeHandler types literal text on the node's display. It backs the VNC_TYPE
// job type behind the aceteam `vnc_type` MCP tool (issue #4179), reusing the
// existing internal/desktop input path.
type TypeHandler struct{}

// Execute types the payload's `text` field on the display.
//
// Payload fields:
//   - text: the literal text to type (required, non-empty)
//
// Response JSON: {"ok": true, "typed": <char-count>}.
func (h *TypeHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	text, ok := job.Payload["text"]
	if !ok || text == "" {
		return nil, fmt.Errorf("job payload missing 'text' field")
	}

	actions, err := buildTypeActions(text)
	if err != nil {
		return nil, err
	}

	ctx.Log("info", "     - [Job %s] VNC_TYPE typing %d chars", job.ID, len(text))

	if err := desktop.ExecuteActions(context.Background(), actions); err != nil {
		return nil, fmt.Errorf("type failed: %w", err)
	}

	return json.Marshal(map[string]any{"ok": true, "typed": len(text)})
}

// Ensure TypeHandler implements JobHandler.
var _ JobHandler = (*TypeHandler)(nil)

// KeysHandler sends a key combination to the node's display. It backs the
// VNC_KEYS job type behind the aceteam `vnc_keys` MCP tool (issue #4179).
type KeysHandler struct{}

// Execute sends a key combo to the display.
//
// Payload fields (the MCP tool sends both; `keys` is authoritative):
//   - keys:  JSON array of X keysym names, e.g. `["ctrl","c"]` (preferred)
//   - combo: original human combo string, e.g. "Ctrl+C" (fallback/diagnostic)
//
// The keys are joined with '+' into a single xdotool `key` action
// (e.g. "ctrl+c").
//
// Response JSON: {"ok": true, "keys": "<combo>"}.
func (h *KeysHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	combo, err := buildKeyCombo(job.Payload["keys"], job.Payload["combo"])
	if err != nil {
		return nil, err
	}

	actions, err := buildKeyActions(combo)
	if err != nil {
		return nil, err
	}

	ctx.Log("info", "     - [Job %s] VNC_KEYS sending %q", job.ID, combo)

	if err := desktop.ExecuteActions(context.Background(), actions); err != nil {
		return nil, fmt.Errorf("keys failed: %w", err)
	}

	return json.Marshal(map[string]any{"ok": true, "keys": combo})
}

// Ensure KeysHandler implements JobHandler.
var _ JobHandler = (*KeysHandler)(nil)

// buildTypeActions translates literal text into a validated type action.
// Kept as a pure function so it can be unit-tested without a live display.
func buildTypeActions(text string) ([]desktop.Action, error) {
	payload, err := json.Marshal([]desktop.Action{{Type: "type", Text: text}})
	if err != nil {
		return nil, fmt.Errorf("failed to encode type action: %w", err)
	}
	return desktop.ParseActions(payload)
}

// buildKeyCombo resolves the key combo from the job payload. It prefers the
// `keys` JSON array (what the MCP tool computes) and falls back to the raw
// `combo` string. The result is a '+'-joined xdotool combo like "ctrl+c".
// Pure function: unit-testable without a display.
func buildKeyCombo(keysJSON, combo string) (string, error) {
	if keysJSON != "" {
		var keys []string
		if err := json.Unmarshal([]byte(keysJSON), &keys); err != nil {
			return "", fmt.Errorf("invalid 'keys' JSON array: %w", err)
		}
		cleaned := make([]string, 0, len(keys))
		for _, k := range keys {
			k = strings.TrimSpace(k)
			if k != "" {
				cleaned = append(cleaned, k)
			}
		}
		if len(cleaned) > 0 {
			return strings.Join(cleaned, "+"), nil
		}
	}
	if strings.TrimSpace(combo) != "" {
		return strings.TrimSpace(combo), nil
	}
	return "", fmt.Errorf("job payload missing 'keys' (or 'combo') field")
}

// buildKeyActions translates a key combo into a validated key action.
// Pure function: unit-testable without a live display.
func buildKeyActions(combo string) ([]desktop.Action, error) {
	payload, err := json.Marshal([]desktop.Action{{Type: "key", Key: combo}})
	if err != nil {
		return nil, fmt.Errorf("failed to encode key action: %w", err)
	}
	return desktop.ParseActions(payload)
}
