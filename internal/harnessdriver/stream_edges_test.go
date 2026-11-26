package harnessdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"
)

type chunkReader struct {
	source io.Reader
	chunk  int
}

func (r chunkReader) Read(p []byte) (int, error) { return r.source.Read(p[:min(len(p), r.chunk)]) }

type endlessReader struct{ read int }

func (r *endlessReader) Read(p []byte) (int, error) { clear(p); r.read += len(p); return len(p), nil }

type diagnosticReader struct{}

func (diagnosticReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), errors.New("private diagnostic")
}

func TestLineReadErrorDoesNotPublishPartialData(t *testing.T) {
	r, _ := NewLineReader(diagnosticReader{}, 100)
	got, err := r.Next()
	if got != nil || !errors.Is(err, ErrRead) || strings.Contains(err.Error(), "private") {
		t.Fatalf("partial read returned: %q %v", got, err)
	}
}

func TestLineBudgetAcrossChunkAndFrameBoundaries(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	for _, limit := range []int{1, 2, 7, 16, 64, 1000} {
		var wire bytes.Buffer
		var wants []string
		for i := 0; i < 100; i++ {
			s := strings.Repeat("x", random.Intn(limit+1))
			wants = append(wants, s)
			wire.WriteString(s + "\n")
		}
		for _, chunk := range []int{1, 3, 16, 10000} {
			r, err := NewLineReader(chunkReader{strings.NewReader(wire.String()), chunk}, limit)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range wants {
				got, err := r.Next()
				if err != nil || string(got) != want {
					t.Fatalf("limit=%d chunk=%d frame=%d len=%d err=%v", limit, chunk, i, len(got), err)
				}
			}
			if _, err := r.Next(); !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
		}
	}
	for _, limit := range []int{4, 64, 1 << 20} {
		source := new(endlessReader)
		r, _ := NewLineReader(source, limit)
		if _, err := r.Next(); !errors.Is(err, ErrLineTooLarge) {
			t.Fatal(err)
		}
		if source.read > limit+2 {
			t.Fatalf("unbounded read: %d", source.read)
		}
		before := source.read
		if _, err := r.Next(); !errors.Is(err, ErrLineTooLarge) || source.read != before {
			t.Fatal("terminal reader resumed")
		}
	}
}

type failingWriter struct {
	writes int
	n      int
}

func (w *failingWriter) Write(p []byte) (int, error) { w.writes++; return min(w.n, len(p)), nil }

func TestWriterZeroProgressAndInvalidFrames(t *testing.T) {
	target := new(failingWriter)
	w := NewFrameWriter(target)
	if err := w.WriteJSON(true); !errors.Is(err, ErrWrite) {
		t.Fatal(err)
	}
	target.n = 10
	if err := w.WriteJSON(true); !errors.Is(err, ErrWrite) || target.writes != 1 {
		t.Fatal("zero-progress writer resumed")
	}
	var out bytes.Buffer
	w = NewFrameWriter(&out)
	for _, value := range []any{json.RawMessage("not JSON"), strings.Repeat("a", MaxLineBytes+1), make(chan int)} {
		if err := w.WriteJSON(value); !errors.Is(err, ErrFrame) || out.Len() != 0 {
			t.Fatalf("invalid frame wrote: %v", err)
		}
	}
	if err := w.WriteJSON(map[string]string{"text": "one\ntwo"}); err != nil || strings.Count(out.String(), "\n") != 1 {
		t.Fatal("text newline broke framing")
	}
}

func TestRunStreamsCleanStdoutClosesBlockedStderr(t *testing.T) {
	errR, errW := io.Pipe()
	defer errW.Close()
	done := make(chan error, 1)
	go func() {
		done <- RunStreams(context.Background(), io.NopCloser(strings.NewReader("")), errR, func(context.Context, []byte) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EOF did not stop stderr")
	}
}

func TestRunStreamsHandlerErrorIsFiniteAndClosesReaders(t *testing.T) {
	errR, errW := io.Pipe()
	defer errW.Close()
	err := RunStreams(context.Background(), io.NopCloser(strings.NewReader("one\ntwo\n")), errR, func(context.Context, []byte) error { return errors.New("private callback data") })
	if !errors.Is(err, ErrHandler) || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
}

func TestQueueCloseWakesBlockedWaiters(t *testing.T) {
	for i := 0; i < 20; i++ {
		q, _ := NewEventQueue(1)
		_ = q.Push(context.Background(), sampleEvent(1))
		done := make(chan error, 1)
		go func(q *EventQueue) { done <- q.Push(context.Background(), sampleEvent(2)) }(q)
		q.Close()
		select {
		case err := <-done:
			if !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("close did not release producer")
		}
		q, _ = NewEventQueue(1)
		go func(q *EventQueue) { _, err := q.Pop(context.Background()); done <- err }(q)
		q.Close()
		select {
		case err := <-done:
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("close did not release consumer")
		}
	}
}
