package harnessdriver

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
)

var (
	ErrCodexModelCatalog   = errors.New("harness_codex_model_catalog_invalid")
	ErrCodexModelLimit     = errors.New("harness_codex_model_catalog_limit")
	ErrCodexModelSelection = errors.New("harness_codex_model_selection_required")
)

const (
	maxCodexModelPages = 16
	maxCodexModels     = 128 // Matches the candidate models.reported payload.
	maxModelPageBytes  = 64 << 10
)

var codexModelSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// CodexModelPage is supplied correlation and response content, not proof that a
// request was sent to an initialized process. This subset uses string RPC IDs.
// The first requested cursor is empty; subsequent cursors must match nextCursor.
type CodexModelPage struct {
	RequestID string `json:"-"`
	Cursor    string `json:"-"`
	Response  []byte `json:"-"`
}

func (CodexModelPage) String() string   { return "supplied model page (content omitted)" }
func (CodexModelPage) GoString() string { return "supplied model page (content omitted)" }

// CodexModelCatalog is a complete bounded supplied catalog, not entitlement,
// freshness, authenticated provenance, vendor identity or launch permission.
// It is immutable after parsing. No parser or selector has a runtime caller.
type CodexModelCatalog struct {
	models   []string
	complete bool
}

func (CodexModelCatalog) String() string               { return "supplied model catalog (content omitted)" }
func (CodexModelCatalog) GoString() string             { return "supplied model catalog (content omitted)" }
func (CodexModelCatalog) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

// ParseCodexModelCatalog parses all pages atomically. The caller supplies an
// ordered snapshot from one discovery operation, provider and process context;
// this function cannot independently establish any of those properties.
// Malformed, incomplete, ambiguous, replayed or out-of-order pages return no
// catalog. Only visible model slugs are retained; labels, defaults, descriptions
// and unprojected capabilities never become selection fallbacks or diagnostics.
func ParseCodexModelCatalog(pages []CodexModelPage) (*CodexModelCatalog, error) {
	if len(pages) == 0 {
		return nil, ErrCodexModelCatalog
	}
	if len(pages) > maxCodexModelPages {
		return nil, ErrCodexModelLimit
	}
	models := make([]string, 0)
	requestIDs, cursors := map[string]bool{}, map[string]bool{}
	ids, slugs := map[string]bool{}, map[string]bool{}
	next := ""
	rows := 0
	for index, page := range pages {
		if !boundedCatalogText(page.RequestID, 128) || requestIDs[page.RequestID] || page.Cursor != next {
			return nil, ErrCodexModelCatalog
		}
		requestIDs[page.RequestID] = true
		if len(page.Response) > maxModelPageBytes {
			return nil, ErrCodexModelLimit
		}
		message, err := codexObject(page.Response, true)
		if err != nil {
			return nil, ErrCodexModelCatalog
		}
		for _, field := range []string{"method", "params", "error"} {
			if _, exists := message[field]; exists {
				return nil, ErrCodexModelCatalog
			}
		}
		requestID, err := codexString(message, "id")
		if err != nil || requestID != page.RequestID {
			return nil, ErrCodexModelCatalog
		}
		result, err := codexObject(message["result"], false)
		if err != nil {
			return nil, ErrCodexModelCatalog
		}
		var data []json.RawMessage
		if json.Unmarshal(result["data"], &data) != nil || data == nil {
			return nil, ErrCodexModelCatalog
		}
		rows += len(data)
		if rows > maxCodexModels {
			return nil, ErrCodexModelLimit
		}
		for _, raw := range data {
			model, err := codexObject(raw, false)
			if err != nil {
				return nil, ErrCodexModelCatalog
			}
			id, err := codexString(model, "id")
			if err != nil || !boundedCatalogText(id, 256) || ids[id] {
				return nil, ErrCodexModelCatalog
			}
			slug, err := codexString(model, "model")
			if err != nil || !codexModelSlug.MatchString(slug) || slugs[slug] {
				return nil, ErrCodexModelCatalog
			}
			// hidden is required in this supported subset. Unknown visibility
			// must not become a selectable model by a zero-value bool default.
			hiddenRaw, exists := model["hidden"]
			var hidden bool
			if !exists || bytes.Equal(bytes.TrimSpace(hiddenRaw), []byte("null")) || json.Unmarshal(hiddenRaw, &hidden) != nil {
				return nil, ErrCodexModelCatalog
			}
			ids[id], slugs[slug] = true, true
			if !hidden {
				models = append(models, slug)
			}
		}
		cursor, exists := result["nextCursor"]
		if !exists {
			return nil, ErrCodexModelCatalog
		}
		if bytes.Equal(bytes.TrimSpace(cursor), []byte("null")) {
			if index != len(pages)-1 {
				return nil, ErrCodexModelCatalog
			}
			return &CodexModelCatalog{models: models, complete: true}, nil
		}
		if json.Unmarshal(cursor, &next) != nil || !boundedCatalogText(next, 1024) || cursors[next] {
			return nil, ErrCodexModelCatalog
		}
		cursors[next] = true
	}
	return nil, ErrCodexModelCatalog // Last response still required another page.
}

// Models returns a copy suitable for a candidate models.reported payload. It
// does not construct an envelope or assert native/source/session provenance.
func (c *CodexModelCatalog) Models() (Models, error) {
	if c == nil || !c.complete {
		return Models{}, ErrCodexModelCatalog
	}
	ids := make([]string, len(c.models))
	copy(ids, c.models)
	return Models{IDs: ids}, nil
}

// Select requires an exact, explicit model slug present in the visible catalog.
// Native catalog IDs, labels, isDefault, configuration and aliases are never
// substituted. Membership still does not prove account access or clear a hold.
func (c *CodexModelCatalog) Select(requested string) (string, error) {
	if c == nil || !c.complete || !codexModelSlug.MatchString(requested) {
		return "", ErrCodexModelSelection
	}
	for _, model := range c.models {
		if model == requested {
			return model, nil
		}
	}
	return "", ErrCodexModelSelection
}

func boundedCatalogText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsRune(value, unicode.ReplacementChar) && !strings.ContainsFunc(value, unicode.IsControl)
}
