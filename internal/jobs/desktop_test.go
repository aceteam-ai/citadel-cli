// internal/jobs/desktop_test.go
package jobs

import "testing"

func TestBuildKeyCombo(t *testing.T) {
	tests := []struct {
		name     string
		keysJSON string
		combo    string
		want     string
		wantErr  bool
	}{
		{"keys array", `["ctrl","c"]`, "Ctrl+C", "ctrl+c", false},
		{"keys array single", `["Return"]`, "Enter", "Return", false},
		{"keys array trims blanks", `["ctrl","","alt","del"]`, "", "ctrl+alt+del", false},
		{"falls back to combo", "", "ctrl+v", "ctrl+v", false},
		{"empty keys uses combo", `[]`, "Escape", "Escape", false},
		{"invalid keys json", `not json`, "", "", true},
		{"both empty", "", "", "", true},
		{"keys whitespace only falls back", `["  "]`, "Tab", "Tab", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildKeyCombo(tt.keysJSON, tt.combo)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("buildKeyCombo() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildKeyActions(t *testing.T) {
	actions, err := buildKeyActions("ctrl+c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	if actions[0].Type != "key" || actions[0].Key != "ctrl+c" {
		t.Errorf("got %+v, want key=ctrl+c", actions[0])
	}

	// An unsafe combo must be rejected by the desktop action validator.
	if _, err := buildKeyActions("a;rm -rf /"); err == nil {
		t.Error("expected validation error for injection combo, got nil")
	}
}

func TestBuildActions(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantLen int
		wantErr bool
	}{
		{"single click", `[{"type":"click","x":100,"y":200,"button":1}]`, 1, false},
		{
			"drag sequence",
			`[{"type":"move","x":10,"y":20},{"type":"mousedown","button":1},{"type":"move","x":90,"y":80},{"type":"mouseup","button":1}]`,
			4,
			false,
		},
		{"empty payload", "", 0, true},
		{"whitespace payload", "   ", 0, true},
		{"empty array", `[]`, 0, true},
		{"invalid json", `not json`, 0, true},
		{"unknown action rejected", `[{"type":"exec","text":"rm -rf /"}]`, 0, true},
		{"click out of range rejected", `[{"type":"click","x":-1,"y":100}]`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions, err := buildActions(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got %d actions)", len(actions))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(actions) != tt.wantLen {
				t.Errorf("got %d actions, want %d", len(actions), tt.wantLen)
			}
		})
	}
}

func TestBuildTypeActions(t *testing.T) {
	actions, err := buildTypeActions("hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	if actions[0].Type != "type" || actions[0].Text != "hello world" {
		t.Errorf("got %+v, want type=hello world", actions[0])
	}

	if _, err := buildTypeActions(""); err == nil {
		t.Error("expected error for empty text, got nil")
	}
}
