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

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Hello World":           "hello-world",
		"  Railway Alignment!!": "railway-alignment",
		"already-kebab":         "already-kebab",
		"":                      "note",
		"UPPER_snake.Case":      "upper-snake-case",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCaptureNote_ForwardsArgs(t *testing.T) {
	var args map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
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
		args = req.Params.Arguments
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 2,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "written"}}},
		})
	}))
	defer srv.Close()

	cfg := &Config{APIKey: testAPIKey, MCPURL: srv.URL, Scopes: []string{ScopeRead, ScopeWrite}}
	out, err := CaptureNote(context.Background(), cfg, "My Note", "durable fact", "a desc", "aceteam")
	if err != nil {
		t.Fatalf("CaptureNote: %v", err)
	}
	if out != "written" {
		t.Fatalf("bad out: %q", out)
	}
	if args["name"] != "my-note" {
		t.Fatalf("name not slugified/forwarded: %v", args["name"])
	}
	if args["content"] != "durable fact" || args["scope"] != "aceteam" || args["source"] != "claude-code" {
		t.Fatalf("args not forwarded: %v", args)
	}
	if args["idempotency_key"] != "citadel-claude-session-my-note" {
		t.Fatalf("stable idempotency key not forwarded: %v", args)
	}
}

func TestCaptureNote_ResultIsError(t *testing.T) {
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
					"content": []map[string]any{{"type": "text", "text": "capture rejected"}},
				},
			})
		}
	}))
	defer srv.Close()

	_, err := CaptureNote(context.Background(), &Config{APIKey: testAPIKey, MCPURL: srv.URL, Scopes: []string{ScopeRead, ScopeWrite}}, "n", "fact", "", "")
	if err == nil || !strings.Contains(err.Error(), "capture rejected") {
		t.Fatalf("expected tool-level capture error, got %v", err)
	}
}

func TestCaptureNote_EmptyContentErrors(t *testing.T) {
	if _, err := CaptureNote(context.Background(), &Config{APIKey: "k", Scopes: []string{ScopeRead, ScopeWrite}}, "n", "  ", "", ""); err == nil {
		t.Fatal("expected error on empty content")
	}
}

func TestValidateCaptureContent_RejectsSecretsWithoutEcho(t *testing.T) {
	for _, content := range []string{
		"api_key=abcdefghijklmnopqrstuvwx",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nbody",
		"raw credential act_abcdefghijklmnopqrstuvwxyz0123456789",
	} {
		err := ValidateCaptureContent(content)
		if err == nil {
			t.Fatalf("accepted secret content: %q", content)
		}
		if strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwx") || strings.Contains(err.Error(), "BEGIN OPENSSH") {
			t.Fatalf("error echoed sensitive content: %v", err)
		}
	}
	if err := ValidateCaptureContent("Use a bounded retry and retain the primary error."); err != nil {
		t.Fatalf("ordinary prose rejected: %v", err)
	}
}
