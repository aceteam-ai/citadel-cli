package platform

import "testing"

func TestResolveX11Env_ExplicitEnvWins(t *testing.T) {
	t.Setenv("DISPLAY", ":7")
	t.Setenv("XAUTHORITY", "/tmp/some.Xauth")
	d, x := ResolveX11Env()
	if d != ":7" {
		t.Errorf("DISPLAY = %q, want :7 (explicit env must win)", d)
	}
	if x != "/tmp/some.Xauth" {
		t.Errorf("XAUTHORITY = %q, want /tmp/some.Xauth", x)
	}
}

func TestResolveX11Env_NeverReturnsEmptyDisplay(t *testing.T) {
	// With DISPLAY unset, the resolver must still yield a usable display
	// (detected, or the ":0" last resort) so callers never build a command
	// with an empty DISPLAY.
	t.Setenv("DISPLAY", "")
	t.Setenv("XAUTHORITY", "")
	d, _ := ResolveX11Env()
	if d == "" {
		t.Fatal("resolved DISPLAY is empty; expected a detected display or the :0 fallback")
	}
	if d[0] != ':' {
		t.Errorf("resolved DISPLAY = %q, want a :N form", d)
	}
}

func TestParseNonNegInt(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"0", 0, false},
		{"1", 1, false},
		{"42", 42, false},
		{"", 0, true},
		{"-1", 0, true},
		{"1a", 0, true},
	}
	for _, c := range cases {
		got, err := parseNonNegInt(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseNonNegInt(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Errorf("parseNonNegInt(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
