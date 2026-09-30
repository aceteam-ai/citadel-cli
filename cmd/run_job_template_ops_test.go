package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func verifiedTemplateRequest(t *testing.T, req worker.TemplateRunRequest) worker.TemplateRunRequest {
	t.Helper()
	req.TemplateVersion = 1
	// Default to a closed empty input_schema (the #1161 gate requires
	// additionalProperties:false on every object subschema); a caller that needs
	// declared params supplies its own closed schema so the hash still covers it.
	if req.InputSchema == nil {
		req.InputSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
	}
	if req.OutputSchema == nil {
		req.OutputSchema = json.RawMessage(`{"type":"object"}`)
	}
	if req.Params == nil {
		req.Params = json.RawMessage(`{}`)
	}
	hash, err := jobs.ComputeTemplateManifestHash(req.TemplateKey, req.TemplateVersion, req.InputSchema, req.OutputSchema, req.Runner)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentHash = hash
	return req
}

// registerTestBuiltin registers a builtin for the duration of a test and removes
// only that test entry afterward, preserving the production registry.
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

	ops := liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}
	res, err := ops.Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-1",
		TemplateKey: "papercraft-render",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-echo"}`),
		InputFiles:  json.RawMessage(`[{"path":"resume.txt","node_id":"n1","node_path":"resume.txt"}]`),
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gotInputs) != 1 || !strings.HasSuffix(gotInputs[0], "resume.txt") {
		t.Fatalf("builtin got inputs %v, want one resume.txt absolute path", gotInputs)
	}
	if len(res.InputHashes) != 1 || res.InputHashes[0] != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("candidate resume"))) {
		t.Errorf("input hashes = %v, want one sha256:-prefixed digest", res.InputHashes)
	}
	if len(res.Outputs) != 1 {
		t.Fatalf("want 1 output, got %d", len(res.Outputs))
	}
	out := res.Outputs[0]
	if out.Bytes != int64(len("hello")) {
		t.Errorf("output bytes = %d, want 5", out.Bytes)
	}
	if out.SHA256 != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("hello"))) {
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
	ops := liveTemplateRunOps{workspaceDir: t.TempDir(), nodeID: "n1"}
	_, err := ops.Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-2",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"not-registered"}`),
	}))
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
	ops := liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}
	_, err := ops.Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-3",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-escape"}`),
	}))
	if err == nil {
		t.Fatal("expected an error for an output escaping the workspace, got nil")
	}
}

func TestLiveTemplateRunOps_OutputEscapingPerRunDirectoryRejected(t *testing.T) {
	ws := t.TempDir()
	registerTestBuiltin(t, "test-sibling-output", func(_ context.Context, _ json.RawMessage, _ []string, outDir string) ([]string, error) {
		if err := os.WriteFile(filepath.Join(outDir, "..", "sibling.txt"), []byte("not an output"), 0o644); err != nil {
			return nil, err
		}
		return []string{"../sibling.txt"}, nil
	})
	_, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-sibling",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-sibling-output"}`),
	}))
	if err == nil || !strings.Contains(err.Error(), "per-run output directory") {
		t.Fatalf("sibling output error = %v, want per-run confinement refusal", err)
	}
}

func TestLiveTemplateRunOps_OutputSymlinkOutsidePerRunDirectoryRejected(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	registerTestBuiltin(t, "test-output-symlink", func(_ context.Context, _ json.RawMessage, _ []string, outDir string) ([]string, error) {
		if err := os.Symlink(outside, filepath.Join(outDir, "link.txt")); err != nil {
			return nil, err
		}
		return []string{"link.txt"}, nil
	})
	_, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-symlink",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-output-symlink"}`),
	}))
	if err == nil {
		t.Fatal("output symlink outside the per-run root was accepted")
	}
}

func TestLiveTemplateRunOps_InputHashMatchesStagedBytesBuiltinConsumed(t *testing.T) {
	ws := t.TempDir()
	source := filepath.Join(ws, "source.txt")
	if err := os.WriteFile(source, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	var consumed []byte
	registerTestBuiltin(t, "test-staged-input", func(_ context.Context, _ json.RawMessage, inputs []string, outDir string) ([]string, error) {
		// Change the original after dispatch. The builtin must still consume the
		// staged snapshot whose digest is returned in input_hashes.
		if err := os.WriteFile(source, []byte("after"), 0o644); err != nil {
			return nil, err
		}
		var err error
		consumed, err = os.ReadFile(inputs[0])
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, "result.txt"), consumed, 0o644); err != nil {
			return nil, err
		}
		return []string{"result.txt"}, nil
	})
	req := verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-staged",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-staged-input"}`),
		InputFiles:  json.RawMessage(`[{"path":"source.txt","node_id":"n1","node_path":"source.txt"}]`),
	})
	res, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(consumed) != "before" {
		t.Fatalf("builtin consumed %q, want staged pre-mutation bytes", consumed)
	}
	wantHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("before")))
	if len(res.InputHashes) != 1 || res.InputHashes[0] != wantHash {
		t.Fatalf("input hashes = %v, want %q", res.InputHashes, wantHash)
	}
	inputDir := filepath.Join(ws, "templates", "t", "job-staged", "inputs")
	if _, err := os.Stat(inputDir); !os.IsNotExist(err) {
		t.Fatalf("staged inputs persisted after run: stat error = %v", err)
	}
}

func TestLiveTemplateRunOps_RetryStartsWithCleanRunNamespace(t *testing.T) {
	ws := t.TempDir()
	invocations := 0
	var firstOutDir string
	registerTestBuiltin(t, "test-clean-retry", func(_ context.Context, _ json.RawMessage, _ []string, outDir string) ([]string, error) {
		invocations++
		stale := filepath.Join(outDir, "stale.txt")
		if invocations == 1 {
			firstOutDir = outDir
			if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
				return nil, err
			}
			return nil, errors.New("first attempt failed")
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			return nil, fmt.Errorf("stale output survived retry reset: %v", err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "fresh.txt"), []byte("fresh"), 0o644); err != nil {
			return nil, err
		}
		return []string{"fresh.txt"}, nil
	})
	req := verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-retry",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-clean-retry"}`),
	})
	ops := liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}
	if _, err := ops.Run(context.Background(), req); err == nil {
		t.Fatal("first attempt unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Dir(firstOutDir)); !os.IsNotExist(err) {
		t.Fatalf("failed attempt namespace persisted: stat error = %v", err)
	}
	res, err := ops.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(res.Outputs) != 1 || !strings.HasSuffix(res.Outputs[0].Path, "/fresh.txt") {
		t.Fatalf("retry outputs = %+v, want only fresh.txt", res.Outputs)
	}
}

func TestLiveTemplateRunOps_SymlinkWorkspaceRetargetCannotChangeStagedInput(t *testing.T) {
	parent := t.TempDir()
	workspaceA := filepath.Join(parent, "workspace-a")
	workspaceB := filepath.Join(parent, "workspace-b")
	workspaceLink := filepath.Join(parent, "workspace")
	for _, dir := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(workspaceA, workspaceLink); err != nil {
		t.Skipf("symlink workspace unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceA, "source.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	var consumed []byte
	registerTestBuiltin(t, "test-retargeted-workspace", func(_ context.Context, _ json.RawMessage, inputs []string, outDir string) ([]string, error) {
		inputRel, err := templatePathFromTemplates(inputs[0])
		if err != nil {
			return nil, err
		}
		outRel, err := templatePathFromTemplates(outDir)
		if err != nil {
			return nil, err
		}
		replacementInput := filepath.Join(workspaceB, inputRel)
		if err := os.MkdirAll(filepath.Dir(replacementInput), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(replacementInput, []byte("substituted"), 0o644); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(workspaceB, outRel), 0o755); err != nil {
			return nil, err
		}
		if err := os.Remove(workspaceLink); err != nil {
			return nil, err
		}
		if err := os.Symlink(workspaceB, workspaceLink); err != nil {
			return nil, err
		}
		consumed, err = os.ReadFile(inputs[0])
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, "result.txt"), consumed, 0o644); err != nil {
			return nil, err
		}
		return []string{"result.txt"}, nil
	})

	res, err := (liveTemplateRunOps{workspaceDir: workspaceLink, nodeID: "n1"}).Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-retarget",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-retargeted-workspace"}`),
		InputFiles:  json.RawMessage(`[{"path":"source.txt","node_id":"n1","node_path":"source.txt"}]`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if string(consumed) != "original" {
		t.Fatalf("builtin consumed %q after workspace symlink retarget, want original snapshot", consumed)
	}
	wantHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("original")))
	if len(res.InputHashes) != 1 || res.InputHashes[0] != wantHash {
		t.Fatalf("input hashes = %v, want %q", res.InputHashes, wantHash)
	}
	if len(res.Outputs) != 1 {
		t.Fatalf("outputs = %+v, want one", res.Outputs)
	}
	outputBytes, err := os.ReadFile(filepath.Join(workspaceA, filepath.FromSlash(res.Outputs[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	if string(outputBytes) != "original" {
		t.Fatalf("anchored output = %q, want original", outputBytes)
	}
}

func TestLiveTemplateRunOps_CancelledAttemptCannotOverlapRetryNamespace(t *testing.T) {
	ws := t.TempDir()
	firstStarted := make(chan string, 1)
	releaseFirst := make(chan struct{})
	var invocation atomic.Int32
	registerTestBuiltin(t, "test-cancelled-attempt", func(_ context.Context, _ json.RawMessage, _ []string, outDir string) ([]string, error) {
		switch invocation.Add(1) {
		case 1:
			if err := os.WriteFile(filepath.Join(outDir, "partial.txt"), []byte("partial"), 0o644); err != nil {
				return nil, err
			}
			firstStarted <- outDir
			<-releaseFirst
			return []string{"partial.txt"}, nil
		case 2:
			if err := os.WriteFile(filepath.Join(outDir, "fresh.txt"), []byte("fresh"), 0o644); err != nil {
				return nil, err
			}
			return []string{"fresh.txt"}, nil
		default:
			return nil, fmt.Errorf("unexpected invocation")
		}
	})
	req := verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "same-job",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-cancelled-attempt"}`),
	})
	ops := liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := ops.Run(firstCtx, req)
		firstDone <- err
	}()
	firstOutDir := <-firstStarted
	cancelFirst()

	second, err := ops.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("retry while cancelled builtin was still running: %v", err)
	}
	if len(second.Outputs) != 1 {
		t.Fatalf("retry outputs = %+v, want one", second.Outputs)
	}
	secondPath := filepath.Join(ws, filepath.FromSlash(second.Outputs[0].Path))
	if filepath.Dir(secondPath) == firstOutDir {
		t.Fatal("retry reused the still-running attempt namespace")
	}
	close(releaseFirst)
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled attempt error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Dir(firstOutDir)); !os.IsNotExist(err) {
		t.Fatalf("cancelled attempt namespace persisted: stat error = %v", err)
	}
	if got, err := os.ReadFile(secondPath); err != nil || string(got) != "fresh" {
		t.Fatalf("retry output after old cleanup = %q, %v", got, err)
	}
}

func TestCopyTemplateWithContextStopsBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := cancelAfterFirstRead{cancel: cancel, reader: bytes.NewReader(make([]byte, 256*1024))}
	var dst bytes.Buffer
	n, err := copyTemplateWithContext(ctx, &dst, &src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy error = %v, want context.Canceled", err)
	}
	if n != 0 || dst.Len() != 0 {
		t.Fatalf("copy wrote %d reported bytes and %d buffered bytes after read cancelled the context", n, dst.Len())
	}
}

func TestLiveTemplateRunOps_PartialStagingFailureRemovesAttempt(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "present.txt"), []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	registerTestBuiltin(t, "test-partial-staging", func(context.Context, json.RawMessage, []string, string) ([]string, error) {
		called = true
		return nil, nil
	})
	_, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID:       "job-partial",
		TemplateKey: "t",
		Runner:      json.RawMessage(`{"kind":"builtin","handler":"test-partial-staging"}`),
		InputFiles: json.RawMessage(`[
			{"path":"present.txt","node_id":"n1","node_path":"present.txt"},
			{"path":"missing.txt","node_id":"n1","node_path":"missing.txt"}
		]`),
	}))
	if err == nil {
		t.Fatal("partial staging unexpectedly succeeded")
	}
	if called {
		t.Fatal("builtin ran after partial staging failure")
	}
	jobDir := filepath.Join(ws, "templates", "t", "job-partial")
	entries, readErr := os.ReadDir(jobDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial staging left attempt directories: %v", entries)
	}
}

func TestResolveTemplateInputsTotalBytesBoundaryAndDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    map[string]string
		declared []string
		maxBytes int64
		wantErr  bool
	}{
		{name: "exact", files: map[string]string{"a": "abc", "b": "de"}, declared: []string{"a", "b"}, maxBytes: 5},
		{name: "one_over", files: map[string]string{"a": "abc", "b": "def"}, declared: []string{"a", "b"}, maxBytes: 5, wantErr: true},
		{name: "duplicate_counts_as_declared_bytes", files: map[string]string{"a": "abc"}, declared: []string{"a", "a"}, maxBytes: 5, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			for name, contents := range tc.files {
				if err := os.WriteFile(filepath.Join(ws, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(ws)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.Mkdir("staged", 0o700); err != nil {
				t.Fatal(err)
			}
			staged, err := root.OpenRoot("staged")
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Close()
			files := make([]jobs.TemplateInputFile, 0, len(tc.declared))
			for _, name := range tc.declared {
				files = append(files, jobs.TemplateInputFile{Path: name, NodeID: "n1", NodePath: name})
			}
			_, _, _, err = (liveTemplateRunOps{nodeID: "n1"}).resolveInputs(context.Background(), files, root, staged, filepath.Join(ws, "staged"), tc.maxBytes)
			if tc.wantErr != (err != nil) {
				t.Fatalf("resolveInputs error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestTemplateOutputCapsBoundaryAndUnreportedFiles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    map[string]string
		reported []string
		maxFiles int
		maxBytes int64
		wantErr  bool
	}{
		{name: "exact", files: map[string]string{"a": "abc", "b": "de"}, reported: []string{"a", "b"}, maxFiles: 2, maxBytes: 5},
		{name: "one_byte_over", files: map[string]string{"a": "abc", "b": "def"}, reported: []string{"a", "b"}, maxFiles: 2, maxBytes: 5, wantErr: true},
		{name: "one_file_over", files: map[string]string{"a": "a", "b": "b"}, reported: []string{"a", "b"}, maxFiles: 1, maxBytes: 5, wantErr: true},
		{name: "duplicate_report", files: map[string]string{"a": "a"}, reported: []string{"a", "a"}, maxFiles: 2, maxBytes: 5, wantErr: true},
		{name: "unreported", files: map[string]string{"a": "a", "hidden": "x"}, reported: []string{"a"}, maxFiles: 2, maxBytes: 5, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, contents := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = validateTemplateOutputTree(root, tc.reported, tc.maxFiles, tc.maxBytes)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateTemplateOutputTree error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestLiveTemplateRunOps_OutputCountFailureCleansAttempt(t *testing.T) {
	ws := t.TempDir()
	registerTestBuiltin(t, "test-too-many-outputs", func(_ context.Context, _ json.RawMessage, _ []string, outDir string) ([]string, error) {
		outputs := make([]string, templateMaxOutputFiles+1)
		for i := range outputs {
			outputs[i] = fmt.Sprintf("%02d.txt", i)
			if err := os.WriteFile(filepath.Join(outDir, outputs[i]), []byte("x"), 0o600); err != nil {
				return nil, err
			}
		}
		return outputs, nil
	})
	_, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), verifiedTemplateRequest(t, worker.TemplateRunRequest{
		JobID: "job-output-cap", TemplateKey: "t", Runner: json.RawMessage(`{"kind":"builtin","handler":"test-too-many-outputs"}`),
	}))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("output count error = %v", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(ws, "templates", "t", "job-output-cap"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("over-limit output left attempt directories: %v", entries)
	}
}

func TestPruneRetainedTemplateAttempts(t *testing.T) {
	ws := t.TempDir()
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	now := time.Unix(2_000_000_000, 0)
	old := now.Add(-templateOutputRetention - time.Second)
	cutoff := now.Add(-templateOutputRetention)
	fresh := now.Add(-templateOutputRetention + time.Second)

	makeAttempt := func(name string, marker bool, inputs bool, stamp time.Time) string {
		t.Helper()
		rel := filepath.Join("templates", "t", "job", name)
		if err := root.MkdirAll(filepath.Join(rel, "out"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(filepath.Join(rel, "out", "result"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if inputs {
			if err := root.Mkdir(filepath.Join(rel, "inputs"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if marker {
			markerPath := filepath.Join(rel, templateCompleteMarker)
			if err := root.WriteFile(markerPath, []byte("complete\n"), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := root.Chtimes(markerPath, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		} else if err := root.Chtimes(rel, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return rel
	}

	oldMarked := makeAttempt("attempt-00000000000000000000000000000001", true, false, old)
	legacyOld := makeAttempt("attempt-00000000000000000000000000000002", false, false, old)
	freshMarked := makeAttempt("attempt-00000000000000000000000000000003", true, false, fresh)
	exactCutoff := makeAttempt("attempt-00000000000000000000000000000004", true, false, cutoff)
	activeOld := makeAttempt("attempt-00000000000000000000000000000005", false, true, old)
	notAttempt := makeAttempt("attempt-not-random", true, false, old)

	if err := pruneRetainedTemplateAttempts(root, now, templateOutputRetention); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{oldMarked, legacyOld} {
		if _, err := root.Stat(rel); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expired attempt %s survived: %v", rel, err)
		}
	}
	for _, rel := range []string{freshMarked, exactCutoff, activeOld, notAttempt} {
		if _, err := root.Stat(rel); err != nil {
			t.Errorf("protected attempt %s was removed: %v", rel, err)
		}
	}
}

func TestPruneRetainedTemplateAttemptsFailsClosedOnInvalidMarker(t *testing.T) {
	ws := t.TempDir()
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rel := filepath.Join("templates", "t", "job", "attempt-00000000000000000000000000000001")
	if err := root.MkdirAll(filepath.Join(rel, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir(filepath.Join(rel, templateCompleteMarker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pruneRetainedTemplateAttempts(root, time.Now(), templateOutputRetention); err == nil {
		t.Fatal("non-file completion marker was accepted")
	}
}

type cancelAfterFirstRead struct {
	cancel context.CancelFunc
	reader *bytes.Reader
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}

func templatePathFromTemplates(path string) (string, error) {
	marker := string(filepath.Separator) + "templates" + string(filepath.Separator)
	i := strings.Index(path, marker)
	if i < 0 {
		return "", fmt.Errorf("path %q has no templates segment", path)
	}
	return path[i+1:], nil
}
