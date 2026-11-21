package jobs

import (
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestParseTypeAction(t *testing.T) {
	long := strings.Repeat("a", platform.MaxCobrowseSelectorLen+1)
	cases := []struct {
		name    string
		payload map[string]string
		want    platform.TypeOptions
		text    string
		wantErr string
	}{
		{"plain", map[string]string{"session_id": "s", "text": "hi"}, platform.TypeOptions{}, "hi", ""},
		{"selector and clear", map[string]string{"session_id": "s", "text": "hi", "selector": "#a", "clear": "true"}, platform.TypeOptions{Selector: "#a", Clear: true}, "hi", ""},
		{"empty clear", map[string]string{"session_id": "s", "clear": "true"}, platform.TypeOptions{Clear: true}, "", ""},
		{"clear false", map[string]string{"session_id": "s", "text": "x", "clear": "false"}, platform.TypeOptions{}, "x", ""},
		{"no session", map[string]string{"text": "x"}, platform.TypeOptions{}, "", "session_id"},
		{"empty text no clear", map[string]string{"session_id": "s"}, platform.TypeOptions{}, "", "empty text"},
		{"empty text clear false", map[string]string{"session_id": "s", "clear": "false"}, platform.TypeOptions{}, "", "empty text"},
		{"bad clear", map[string]string{"session_id": "s", "text": "x", "clear": "yes"}, platform.TypeOptions{}, "", "clear"},
		{"long selector", map[string]string{"session_id": "s", "text": "x", "selector": long}, platform.TypeOptions{}, "", "selector"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, text, opts, err := parseTypeAction(c.payload)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts != c.want || text != c.text {
				t.Fatalf("got %q %+v", text, opts)
			}
		})
	}
}

func TestParseKeyAction(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "0": 0, "2": 2, "4": 4, "8": 8} {
		p := map[string]string{"session_id": "s", "key": "a"}
		if raw != "" {
			p["modifiers"] = raw
		}
		_, _, got, err := parseKeyAction(p)
		if err != nil || got != want {
			t.Fatalf("modifiers %q = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"16", "-1", "1.5", "abc", " 2", "+2", "02", "1e1"} {
		_, _, _, err := parseKeyAction(map[string]string{"session_id": "s", "key": "a", "modifiers": raw})
		if !errors.Is(err, platform.ErrModifiersOutRange) {
			t.Fatalf("modifiers %q: err = %v, want refusal", raw, err)
		}
	}
	// Clipboard and editing chords are not allowed.
	for _, c := range []struct{ key, mods string }{
		{"v", "2"}, {"c", "2"}, {"x", "2"}, {"v", "4"}, {"c", "4"}, {"x", "4"},
		{"Delete", "8"}, {"z", "2"}, {"y", "4"}, {"ArrowLeft", "1"}, {"a", "1"}, {"a", "6"}, {"a", "15"}, {"a", "10"},
	} {
		_, _, _, err := parseKeyAction(map[string]string{"session_id": "s", "key": c.key, "modifiers": c.mods})
		if !errors.Is(err, platform.ErrKeyChordNotAllowed) {
			t.Fatalf("%s+%s: err = %v, want refusal", c.mods, c.key, err)
		}
	}
	for _, c := range []struct{ key, mods string }{{"Home", "2"}, {"Tab", "8"}, {"a", "4"}, {"ArrowDown", "8"}} {
		if _, _, _, err := parseKeyAction(map[string]string{"session_id": "s", "key": c.key, "modifiers": c.mods}); err != nil {
			t.Fatalf("%s+%s: %v", c.mods, c.key, err)
		}
	}
	if _, _, _, err := parseKeyAction(map[string]string{"session_id": "s"}); err == nil {
		t.Fatal("missing key must be refused")
	}
}
