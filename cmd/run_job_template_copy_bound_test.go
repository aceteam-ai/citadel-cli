package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

type templateCountingReader struct {
	reader   io.Reader
	consumed int
	requests []int
}

func (r *templateCountingReader) Read(p []byte) (int, error) {
	r.requests = append(r.requests, len(p))
	n, err := r.reader.Read(p)
	r.consumed += n
	return n, err
}

func TestCopyTemplateLimitBoundsReadAhead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       []byte
		limit      int64
		wantExcess bool
	}{
		{name: "empty exact", limit: 0},
		{name: "zero lookahead", body: []byte("ab"), limit: 0, wantExcess: true},
		{name: "exact", body: []byte("abc"), limit: 3},
		{name: "short", body: []byte("ab"), limit: 3},
		{name: "one extra", body: []byte("abcd"), limit: 3, wantExcess: true},
		{name: "large source tiny allowance", body: bytes.Repeat([]byte("x"), 256*1024), limit: 3, wantExcess: true},
		{name: "cross buffer", body: bytes.Repeat([]byte("x"), 256*1024), limit: 128*1024 + 3, wantExcess: true},
		{name: "max int64", body: []byte("abc"), limit: math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &templateCountingReader{reader: bytes.NewReader(tc.body)}
			var dst bytes.Buffer
			n, err := copyTemplateWithContextLimit(context.Background(), &dst, src, tc.limit)
			if errors.Is(err, errTemplateCopyLimit) != tc.wantExcess || (err != nil && !tc.wantExcess) {
				t.Fatalf("copy: n=%d err=%v wantExcess=%v", n, err, tc.wantExcess)
			}
			if n != int64(dst.Len()) || n > tc.limit || (tc.limit != math.MaxInt64 && int64(src.consumed) > tc.limit+1) {
				t.Fatalf("unbounded copy: n=%d written=%d consumed=%d cap=%d", n, dst.Len(), src.consumed, tc.limit)
			}
			if !tc.wantExcess && !bytes.Equal(dst.Bytes(), tc.body) {
				t.Fatal("successful copy changed content")
			}
			// Also pin the request, not just consumption by a cooperative reader.
			if tc.limit < 128*1024 && src.requests[0] != int(tc.limit)+1 {
				t.Fatalf("first read request=%d, want=%d", src.requests[0], tc.limit+1)
			}
			if tc.wantExcess && err.Error() != fmt.Sprintf("byte limit of %d exceeded", tc.limit) {
				t.Fatalf("legacy byte-limit diagnostic changed: %v", err)
			}
		})
	}
}

type templateCopyFailWriter struct{ err error }

func (w templateCopyFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCopyTemplateLimitPreservesIOAndCancellation(t *testing.T) {
	readErr := errors.New("reader failure")
	writeErr := errors.New("writer failure")
	if _, err := copyTemplateWithContextLimit(context.Background(), io.Discard, iotest.ErrReader(readErr), 3); !errors.Is(err, readErr) || errors.Is(err, errTemplateCopyLimit) {
		t.Fatalf("read error reclassified: %v", err)
	}
	if _, err := copyTemplateWithContextLimit(context.Background(), templateCopyFailWriter{err: writeErr}, strings.NewReader("abc"), 3); !errors.Is(err, writeErr) || errors.Is(err, errTemplateCopyLimit) {
		t.Fatalf("write error reclassified: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &templateCountingReader{reader: strings.NewReader("abcd")}
	if _, err := copyTemplateWithContextLimit(ctx, io.Discard, src, 3); !errors.Is(err, context.Canceled) || errors.Is(err, errTemplateCopyLimit) || len(src.requests) != 0 {
		t.Fatalf("cancellation read or reclassified: %v requests=%v", err, src.requests)
	}
}

func TestResolveTemplateInputsExpectedCopyBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantError string
		expected              *int64
		global                int64
		duplicates            int
	}{
		{name: "omitted", body: "abc", global: 3},
		{name: "zero exact", expected: videoComposeTestInt64(0), global: 3},
		{name: "zero excess", body: "a", expected: videoComposeTestInt64(0), global: 3, wantError: "expected_bytes"},
		{name: "exact", body: "abc", expected: videoComposeTestInt64(3), global: 3},
		{name: "short", body: "ab", expected: videoComposeTestInt64(3), global: 3, wantError: "expected_bytes"},
		{name: "long", body: "abcd", expected: videoComposeTestInt64(3), global: 4, wantError: "expected_bytes"},
		{name: "global tighter", body: "abcd", expected: videoComposeTestInt64(5), global: 3, wantError: "declared input bytes"},
		{name: "max int64 expectation", body: "abc", expected: videoComposeTestInt64(math.MaxInt64), global: 3, wantError: "expected_bytes"},
		{name: "duplicate exact", body: "abc", expected: videoComposeTestInt64(3), global: 6, duplicates: 1},
		{name: "duplicate aggregate", body: "abc", expected: videoComposeTestInt64(3), global: 5, duplicates: 1, wantError: "declared input bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "source"), []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(ws)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.Mkdir("staged", 0700); err != nil {
				t.Fatal(err)
			}
			staged, err := root.OpenRoot("staged")
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Close()
			file := jobs.TemplateInputFile{Path: "source", NodeID: "n1", NodePath: "source", ExpectedBytes: tc.expected}
			files := []jobs.TemplateInputFile{file}
			for i := 0; i < tc.duplicates; i++ {
				files = append(files, file)
			}
			_, _, hashes, err := (liveTemplateRunOps{nodeID: "n1"}).resolveInputs(context.Background(), files, root, staged, filepath.Join(ws, "staged"), tc.global)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("expected %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil || len(hashes) != len(files) {
				t.Fatalf("staging: hashes=%v err=%v", hashes, err)
			}
			for _, hash := range hashes {
				if hash != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(tc.body))) {
					t.Fatalf("snapshot digest changed: %s", hash)
				}
			}
		})
	}
}

func videoComposeTestInt64(n int64) *int64 { return &n }

func TestTemplateExpectedCopyOverflowCleansBeforeBuiltin(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "source"), []byte("more than expected"), 0600); err != nil {
		t.Fatal(err)
	}
	registerTestBuiltin(t, "test-expected-copy-cap", func(context.Context, json.RawMessage, []string, string) ([]string, error) {
		t.Fatal("copy overrun invoked builtin")
		return nil, nil
	})
	req := verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID: "copy-cap", TemplateKey: "generic-copy-cap", Runner: json.RawMessage(`{"kind":"builtin","handler":"test-expected-copy-cap"}`),
		InputFiles: json.RawMessage(`[{"path":"source","node_id":"n1","node_path":"source","expected_bytes":1}]`),
	})
	res, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), req)
	if res != nil || !errors.Is(err, errTemplateCopyLimit) || !strings.Contains(err.Error(), "expected_bytes") {
		t.Fatalf("overrun refusal: result=%+v err=%v", res, err)
	}
	entries, err := os.ReadDir(filepath.Join(ws, "templates", "generic-copy-cap", "copy-cap"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("overrun retained attempt: %v %v", entries, err)
	}
}
