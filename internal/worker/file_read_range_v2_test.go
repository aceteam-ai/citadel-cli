package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/artifact"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

func TestConditionalRangeCapabilityGatesAndExecutionRevoke(t *testing.T) {
	ws := t.TempDir()
	for _, opts := range []LegacyHandlerOpts{{}, {WorkspaceDir: ws, FilesDisabled: true}, {WorkspaceDir: ws, FilesEnabled: func() bool { return false }}} {
		runner := NewRunner(nil, CreateLegacyHandlersWithOpts(opts), RunnerConfig{})
		for _, jt := range runner.SupportedJobTypes() {
			if jt == JobTypeFileReadRangeV2 {
				t.Fatalf("advertised unavailable V2 with %+v", opts)
			}
		}
	}
	enabled := true
	runner := NewRunner(nil, CreateLegacyHandlersWithOpts(LegacyHandlerOpts{WorkspaceDir: ws, FilesEnabled: func() bool { return enabled }}), RunnerConfig{})
	var selected JobHandler
	for _, handler := range runner.handlers {
		if handler.CanHandle(JobTypeFileReadRangeV2) {
			selected = handler
		}
	}
	advertised := false
	for _, jt := range runner.SupportedJobTypes() {
		advertised = advertised || jt == JobTypeFileReadRangeV2
	}
	if advertised != artifact.Supported() || (selected != nil) != artifact.Supported() {
		t.Fatalf("platform gate mismatch advertised=%v selected=%v", advertised, selected)
	}
	if !artifact.Supported() {
		return
	}
	// A valid request proves denial comes from the execution permission gate.
	path := filepath.Join(ws, "media")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	identity, err := artifact.Capture(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("original")))
	generation, _ := identity.Generation(hash)
	payload := map[string]any{"path": path, "offset": "0", "length": "4", "expected_artifact_version": "1", "expected_sha256": hash, "expected_bytes": "8", "expected_generation": generation}
	enabled = false
	if selected.CanHandle(JobTypeFileReadRangeV2) {
		t.Fatal("revoked Files permission left routing enabled")
	}
	for _, jt := range runner.SupportedJobTypes() {
		if jt == JobTypeFileReadRangeV2 {
			t.Fatal("revoked Files permission left capability advertised")
		}
	}
	result, err := selected.Execute(context.Background(), &Job{ID: "revoked", Type: JobTypeFileReadRangeV2, Payload: payload}, &NoOpStreamWriter{})
	if err == nil || result == nil || result.Status != JobStatusFailure {
		t.Fatalf("execution revoke did not refuse: %+v %v", result, err)
	}
	legacy := NewRunner(nil, []JobHandler{NewLegacyHandlerAdapter(JobTypeFileReadRangeV1, jobs.NewFileReadBytesRangeHandler(ws))}, RunnerConfig{})
	for _, jt := range legacy.SupportedJobTypes() {
		if jt == JobTypeFileReadRangeV2 {
			t.Fatal("V1-only node advertised V2")
		}
	}
}

func TestConditionalRangeTypedOutcomesSurviveRealAdapter(t *testing.T) {
	if !artifact.Supported() {
		t.Skip("generation is Linux-only")
	}
	ws := t.TempDir()
	path := filepath.Join(ws, "media")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	identity, err := artifact.Capture(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("original")))
	generation, _ := identity.Generation(hash)
	payload := map[string]any{"path": path, "offset": "8", "length": "4", "expected_artifact_version": "1", "expected_sha256": hash, "expected_bytes": "8", "expected_generation": generation}
	adapter := newGatedLegacyHandlerAdapter(JobTypeFileReadRangeV2, jobs.NewFileReadBytesRangeV2Handler(ws), func() bool { return true })
	for _, outcome := range []string{"range_not_satisfiable", "precondition_failed"} {
		if outcome == "precondition_failed" {
			if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		result, err := adapter.Execute(context.Background(), &Job{ID: "typed", Type: JobTypeFileReadRangeV2, Payload: payload}, &NoOpStreamWriter{})
		if err != nil || result == nil || result.Status != JobStatusSuccess {
			t.Fatalf("domain refusal flattened into error: %+v %v", result, err)
		}
		wire, err := json.Marshal(result.Output)
		if err != nil {
			t.Fatal(err)
		}
		var transported struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(wire, &transported); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(transported.Output), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["contract_version"] != float64(2) || decoded["outcome"] != outcome {
			t.Fatalf("typed outcome lost through transport: %s", wire)
		}
		if _, ok := decoded["content"]; ok {
			t.Fatal("refusal included bytes")
		}
		if _, ok := decoded["encoding"]; ok {
			t.Fatal("refusal included encoding")
		}
		if outcome == "precondition_failed" && len(decoded) != 2 {
			t.Fatalf("precondition disclosed replacement metadata: %+v", decoded)
		}
		if outcome == "range_not_satisfiable" && (len(decoded) != 5 || decoded["total_size"] != float64(8) || decoded["offset"] != float64(8)) {
			t.Fatalf("EOF metadata lost: %+v", decoded)
		}
	}
}
