package harnessdriver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLinesPreserveBufferedAndLargeFrames(t *testing.T) {
	large := strings.Repeat("x", 5<<20)
	wire := "first\nsecond\n" + large + "\nlast\n"
	r, err := NewLineReader(strings.NewReader(wire), MaxLineBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second", large, "last"} {
		got, err := r.Next()
		if err != nil || string(got) != want {
			t.Fatalf("frame mismatch: len=%d err=%v", len(got), err)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
}

func TestLinesBoundEveryFrameAndRequireTerminator(t *testing.T) {
	for _, tc := range []struct {
		wire  string
		limit int
		want  error
	}{
		{"1234\n12345\n", 4, ErrLineTooLarge},
		{strings.Repeat("x", 100000), 4, ErrLineTooLarge},
		{"1234", 4, ErrTruncatedLine},
		{"123\r\n", 4, nil},
	} {
		r, err := NewLineReader(strings.NewReader(tc.wire), tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Next()
		if strings.HasPrefix(tc.wire, "1234\n") {
			_, err = r.Next()
		}
		if !errors.Is(err, tc.want) {
			t.Fatalf("limit=%d: got %v want %v", tc.limit, err, tc.want)
		}
	}
	if _, err := NewLineReader(strings.NewReader(""), MaxLineBytes+1); !errors.Is(err, ErrInvalidLimit) {
		t.Fatal(err)
	}
}

type shortWriter struct {
	bytes.Buffer
	fail bool
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.fail {
		return 0, errors.New("private diagnostic")
	}
	if len(p) > 3 {
		p = p[:3]
	}
	return w.Buffer.Write(p)
}

func TestWriterSerializesWholeFramesAndPoisonsFailures(t *testing.T) {
	var target shortWriter
	w := NewFrameWriter(&target)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.WriteJSON(map[string]string{"value": "synthetic"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := strings.Count(target.String(), "{\"value\":\"synthetic\"}\n"); got != 30 {
		t.Fatalf("interleaved frames: %d", got)
	}
	target.fail = true
	if err := w.WriteJSON(true); !errors.Is(err, ErrWrite) || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
	target.fail = false
	if err := w.WriteJSON(true); !errors.Is(err, ErrWrite) {
		t.Fatalf("poisoned writer reused: %v", err)
	}
}

func TestRunStreamsDrainsStderrAndCancelsOwnedReaders(t *testing.T) {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	seen := make(chan string, 2)
	go func() {
		done <- RunStreams(ctx, outR, errR, func(_ context.Context, line []byte) error { seen <- string(line); return nil })
	}()
	produced := make(chan error, 1)
	go func() {
		defer outW.Close()
		defer errW.Close()
		if _, err := io.Copy(errW, strings.NewReader(strings.Repeat("discard", 1<<17))); err != nil {
			produced <- err
			return
		}
		_, err := io.WriteString(outW, "one\ntwo\n")
		produced <- err
	}()
	select {
	case err := <-produced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stderr stalled stdout")
	}
	for _, want := range []string{"one", "two"} {
		select {
		case got := <-seen:
			if got != want {
				t.Fatal(got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("lost buffered line")
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readers did not finish")
	}

	outR, outW = io.Pipe()
	errR, errW = io.Pipe()
	defer outW.Close()
	defer errW.Close()
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- RunStreams(ctx, outR, errR, func(context.Context, []byte) error { return nil }) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel leaked readers")
	}
}
