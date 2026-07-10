package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClickJS_EscapesSelectorAndThrows(t *testing.T) {
	// A selector containing a quote must be safely embedded (not break the JS)
	// and the snippet must throw when the element is missing so a stale selector
	// surfaces as an error rather than a false success.
	sel := `button[aria-label="Leave call"]`
	js := clickJS(sel)
	marshaled, _ := json.Marshal(sel)
	if !strings.Contains(js, string(marshaled)) {
		t.Errorf("clickJS did not embed json-escaped selector; got: %s", js)
	}
	if !strings.Contains(js, "throw new Error") {
		t.Errorf("clickJS must throw on missing selector; got: %s", js)
	}
	if !strings.Contains(js, ".click()") {
		t.Errorf("clickJS must click the element; got: %s", js)
	}
}

func TestTypeJS_EscapesSelectorAndText(t *testing.T) {
	sel := `input[aria-label="Your name"]`
	text := `O'Brien "Bot" \ x`
	js := typeJS(sel, text)
	for _, want := range []string{string(mustJSON(t, sel)), string(mustJSON(t, text))} {
		if !strings.Contains(js, want) {
			t.Errorf("typeJS missing escaped %q; got: %s", want, js)
		}
	}
	if !strings.Contains(js, "throw new Error") {
		t.Errorf("typeJS must throw on missing selector; got: %s", js)
	}
	// Uses the native setter path so controlled (React) inputs observe the change.
	if !strings.Contains(js, "getOwnPropertyDescriptor") || !strings.Contains(js, "dispatchEvent") {
		t.Errorf("typeJS must set value via native setter + dispatch events; got: %s", js)
	}
}

func TestDescribeCDPException(t *testing.T) {
	cases := []struct {
		name string
		exc  any
		want string
	}{
		{
			name: "exception description",
			exc:  map[string]any{"exception": map[string]any{"description": "Error: selector not found"}},
			want: "Error: selector not found",
		},
		{
			name: "exception value fallback",
			exc:  map[string]any{"exception": map[string]any{"value": "boom"}},
			want: "boom",
		},
		{
			name: "text fallback",
			exc:  map[string]any{"text": "Uncaught"},
			want: "Uncaught",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeCDPException(tc.exc); got != tc.want {
				t.Errorf("describeCDPException = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFindFreeDebugPort(t *testing.T) {
	p, err := findFreeDebugPort()
	if err != nil {
		t.Fatalf("findFreeDebugPort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("findFreeDebugPort returned out-of-range port %d", p)
	}
}

// TestMeetingBrowser_Launch is an integration test: it launches a real Chromium
// on a real Xvfb display and drives one CDP round-trip. It skips under -short and
// wherever the browser/display deps are missing, mirroring audio_test.go's
// hardware-gated convention.
func TestMeetingBrowser_Launch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping meeting-browser launch test in -short mode")
	}
	if !ChromiumAvailable() || !XvfbAvailable() {
		t.Skip("no Chromium or Xvfb on this host; skipping meeting-browser launch test")
	}
	br := NewMeetingBrowser("citadel_meeting_gotest")
	if err := br.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer br.Close()
	if br.DebugPort() <= 0 {
		t.Fatalf("expected a CDP debug port after Start, got %d", br.DebugPort())
	}
	v, err := br.Evaluate("1+1")
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if n, ok := v.(float64); !ok || n != 2 {
		t.Fatalf("Evaluate(1+1) = %v (%T), want 2", v, v)
	}
	// A throwing expression must surface as a Go error, not a silent success.
	if _, err := br.Evaluate(`throw new Error("nope")`); err == nil {
		t.Fatal("expected error from throwing JS expression, got nil")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%v): %v", v, err)
	}
	return b
}
