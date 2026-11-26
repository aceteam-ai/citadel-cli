package harnessdriver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

const MaxLineBytes = 8 << 20

var (
	ErrInvalidLimit  = errors.New("harness_driver_invalid_limit")
	ErrLineTooLarge  = errors.New("harness_driver_line_too_large")
	ErrTruncatedLine = errors.New("harness_driver_unterminated_line")
	ErrRead          = errors.New("harness_driver_read_failed")
	ErrWrite         = errors.New("harness_driver_write_failed")
	ErrFrame         = errors.New("harness_driver_invalid_frame")
	ErrHandler       = errors.New("harness_driver_handler_failed")
)

// LineReader owns one buffered stream, not one reader per readiness notification.
// A newline is required, including on the final frame. A terminal error poisons
// the reader: no partial or over-limit frame can be interpreted as a new frame.
type LineReader struct {
	limited  *io.LimitedReader
	reader   *bufio.Reader
	max      int
	terminal error
}

func NewLineReader(source io.Reader, max int) (*LineReader, error) {
	if source == nil || max < 1 || max > MaxLineBytes {
		return nil, ErrInvalidLimit
	}
	limited := &io.LimitedReader{R: source}
	return &LineReader{limited: limited, reader: bufio.NewReaderSize(limited, min(64<<10, max+2)), max: max}, nil
}

func (r *LineReader) Next() ([]byte, error) {
	if r.terminal != nil {
		return nil, r.terminal
	}
	// Include bytes already buffered for this frame in the budget. Allow one
	// delimiter and one extra byte to distinguish an over-limit frame from EOF.
	r.limited.N = int64(r.max + 2 - r.reader.Buffered())
	line, err := r.reader.ReadBytes('\n')
	terminated := len(line) > 0 && line[len(line)-1] == '\n'
	if terminated {
		line = line[:len(line)-1]
	}
	if len(line) > r.max {
		r.terminal = ErrLineTooLarge
	} else if err != nil {
		switch {
		case errors.Is(err, io.EOF) && len(line) == 0:
			r.terminal = io.EOF
		case errors.Is(err, io.EOF):
			r.terminal = ErrTruncatedLine
		default:
			r.terminal = ErrRead
		}
	} else if !terminated {
		r.terminal = ErrTruncatedLine
	}
	if r.terminal != nil {
		return nil, r.terminal
	}
	return bytes.TrimSuffix(line, []byte{'\r'}), nil
}

// FrameWriter serializes complete JSON lines, handles short writes, and refuses
// reuse after a failed write, whose delivery may be partial or unknown.
type FrameWriter struct {
	mu     sync.Mutex
	writer io.Writer
	failed bool
}

func NewFrameWriter(writer io.Writer) *FrameWriter { return &FrameWriter{writer: writer} }

func (w *FrameWriter) WriteJSON(value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > MaxLineBytes || bytes.ContainsAny(raw, "\r\n") {
		return ErrFrame
	}
	raw = append(raw, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed || w.writer == nil {
		return ErrWrite
	}
	for len(raw) > 0 {
		n, err := w.writer.Write(raw)
		if n < 0 || n > len(raw) {
			w.failed = true
			return ErrWrite
		}
		raw = raw[n:]
		if err != nil || n == 0 {
			w.failed = true
			return ErrWrite
		}
	}
	return nil
}

// RunStreams takes ownership of supplied readers. Close must interrupt their
// reads. stdout and stderr are drained by separate goroutines; stderr contents
// are discarded, never interpreted or retained. A handler must honor ctx and
// must not retain the supplied frame without copying it. This does not start a
// child process or prove anything about the source of either stream.
func RunStreams(ctx context.Context, stdout, stderr io.ReadCloser, handler func(context.Context, []byte) error) error {
	if stdout == nil || stderr == nil || handler == nil {
		return ErrRead
	}
	var once sync.Once
	closeReaders := func() { once.Do(func() { _ = stdout.Close(); _ = stderr.Close() }) }
	defer closeReaders()
	stop := context.AfterFunc(ctx, closeReaders)
	defer stop()
	type result struct {
		stdout bool
		err    error
	}
	done := make(chan result, 2)
	go func() {
		r, _ := NewLineReader(stdout, MaxLineBytes)
		for {
			if err := ctx.Err(); err != nil {
				done <- result{true, err}
				return
			}
			line, err := r.Next()
			if errors.Is(err, io.EOF) {
				done <- result{stdout: true}
				return
			}
			if err != nil {
				done <- result{true, err}
				return
			}
			if err = handler(ctx, line); err != nil {
				done <- result{true, ErrHandler}
				return
			}
		}
	}()
	go func() {
		_, err := io.Copy(io.Discard, stderr)
		if err != nil {
			err = ErrRead
		}
		done <- result{false, err}
	}()
	var outcome error
	shuttingDown := false
	for i := 0; i < 2; i++ {
		r := <-done
		if (r.stdout || r.err != nil) && !shuttingDown {
			outcome = r.err
			shuttingDown = true
			closeReaders()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return outcome
}
