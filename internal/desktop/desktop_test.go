package desktop

import (
	"encoding/json"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name: "ubuntu",
			contents: `NAME="Ubuntu"
VERSION="22.04.3 LTS (Jammy Jellyfish)"
ID=ubuntu
PRETTY_NAME="Ubuntu 22.04.3 LTS"
VERSION_ID="22.04"`,
			want: "Ubuntu 22.04.3 LTS",
		},
		{
			name: "debian",
			contents: `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"`,
			want: "Debian GNU/Linux 12 (bookworm)",
		},
		{
			name:     "empty",
			contents: "",
			want:     "unknown",
		},
		{
			name:     "no pretty name",
			contents: "NAME=\"Ubuntu\"\nVERSION_ID=\"22.04\"",
			want:     "unknown",
		},
		{
			name:     "single-quoted",
			contents: "PRETTY_NAME='Arch Linux'",
			want:     "Arch Linux",
		},
		{
			name:     "unquoted",
			contents: "PRETTY_NAME=Fedora",
			want:     "Fedora",
		},
		{
			name:     "empty value",
			contents: "PRETTY_NAME=\"\"",
			want:     "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseOSRelease(tt.contents)
			if got != tt.want {
				t.Errorf("ParseOSRelease() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseXrandrOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "primary with resolution",
			output: `Screen 0: minimum 320 x 200, current 1920 x 1080, maximum 16384 x 16384
eDP-1 connected primary 1920x1080+0+0 (normal left inverted right x axis y axis) 344mm x 194mm
   1920x1080     60.01*+  60.01    59.97
   1280x1024     60.02`,
			want: "1920x1080",
		},
		{
			name: "active mode fallback",
			output: `Screen 0: minimum 320 x 200, current 2560 x 1440, maximum 16384 x 16384
DP-1 connected (normal left inverted right x axis y axis)
   2560x1440     59.95*+
   1920x1080     60.00`,
			want: "2560x1440",
		},
		{
			name:   "no displays",
			output: "Screen 0: minimum 320 x 200, current 1024 x 768, maximum 16384 x 16384",
			want:   "",
		},
		{
			name:   "empty",
			output: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseXrandrOutput(tt.output)
			if got != tt.want {
				t.Errorf("ParseXrandrOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

func TestParseActions(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{"move", `[{"type":"move","x":100,"y":200}]`, 1, false},
		{"click", `[{"type":"click","x":100,"y":200,"button":1}]`, 1, false},
		{"type", `[{"type":"type","text":"hello world"}]`, 1, false},
		{"key", `[{"type":"key","key":"Return"}]`, 1, false},
		{"scroll", `[{"type":"scroll","delta":-3}]`, 1, false},
		{"multiple", `[{"type":"move","x":10,"y":20},{"type":"click","x":10,"y":20},{"type":"type","text":"hi"}]`, 3, false},
		{"empty array", `[]`, 0, true},
		{"invalid json", `not json`, 0, true},
		{"unknown type", `[{"type":"exec","text":"rm -rf /"}]`, 0, true},
		{"move missing x", `[{"type":"move","y":100}]`, 0, true},
		{"move missing y", `[{"type":"move","x":100}]`, 0, true},
		{"click out of range", `[{"type":"click","x":-1,"y":100}]`, 0, true},
		{"type empty text", `[{"type":"type","text":""}]`, 0, true},
		{"key empty", `[{"type":"key","key":""}]`, 0, true},
		{"key injection", `[{"type":"key","key":"a;rm -rf /"}]`, 0, true},
		{"scroll zero", `[{"type":"scroll","delta":0}]`, 0, true},
		{"scroll missing delta", `[{"type":"scroll"}]`, 0, true},
		{"invalid button", `[{"type":"click","x":100,"y":100,"button":6}]`, 0, true},
		{"scroll out of range", `[{"type":"scroll","delta":200}]`, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions, err := ParseActions([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if len(actions) != tt.wantLen {
				t.Errorf("got %d actions, want %d", len(actions), tt.wantLen)
			}
		})
	}
}

func TestActionToXdotoolArgs(t *testing.T) {
	tests := []struct {
		name     string
		action   Action
		wantCmd  string
		wantArgs []string
	}{
		{
			name:     "move",
			action:   Action{Type: "move", X: intPtr(100), Y: intPtr(200)},
			wantCmd:  "mousemove",
			wantArgs: []string{"--", "100", "200"},
		},
		{
			name:     "click with position",
			action:   Action{Type: "click", X: intPtr(50), Y: intPtr(75), Button: intPtr(1)},
			wantCmd:  "mousemove",
			wantArgs: []string{"--", "50", "75", "click", "1"},
		},
		{
			name:     "click default button",
			action:   Action{Type: "click", X: intPtr(50), Y: intPtr(75)},
			wantCmd:  "mousemove",
			wantArgs: []string{"--", "50", "75", "click", "1"},
		},
		{
			name:     "type text",
			action:   Action{Type: "type", Text: "hello"},
			wantCmd:  "type",
			wantArgs: []string{"--clearmodifiers", "--", "hello"},
		},
		{
			name:     "key press",
			action:   Action{Type: "key", Key: "ctrl+c"},
			wantCmd:  "key",
			wantArgs: []string{"--clearmodifiers", "ctrl+c"},
		},
		{
			name:     "scroll down",
			action:   Action{Type: "scroll", Delta: intPtr(-3)},
			wantCmd:  "click",
			wantArgs: []string{"5", "5", "5"},
		},
		{
			name:     "scroll up",
			action:   Action{Type: "scroll", Delta: intPtr(2)},
			wantCmd:  "click",
			wantArgs: []string{"4", "4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, args, err := ActionToXdotoolArgs(tt.action)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if cmd != tt.wantCmd {
				t.Errorf("cmd = %q, want %q", cmd, tt.wantCmd)
			}
			if len(args) != len(tt.wantArgs) {
				t.Errorf("args = %v, want %v", args, tt.wantArgs)
				return
			}
			for i, a := range args {
				if a != tt.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, a, tt.wantArgs[i])
				}
			}
		})
	}
}

func TestCapabilitiesJSON(t *testing.T) {
	caps := Capabilities{
		OS:               "linux",
		OSVersion:        "Ubuntu 22.04.3 LTS",
		Display:          ":0",
		ScreenResolution: "1920x1080",
		VNCPort:          5900,
	}

	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var decoded Capabilities
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if decoded != caps {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", decoded, caps)
	}
}

func TestVNCPortOmitEmpty(t *testing.T) {
	caps := Capabilities{OS: "linux", OSVersion: "Ubuntu 22.04.3 LTS"}
	data, _ := json.Marshal(caps)
	var decoded map[string]interface{}
	json.Unmarshal(data, &decoded)
	if _, ok := decoded["vnc_port"]; ok {
		t.Error("vnc_port should be omitted when zero")
	}
}

func TestTooManyActions(t *testing.T) {
	actions := make([]Action, 101)
	for i := range actions {
		actions[i] = Action{Type: "move", X: intPtr(0), Y: intPtr(0)}
	}
	data, _ := json.Marshal(actions)
	_, err := ParseActions(data)
	if err == nil {
		t.Error("expected error for > 100 actions")
	}
}

func TestTypeLongText(t *testing.T) {
	longText := make([]byte, 1001)
	for i := range longText {
		longText[i] = 'a'
	}
	input := `[{"type":"type","text":"` + string(longText) + `"}]`
	_, err := ParseActions([]byte(input))
	if err == nil {
		t.Error("expected error for text > 1000 chars")
	}
}
