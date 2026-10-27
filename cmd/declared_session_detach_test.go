package cmd

import (
	"bytes"
	"testing"
)

func TestDeclaredDetachAcrossEveryInputBoundary(t *testing.T) {
	input := []byte{'a', 0x1d, 'x', 'b', 0x1d, 'd', 'c'}
	for split := 0; split <= len(input); split++ {
		decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
		if err != nil {
			t.Fatal(err)
		}
		var actual []byte
		detached := false
		for _, chunk := range [][]byte{input[:split], input[split:]} {
			var output []byte
			output, detached = decoder.Feed(chunk)
			actual = append(actual, output...)
			if detached {
				break
			}
		}
		if !detached || !bytes.Equal(actual, []byte{'a', 0x1d, 'x', 'b'}) {
			t.Fatalf("split %d: forwarded %q, detached=%v", split, actual, detached)
		}
		if len(decoder.Flush()) != 0 {
			t.Fatal("a completed detach retained input")
		}
	}
}

func TestDeclaredDetachPreservesIncompleteAndRepeatedPrefixes(t *testing.T) {
	decoder, err := newDeclaredDetachDecoder(defaultDeclaredDetach)
	if err != nil {
		t.Fatal(err)
	}
	first, detached := decoder.Feed([]byte{'x', 0x1d})
	if detached || string(first) != "x" {
		t.Fatal("incomplete chord lost ordinary input")
	}
	second, detached := decoder.Feed([]byte{0x1d, 'y', 0x1d})
	if detached || !bytes.Equal(second, []byte{0x1d, 0x1d, 'y'}) {
		t.Fatal("repeated prefixes were dropped or treated as detach")
	}
	if tail := decoder.Flush(); !bytes.Equal(tail, []byte{0x1d}) || len(decoder.Flush()) != 0 {
		t.Fatal("EOF did not flush exactly the buffered byte")
	}
}

func TestDeclaredDetachCustomChordAndStrictGrammar(t *testing.T) {
	for _, spec := range []string{"C-a z", "C-? !", "C-@ @", "C-\\ q"} {
		decoder, err := newDeclaredDetachDecoder(spec)
		if err != nil {
			t.Fatalf("valid chord %q: %v", spec, err)
		}
		if output, detached := decoder.Feed([]byte{decoder.control, decoder.literal, 'x'}); !detached || len(output) != 0 {
			t.Fatalf("custom chord %q did not consume only its own input", spec)
		}
	}
	for _, spec := range []string{"", "C-]", "C-]d", "C-]  d", "C-] ", "C-] dd", "C-1 d", "ctrl-],d", "C-] \n", "C-é d"} {
		if _, err := newDeclaredDetachDecoder(spec); err == nil {
			t.Fatalf("accepted malformed detach keys %q", spec)
		}
	}
}
