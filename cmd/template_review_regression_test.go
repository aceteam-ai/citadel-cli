package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestReviewForeignNodeInputRefused(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "same-name.txt"), []byte("unrelated local content"), 0600); err != nil {
		t.Fatal(err)
	}
	o := liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}
	got, hashes, err := o.resolveInputs(json.RawMessage(`[{"path":"node:other-node/same-name.txt","node_id":"other-node","node_path":"same-name.txt"}]`))
	if err == nil {
		t.Fatalf("foreign node reference consumed local file: paths=%v hashes=%v", got, hashes)
	}
}

func TestLiveTemplateRunOps_RefusesBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*worker.TemplateRunRequest)
	}{
		{"malformed_params", func(r *worker.TemplateRunRequest) { r.Params = json.RawMessage(`{"gain":`) }},
		{"array_params", func(r *worker.TemplateRunRequest) { r.Params = json.RawMessage(`[]`) }},
		{"invalid_schema_params", func(r *worker.TemplateRunRequest) { r.Params = json.RawMessage(`{"gain":"loud"}`) }},
		{"malformed_files", func(r *worker.TemplateRunRequest) { r.InputFiles = json.RawMessage(`[{`) }},
		{"foreign_files", func(r *worker.TemplateRunRequest) {
			r.InputFiles = json.RawMessage(`[{"path":"node:other-node/same-name.txt","node_id":"other-node","node_path":"same-name.txt"}]`)
		}},
		{"missing_identity", func(r *worker.TemplateRunRequest) {
			r.InputFiles = json.RawMessage(`[{"path":"same-name.txt","node_path":"same-name.txt"}]`)
		}},
		{"mixed_foreign_after_local", func(r *worker.TemplateRunRequest) {
			r.InputFiles = json.RawMessage(`[{"path":"node:n1/same-name.txt","node_id":"n1","node_path":"same-name.txt"},{"path":"node:other/a.txt","node_id":"other","node_path":"a.txt"}]`)
		}},
		{"mismatch_hash", func(r *worker.TemplateRunRequest) { r.ContentHash = "deadbeef" }},
		{"ambiguous_runner", func(r *worker.TemplateRunRequest) {
			r.Runner = json.RawMessage(`{"kind":"builtin","handler":"refusal-test","Handler":"evil"}`)
			rehashLiveRequest(t, r)
		}},
		{"shell_runner", func(r *worker.TemplateRunRequest) {
			r.Runner = json.RawMessage(`{"kind":"shell","handler":"refusal-test"}`)
			rehashLiveRequest(t, r)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			// A matching local filename must not satisfy a foreign reference.
			if err := os.WriteFile(filepath.Join(ws, "same-name.txt"), []byte("unrelated"), 0600); err != nil {
				t.Fatal(err)
			}
			called := false
			registerTestBuiltin(t, "refusal-test", func(context.Context, json.RawMessage, []string, string) ([]string, error) {
				called = true
				return nil, nil
			})
			r := verifiedTemplateRequest(t, worker.TemplateRunRequest{JobID: "refusal", TemplateKey: "test", Runner: json.RawMessage(`{"kind":"builtin","handler":"refusal-test"}`)})
			r.InputSchema = json.RawMessage(`{"type":"object","properties":{"gain":{"type":"number"}}}`)
			rehashLiveRequest(t, &r)
			tc.change(&r)
			_, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).Run(context.Background(), r)
			if err == nil || called {
				t.Fatalf("invalid request reached builtin: err=%v called=%v", err, called)
			}
			entries, readErr := os.ReadDir(ws)
			if readErr != nil || len(entries) != 1 || entries[0].Name() != "same-name.txt" {
				t.Fatalf("invalid request wrote workspace: %v %v", entries, readErr)
			}
		})
	}
}

func rehashLiveRequest(t *testing.T, r *worker.TemplateRunRequest) {
	t.Helper()
	hash, err := jobs.ComputeTemplateManifestHash(r.TemplateKey, r.TemplateVersion, r.InputSchema, r.OutputSchema, r.Runner)
	if err != nil {
		t.Fatal(err)
	}
	r.ContentHash = hash
}

func TestLiveTemplateRunOps_InputSymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Fatal(err)
	}
	_, _, err := (liveTemplateRunOps{workspaceDir: ws, nodeID: "n1"}).resolveInputs(json.RawMessage(`[{"path":"node:n1/escape","node_id":"n1","node_path":"escape"}]`))
	if err == nil {
		t.Fatal("symlink outside workspace accepted")
	}
}
