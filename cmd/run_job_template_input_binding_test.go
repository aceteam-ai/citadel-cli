package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func boundTemplateInput(name string, body []byte) map[string]any {
	return map[string]any{
		"path": "node:n1/" + name, "node_id": "n1", "node_path": name,
		"expected_sha256": fmt.Sprintf("sha256:%x", sha256.Sum256(body)), "expected_bytes": len(body),
	}
}

func boundTemplateInputJSON(t *testing.T, refs []map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLiveTemplateRunOps_FilmInputBindingRefusesBeforeBuiltin(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func([]map[string]any) []map[string]any
		mutateFile string
	}{
		{name: "film changed after approval", mutateFile: "film.json", want: "expected_sha256"},
		{name: "chapter changed after approval", mutateFile: "chapter.mp4", want: "expected_sha256"},
		{name: "poster changed after approval", mutateFile: "poster.png", want: "expected_sha256"},
		{name: "size mismatch", want: "expected_bytes", edit: func(refs []map[string]any) []map[string]any { refs[1]["expected_bytes"] = 2; return refs }},
		{name: "malformed digest", want: "expected_sha256", edit: func(refs []map[string]any) []map[string]any {
			refs[1]["expected_sha256"] = strings.Repeat("a", 64)
			return refs
		}},
		{name: "wrong path identity", want: "disagrees", edit: func(refs []map[string]any) []map[string]any { refs[1]["path"] = "node:n1/other.mp4"; return refs }},
		{name: "substituted source path", want: "expected_sha256", edit: func(refs []map[string]any) []map[string]any {
			refs[1]["path"] = "node:n1/other.mp4"
			refs[1]["node_path"] = "other.mp4"
			return refs
		}},
		{name: "missing digest", want: "requires", edit: func(refs []map[string]any) []map[string]any { delete(refs[1], "expected_sha256"); return refs }},
		{name: "missing size", want: "requires", edit: func(refs []map[string]any) []map[string]any { delete(refs[1], "expected_bytes"); return refs }},
		{name: "duplicate source with conflicting digest", want: "expected_sha256", edit: func(refs []map[string]any) []map[string]any {
			return append(refs, boundTemplateInput("chapter.mp4", []byte("y")))
		}},
		{name: "duplicate source with conflicting size", want: "expected_bytes", edit: func(refs []map[string]any) []map[string]any {
			duplicate := boundTemplateInput("chapter.mp4", []byte("x"))
			duplicate["expected_bytes"] = 2
			return append(refs, duplicate)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			refs := []map[string]any{}
			for _, name := range []string{"film.json", "chapter.mp4", "poster.png"} {
				if err := os.WriteFile(filepath.Join(ws, name), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				refs = append(refs, boundTemplateInput(name, []byte("x")))
			}
			if err := os.WriteFile(filepath.Join(ws, "other.mp4"), []byte("z"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.edit != nil {
				refs = tc.edit(refs)
			}
			req := verifiedTemplateRequest(t, worker.TemplateRunRequest{
				JobID: "bound-refusal", TemplateKey: "video-compose",
				Runner: json.RawMessage(`{"kind":"builtin","handler":"test-bound-film"}`), InputFiles: boundTemplateInputJSON(t, refs),
			})
			if tc.mutateFile != "" {
				// The approved ref now points to a same-size, one-byte mutation.
				if err := os.WriteFile(filepath.Join(ws, tc.mutateFile), []byte("y"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			registerTestBuiltin(t, "test-bound-film", func(context.Context, json.RawMessage, []string, string) ([]string, error) {
				called = true
				return nil, errors.New("render side effect must not run")
			})
			res, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) || called || res != nil {
				t.Fatalf("input refusal: result=%+v err=%v builtin called=%v", res, err, called)
			}
			attempts, err := filepath.Glob(filepath.Join(ws, "templates", "video-compose", "bound-refusal", "*"))
			if err != nil || len(attempts) != 0 {
				t.Fatalf("refused attempt left staging/output namespace: %v %v", attempts, err)
			}
		})
	}
}

func TestLiveTemplateRunOps_BoundSnapshotSurvivesSourceMutationAndRetryRechecks(t *testing.T) {
	ws := t.TempDir()
	source := filepath.Join(ws, "chapter.mp4")
	approved := []byte("original")
	if err := os.WriteFile(source, approved, 0o600); err != nil {
		t.Fatal(err)
	}
	invocations := 0
	var firstInput string
	registerTestBuiltin(t, "test-bound-retry", func(_ context.Context, _ json.RawMessage, inputs []string, outDir string) ([]string, error) {
		invocations++
		if invocations == 1 {
			firstInput = inputs[0]
			if err := os.WriteFile(source, []byte("mutated!"), 0o600); err != nil {
				return nil, err
			}
		}
		consumed, err := os.ReadFile(inputs[0])
		if err != nil || string(consumed) != string(approved) {
			return nil, fmt.Errorf("builtin consumed unapproved source bytes %q: %v", consumed, err)
		}
		if invocations == 1 {
			return nil, errors.New("synthetic retry")
		}
		if inputs[0] == firstInput {
			return nil, errors.New("retry reused previous snapshot namespace")
		}
		if err := os.WriteFile(filepath.Join(outDir, "film.mp4"), consumed, 0o600); err != nil {
			return nil, err
		}
		return []string{"film.mp4"}, nil
	})
	req := verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID: "bound-retry", TemplateKey: "video-compose",
		Runner:     json.RawMessage(`{"kind":"builtin","handler":"test-bound-retry"}`),
		InputFiles: boundTemplateInputJSON(t, []map[string]any{boundTemplateInput("chapter.mp4", approved)}),
	})
	ops := liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}
	if _, err := ops.Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "synthetic retry") || invocations != 1 {
		t.Fatalf("first attempt: %v invocations=%d", err, invocations)
	}
	if _, err := ops.Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "expected_sha256") || invocations != 1 {
		t.Fatalf("retry accepted changed workspace bytes: %v invocations=%d", err, invocations)
	}
	if err := os.WriteFile(source, approved, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ops.Run(context.Background(), req)
	wantHash := fmt.Sprintf("sha256:%x", sha256.Sum256(approved))
	if err != nil || invocations != 2 || res == nil || len(res.InputHashes) != 1 || res.InputHashes[0] != wantHash {
		t.Fatalf("retry after restoring approved bytes: %+v %v invocations=%d", res, err, invocations)
	}
}

func TestLiveTemplateRunOps_GenericOptionalExpectationsAreEnforced(t *testing.T) {
	for _, field := range []string{"expected_sha256", "expected_bytes"} {
		t.Run(field, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "source.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			ref := boundTemplateInput("source.txt", []byte("xx"))
			if field == "expected_sha256" {
				delete(ref, "expected_bytes")
			} else {
				delete(ref, "expected_sha256")
			}
			registerTestBuiltin(t, "test-bound-generic", func(context.Context, json.RawMessage, []string, string) ([]string, error) {
				t.Fatal("generic builtin invoked with mismatched expectation")
				return nil, nil
			})
			req := verifiedTemplateRequest(t, worker.TemplateRunRequest{
				JobID: "generic-binding", TemplateKey: "generic-template",
				Runner: json.RawMessage(`{"kind":"builtin","handler":"test-bound-generic"}`), InputFiles: boundTemplateInputJSON(t, []map[string]any{ref}),
			})
			if _, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("generic optional expectation ignored: %v", err)
			}
		})
	}
}
