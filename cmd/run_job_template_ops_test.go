package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// registerTestBuiltin registers a builtin for the duration of a test and removes
// it afterward, so the global registry stays empty for other tests.
func registerTestBuiltin(t *testing.T, name string, fn BuiltinTemplateRunner) {
	t.Helper()
	RegisterBuiltinTemplateRunner(name, fn)
	t.Cleanup(func() {
		builtinTemplateRunnersMu.Lock()
		delete(builtinTemplateRunners, name)
		builtinTemplateRunnersMu.Unlock()
	})
}

func TestLiveTemplateRunOps_RunCollectsWorkspaceOutputsAndInputHashes(t *testing.T) {
	ws := t.TempDir()

	// A node-backed input file already present in the workspace.
	if err := os.WriteFile(filepath.Join(ws, "resume.txt"), []byte("candidate resume"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotInputs []string
	registerTestBuiltin(t, "test-echo", func(_ context.Context, _ json.RawMessage, inputs []string, outDir string) ([]string, error) {
		gotInputs = inputs
		if err := os.WriteFile(filepath.Join(outDir, "final.txt"), []byte("hello"), 0o644); err != nil {
			return nil, err
		}
		return []string{"final.txt"}, nil
	})

	ops := liveTemplateRunOps{workspaceDir: ws}
	res, err := ops.Run(context.Background(), worker.TemplateRunRequest{
		JobID:       "job-1",
		TemplateKey: "papercraft-render",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-echo"}`),
		InputFiles:  json.RawMessage(`[{"path":"resume.txt","node_id":"n1","node_path":"resume.txt"}]`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gotInputs) != 1 || !strings.HasSuffix(gotInputs[0], "resume.txt") {
		t.Fatalf("builtin got inputs %v, want one resume.txt absolute path", gotInputs)
	}
	if len(res.InputHashes) != 1 || !strings.HasPrefix(res.InputHashes[0], "sha256:") {
		t.Errorf("input hashes = %v, want one sha256:-prefixed digest", res.InputHashes)
	}
	if len(res.Outputs) != 1 {
		t.Fatalf("want 1 output, got %d", len(res.Outputs))
	}
	out := res.Outputs[0]
	if out.Bytes != int64(len("hello")) {
		t.Errorf("output bytes = %d, want 5", out.Bytes)
	}
	if !strings.HasPrefix(out.SHA256, "sha256:") || len(out.SHA256) != len("sha256:")+64 {
		t.Errorf("output sha256 = %q, want sha256:<64 hex>", out.SHA256)
	}
	// The reported path is workspace-relative and under the per-run template dir.
	if filepath.IsAbs(out.Path) || !strings.HasPrefix(out.Path, "templates/papercraft-render/") {
		t.Errorf("output path = %q, want workspace-relative under templates/papercraft-render/", out.Path)
	}
	if res.DurationMs < 0 {
		t.Errorf("duration_ms = %d, want >= 0", res.DurationMs)
	}
}

func TestLiveTemplateRunOps_UnknownHandlerIsTerminal(t *testing.T) {
	ops := liveTemplateRunOps{workspaceDir: t.TempDir()}
	_, err := ops.Run(context.Background(), worker.TemplateRunRequest{
		JobID:       "job-2",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"not-registered"}`),
	})
	if !errors.Is(err, worker.ErrTemplateUnknownHandler) {
		t.Fatalf("want ErrTemplateUnknownHandler, got %v", err)
	}
}

// TestLiveTemplateRunOps_OutputEscapingWorkspaceRejected pins that a builtin
// cannot report an output that resolves outside the workspace boundary.
func TestLiveTemplateRunOps_OutputEscapingWorkspaceRejected(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	registerTestBuiltin(t, "test-escape", func(_ context.Context, _ json.RawMessage, _ []string, outDir string) ([]string, error) {
		// Return a path that climbs out of the workspace entirely.
		rel, err := filepath.Rel(outDir, outside)
		if err != nil {
			return nil, err
		}
		return []string{rel}, nil
	})
	ops := liveTemplateRunOps{workspaceDir: ws}
	_, err := ops.Run(context.Background(), worker.TemplateRunRequest{
		JobID:       "job-3",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-escape"}`),
	})
	if err == nil {
		t.Fatal("expected an error for an output escaping the workspace, got nil")
	}
}
