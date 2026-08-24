package cobrowsestream

import (
	"encoding/json"
	"testing"
)

func TestInitMessageMarshal(t *testing.T) {
	b, err := NewInitMessage("cb-abc").Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "init" {
		t.Errorf("type = %v, want init", m["type"])
	}
	if m["codec"] != "mjpeg" {
		t.Errorf("codec = %v, want mjpeg", m["codec"])
	}
	if m["sessionId"] != "cb-abc" {
		t.Errorf("sessionId = %v, want cb-abc", m["sessionId"])
	}
}

func TestParseInputMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
		typ  string
	}{
		{"mouse", `{"type":"mouse","event":"mousePressed","x":0.5,"y":0.25,"button":"left"}`, true, InputTypeMouse},
		{"key", `{"type":"key","keyEvent":"keyDown","key":"a","text":"a"}`, true, InputTypeKey},
		{"unknown type", `{"type":"scroll"}`, false, ""},
		{"no type", `{"x":0.5}`, false, ""},
		{"garbage", `not json`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := parseInputMessage([]byte(tc.in))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && m.Type != tc.typ {
				t.Errorf("type = %q, want %q", m.Type, tc.typ)
			}
		})
	}
}

func TestClamp01(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{-0.5, 0}, {0, 0}, {0.5, 0.5}, {1, 1}, {1.5, 1},
	}
	for _, tc := range cases {
		if got := clamp01(tc.in); got != tc.want {
			t.Errorf("clamp01(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMouseButtonDefaultsToNone(t *testing.T) {
	if got := mouseButton(""); got != "none" {
		t.Errorf("empty button = %q, want none", got)
	}
	if got := mouseButton("bogus"); got != "none" {
		t.Errorf("unknown button = %q, want none", got)
	}
	if got := mouseButton("left"); got != "left" {
		t.Errorf("left button = %q, want left", got)
	}
}

func TestFrameHolderCoalesces(t *testing.T) {
	h := newFrameHolder()
	// Many sets while nobody is taking: only the latest survives, signal is
	// one-deep -> memory bounded to one frame no matter how far behind a viewer is.
	for i := 0; i < 100; i++ {
		h.set([]byte{byte(i)})
	}
	// Exactly one signal is pending (buffered channel of size 1).
	select {
	case <-h.signal:
	default:
		t.Fatal("expected a pending signal after sets")
	}
	got := h.take()
	if len(got) != 1 || got[0] != 99 {
		t.Fatalf("take = %v, want latest [99]", got)
	}
	// After take, the slot is empty.
	if got := h.take(); got != nil {
		t.Errorf("second take = %v, want nil", got)
	}
	// No extra signals buffered.
	select {
	case <-h.signal:
		t.Fatal("unexpected extra signal")
	default:
	}
}
