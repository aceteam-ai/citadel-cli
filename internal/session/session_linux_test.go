//go:build linux

package session

import "testing"

func TestParseDisplay(t *testing.T) {
	tests := []struct {
		display  string
		wantHost string
		wantDpy  string
	}{
		{":0", "", "0"},
		{":1", "", "1"},
		{":0.0", "", "0"},
		{":10.1", "", "10"},
		{"localhost:0", "localhost", "0"},
		{"192.168.1.5:1.0", "192.168.1.5", "1"},
		{"no-colon", "", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.display, func(t *testing.T) {
			host, dpy := parseDisplay(tt.display)
			if host != tt.wantHost || dpy != tt.wantDpy {
				t.Errorf("parseDisplay(%q) = (%q, %q), want (%q, %q)",
					tt.display, host, dpy, tt.wantHost, tt.wantDpy)
			}
		})
	}
}

func TestItoaAtoiSafe(t *testing.T) {
	cases := []struct {
		n int
		s string
	}{{0, "0"}, {1, "1"}, {6000, "6000"}, {6001, "6001"}}
	for _, c := range cases {
		if got := itoa(c.n); got != c.s {
			t.Errorf("itoa(%d) = %q, want %q", c.n, got, c.s)
		}
		if got := atoiSafe(c.s); got != c.n {
			t.Errorf("atoiSafe(%q) = %d, want %d", c.s, got, c.n)
		}
	}
	if got := atoiSafe("abc"); got != 0 {
		t.Errorf("atoiSafe non-numeric should be 0, got %d", got)
	}
}
