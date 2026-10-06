package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFormatRecall(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		budget int
		want   string // "" means expect empty
		sub    string // substring expected when non-empty
	}{
		{"empty", "  ", 100, "", ""},
		{"no memories", "No memories found.", 100, "", ""},
		{"empty array", "[]", 100, "", ""},
		{"has content", "- railway needs socks5 relay", 100, "x", "railway needs socks5 relay"},
	}
	for _, c := range cases {
		got := FormatRecall(c.raw, c.budget)
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: expected empty, got %q", c.name, got)
			}
			continue
		}
		if !strings.HasPrefix(got, RecallHeader) {
			t.Errorf("%s: missing header: %q", c.name, got)
		}
		if !strings.Contains(got, c.sub) {
			t.Errorf("%s: want substring %q in %q", c.name, c.sub, got)
		}
	}
}

func TestFormatRecall_Budget(t *testing.T) {
	raw := strings.Repeat("a", 5000)
	got := FormatRecall(raw, 100)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker, got len %d", len(got))
	}
	if len(got) > 200 {
		t.Fatalf("budget not applied: len %d", len(got))
	}
}

func TestFormatRecall_BudgetPreservesUTF8(t *testing.T) {
	got := FormatRecall("猫犬鳥馬", 3)
	if !strings.Contains(got, "猫犬鳥") || strings.ContainsRune(got, '�') || !strings.Contains(got, "truncated") {
		t.Fatalf("recall was not rune-bounded cleanly: %q", got)
	}
}

func TestRecall_EndToEnd(t *testing.T) {
	var sawQuery, sawScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			writeInitialize(w, "")
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		}
		sawQuery, _ = req.Params.Arguments["query"].(string)
		sawScope, _ = req.Params.Arguments["scope"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 2,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "remembered: X"}}},
		})
	}))
	defer srv.Close()

	cfg := &Config{APIKey: testAPIKey, MCPURL: srv.URL, Scopes: []string{ScopeRead, ScopeWrite}}
	block, err := Recall(context.Background(), cfg, "aceteam", "how does railway reach nodes", 0)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.Contains(block, "remembered: X") {
		t.Fatalf("bad block: %q", block)
	}
	if sawQuery != "how does railway reach nodes" {
		t.Fatalf("query not forwarded: %q", sawQuery)
	}
	if sawScope != "aceteam" {
		t.Fatalf("scope not forwarded: %q", sawScope)
	}
}

func TestRecall_EmptyScopeOmitted(t *testing.T) {
	var hadScopeKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			writeInitialize(w, "")
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, hadScopeKey = req.Params.Arguments["scope"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 2,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "hit"}}},
		})
	}))
	defer srv.Close()

	cfg := &Config{APIKey: testAPIKey, MCPURL: srv.URL, Scopes: []string{ScopeRead, ScopeWrite}}
	// Empty scope => search ALL scopes => no "scope" key in the arguments.
	if _, err := Recall(context.Background(), cfg, "", "any query", 0); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if hadScopeKey {
		t.Fatal("empty scope should be omitted (search all scopes), but scope key was sent")
	}
}

func TestRecall_ResultIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, _, _ := decodeReq(t, r)
		switch method {
		case "initialize":
			writeInitialize(w, "")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 2,
				"result": map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": "recall rejected"}},
				},
			})
		}
	}))
	defer srv.Close()

	_, err := Recall(context.Background(), &Config{APIKey: testAPIKey, MCPURL: srv.URL, Scopes: []string{ScopeRead, ScopeWrite}}, "", "query", 100)
	if err == nil || !strings.Contains(err.Error(), "recall rejected") {
		t.Fatalf("expected tool-level recall error, got %v", err)
	}
}

func TestRecall_NoKeyErrors(t *testing.T) {
	_, err := Recall(context.Background(), &Config{}, "", "q", 0)
	if err == nil {
		t.Fatal("expected error with no API key")
	}
}
