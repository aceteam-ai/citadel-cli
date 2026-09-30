package tmux

import (
	"errors"
	"testing"
)

func TestParseTmuxVersionAndMinimum(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		supported bool
	}{
		{name: "minimum", raw: "tmux 2.6", supported: true},
		{name: "patch suffix", raw: "tmux 2.6a", supported: true},
		{name: "current", raw: "tmux 3.4", supported: true},
		{name: "missing boolean operator", raw: "tmux 2.5", supported: false},
		{name: "old", raw: "tmux 1.9", supported: false},
		{name: "development label", raw: "tmux next-3.6", supported: false},
		{name: "garbage", raw: "not tmux", supported: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := supportedTmuxVersion(tc.raw); got != tc.supported {
				t.Fatalf("supportedTmuxVersion(%q) = %v, want %v", tc.raw, got, tc.supported)
			}
		})
	}
}

func TestParseTmuxVersionRejectsUnknownOutput(t *testing.T) {
	_, err := parseTmuxVersion("tmux next-3.6")
	if !errors.Is(err, ErrTmuxVersionUnsupported) {
		t.Fatalf("error = %v, want ErrTmuxVersionUnsupported", err)
	}
}
