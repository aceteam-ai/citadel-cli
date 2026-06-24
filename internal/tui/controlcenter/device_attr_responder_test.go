package controlcenter

import (
	"bytes"
	"testing"
)

func TestDeviceAttrResponder_PrimaryDA(t *testing.T) {
	cases := map[string][]byte{
		"bare CSI c": []byte("\x1b[c"),
		"CSI 0 c":    []byte("\x1b[0c"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var r deviceAttrResponder
			got := r.scan(in)
			if !bytes.Equal(got, primaryDAResponse) {
				t.Fatalf("got %q, want primary DA %q", got, primaryDAResponse)
			}
		})
	}
}

func TestDeviceAttrResponder_SecondaryDA(t *testing.T) {
	var r deviceAttrResponder
	got := r.scan([]byte("\x1b[>c"))
	if !bytes.Equal(got, secondaryDAResponse) {
		t.Fatalf("got %q, want secondary DA %q", got, secondaryDAResponse)
	}
}

func TestDeviceAttrResponder_TertiaryDAIgnored(t *testing.T) {
	var r deviceAttrResponder
	if got := r.scan([]byte("\x1b[=c")); got != nil {
		t.Fatalf("tertiary DA should yield no reply, got %q", got)
	}
}

func TestDeviceAttrResponder_EmbeddedInOutput(t *testing.T) {
	// A realistic startup burst: prompt text, the DA query, more text.
	var r deviceAttrResponder
	got := r.scan([]byte("jason@host ~> \x1b[c\x1b[0m hi"))
	if !bytes.Equal(got, primaryDAResponse) {
		t.Fatalf("got %q, want primary DA reply", got)
	}
}

func TestDeviceAttrResponder_SplitAcrossChunks(t *testing.T) {
	// The query is split at every interior byte boundary; the reply must
	// still fire exactly once, on the chunk carrying the final 'c'.
	full := []byte("\x1b[0c")
	for split := 1; split < len(full); split++ {
		t.Run("", func(t *testing.T) {
			var r deviceAttrResponder
			first := r.scan(full[:split])
			if len(first) != 0 {
				t.Fatalf("split=%d: unexpected early reply %q", split, first)
			}
			second := r.scan(full[split:])
			if !bytes.Equal(second, primaryDAResponse) {
				t.Fatalf("split=%d: got %q, want primary DA reply", split, second)
			}
		})
	}
}

func TestDeviceAttrResponder_NonDASequencesIgnored(t *testing.T) {
	// Color (SGR 'm'), cursor move ('H'), erase ('K'), and plain text must
	// never produce a reply — only 'c'-terminated CSI is a DA query.
	var r deviceAttrResponder
	noise := []byte("\x1b[31mred\x1b[0m\x1b[2J\x1b[1;1H\x1b[Kplain text 0c c [c")
	if got := r.scan(noise); got != nil {
		t.Fatalf("non-DA sequences/text should yield no reply, got %q", got)
	}
}

func TestDeviceAttrResponder_MultipleQueries(t *testing.T) {
	var r deviceAttrResponder
	got := r.scan([]byte("\x1b[c\x1b[>c"))
	want := append(append([]byte{}, primaryDAResponse...), secondaryDAResponse...)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDeviceAttrResponder_RunawayParamsResync(t *testing.T) {
	// A never-terminated CSI must not grow params unbounded, and a later
	// real DA query must still be answered after the responder resyncs.
	var r deviceAttrResponder
	long := append([]byte("\x1b["), bytes.Repeat([]byte("1;"), 40)...)
	if got := r.scan(long); got != nil {
		t.Fatalf("unterminated CSI should yield no reply, got %q", got)
	}
	if got := r.scan([]byte("\x1b[c")); !bytes.Equal(got, primaryDAResponse) {
		t.Fatalf("responder should resync and answer DA, got %q", got)
	}
}
