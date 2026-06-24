package deskstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInitMessage_WireContract pins the exact init JSON the web (#4254) and iOS
// (#4255) clients depend on. Changing any key or value here is a wire-contract
// break.
func TestInitMessage_WireContract(t *testing.T) {
	m := NewInitMessage(1920, 1080, 30, 60)
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)

	for _, want := range []string{
		`"type":"init"`,
		`"codec":"h264"`,
		`"width":1920`,
		`"height":1080`,
		`"fps":30`,
		`"keyframeInterval":60`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("init JSON missing %q\ngot: %s", want, s)
		}
	}

	// Round-trip to ensure the field names are exactly as documented.
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"type", "codec", "width", "height", "fps", "keyframeInterval"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("init JSON missing key %q", key)
		}
	}
}

func TestParseClientMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"requestKeyframe", `{"type":"requestKeyframe"}`, ClientMsgRequestKeyframe},
		{"unknown-type", `{"type":"resize"}`, "resize"},
		{"no-type", `{"foo":"bar"}`, ""},
		{"invalid-json", `not json`, ""},
		{"empty", ``, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseClientMessage([]byte(c.in)); got != c.want {
				t.Errorf("parseClientMessage(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseXdpyinfoGeometry(t *testing.T) {
	out := "  ...\n  dimensions:    1920x1080 pixels (508x285 millimeters)\n  ..."
	g := parseXdpyinfoGeometry(out)
	if g.Width != 1920 || g.Height != 1080 {
		t.Errorf("got %dx%d, want 1920x1080", g.Width, g.Height)
	}
}

func TestParseXdpyinfoGeometry_FallsBack(t *testing.T) {
	g := parseXdpyinfoGeometry("no dimensions here")
	if g != defaultGeometry {
		t.Errorf("got %+v, want default %+v", g, defaultGeometry)
	}
}
