package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		args = req.Params.Arguments
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 2,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "written"}}},
		})
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "act_k", MCPURL: srv.URL}
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
}

func TestCaptureNote_EmptyContentErrors(t *testing.T) {
	if _, err := CaptureNote(context.Background(), &Config{APIKey: "k"}, "n", "  ", "", ""); err == nil {
		t.Fatal("expected error on empty content")
	}
}
