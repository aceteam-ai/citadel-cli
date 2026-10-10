package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goodSigner is a hermetic in-memory ECDSA signer (never touches the real node
// identity). fpErrSigner resolves no fingerprint, so a handler's work-receipt
// preflight fails — the "signing unavailable on a dispatched node" case.
type goodSigner struct{ key *ecdsa.PrivateKey }

func newGoodSigner(t *testing.T) *goodSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &goodSigner{key: k}
}
func (s *goodSigner) Sign(p []byte) ([]byte, error) {
	d := sha256.Sum256(p)
	return ecdsa.SignASN1(rand.Reader, s.key, d[:])
}
func (s *goodSigner) PublicKeyFingerprint() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type fpErrSigner struct{}

func (fpErrSigner) Sign([]byte) ([]byte, error)           { return nil, errors.New("no key") }
func (fpErrSigner) PublicKeyFingerprint() (string, error) { return "", errors.New("no identity key") }

// fakeTEIServer is a minimal stub of the node's TEI embedding service.
func fakeTEIServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type di struct {
			Object    string    `json:"object"`
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		}
		var data []di
		for i := range req.Input {
			data = append(data, di{Object: "embedding", Embedding: []float64{0.1, 0.2}, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "model": "m", "data": data,
			"usage": map[string]int{"prompt_tokens": 2, "total_tokens": 2},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunnerReceiptSigningUnavailablePublishesTerminalErrorWithoutRetry mirrors
// the Files-disabled terminal-error test: a dispatched FILE_INDEX whose node has
// no signing key fails CLOSED — exactly one terminal error with reason
// receipt_signing_unavailable, source.Fail (not Nack/retry), on every delivery.
func TestRunnerReceiptSigningUnavailablePublishesTerminalErrorWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata JobMetadata
	}{
		{"first_redis_delivery", JobMetadata{Attempts: 1, MaxAttempts: 3}},
		{"final_redis_delivery", JobMetadata{Attempts: 2, MaxAttempts: 3}},
		{"without_delivery_metadata", JobMetadata{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "a.md"), []byte("hello"), 0o644); err != nil {
				t.Fatal(err)
			}
			source := NewMockJobSource("test", nil)
			// A non-nil signer that cannot resolve a fingerprint => the FILE_INDEX
			// handler is DISPATCHED (fail-closed) but preflight fails.
			handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{
				WorkspaceDir:      ws,
				WorkReceiptSigner: fpErrSigner{},
			})
			runner := NewRunner(source, handlers, RunnerConfig{WorkerID: "test-worker"})
			stream := &MockStreamWriter{}
			job := &Job{
				ID: "idx-nokey", Type: JobTypeFileIndex,
				Payload:  map[string]any{"path": ws},
				Metadata: tc.metadata,
			}

			if runner.executeJob(context.Background(), job, stream, time.Now(), false, 0) {
				t.Fatal("a signing-unavailable refusal must not report job success")
			}
			if stream.errorCount != 1 || stream.endCount != 0 || stream.erroredRecover {
				t.Fatalf("expected exactly one non-recoverable error: errors=%d ends=%d recoverable=%v", stream.errorCount, stream.endCount, stream.erroredRecover)
			}
			var refusal struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(stream.erroredErr.Error()), &refusal); err != nil || refusal.Reason != "receipt_signing_unavailable" {
				t.Fatalf("terminal event lost typed refusal: reason=%q err=%v", refusal.Reason, err)
			}
			if len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 || len(source.AckedJobs()) != 0 {
				t.Fatalf("expected source.Fail only: failed=%d nacked=%d acked=%d", len(source.FailedJobs()), len(source.NackedJobs()), len(source.AckedJobs()))
			}
			if source.FailedData()[0]["reason"] != "receipt_signing_unavailable" {
				t.Fatalf("failed-job metadata lost refusal reason: %#v", source.FailedData())
			}
		})
	}
}

// TestBuildUsageRecordReadsNestedUsage pins the "usage key fix": a legacy
// handler's JSON output (wrapped as Output["output"] string) carrying a nested
// `usage` object flows onto the usage record (attribution + units + tokens/
// bytes + receipt binding).
func TestBuildUsageRecordReadsNestedUsage(t *testing.T) {
	inner := map[string]any{
		"files_indexed": 3,
		"usage": map[string]any{
			"origin":          "dispatched",
			"org_id":          "org_7",
			"action":          "embed_served",
			"signed":          true,
			"manifest_sha256": "sha256:abc",
			"units":           map[string]any{"embeddings": 2, "total_tokens": 9},
			"prompt_tokens":   4,
			"total_tokens":    9,
			"request_bytes":   120,
			"response_bytes":  340,
		},
	}
	innerJSON, _ := json.Marshal(inner)
	result := &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{"output": string(innerJSON)},
	}
	job := &Job{ID: "j1", Type: "embedding"}

	rec := buildUsageRecord(job, "success", time.Now(), time.Now(), result, nil)
	if rec.Origin != "dispatched" || rec.OrgID != "org_7" || rec.WorkAction != "embed_served" {
		t.Errorf("attribution = (%q,%q,%q)", rec.Origin, rec.OrgID, rec.WorkAction)
	}
	if !rec.ReceiptSigned || rec.ReceiptManifestSHA256 != "sha256:abc" {
		t.Errorf("receipt binding = (%v,%q)", rec.ReceiptSigned, rec.ReceiptManifestSHA256)
	}
	if rec.PromptTokens != 4 || rec.TotalTokens != 9 || rec.RequestBytes != 120 || rec.ResponseBytes != 340 {
		t.Errorf("token/byte metrics = (%d,%d,%d,%d)", rec.PromptTokens, rec.TotalTokens, rec.RequestBytes, rec.ResponseBytes)
	}
	if rec.Units["embeddings"] != 2 || rec.Units["total_tokens"] != 9 {
		t.Errorf("units = %v", rec.Units)
	}
}

// TestBuildUsageRecordNativeUsageUnchanged pins that the native (_usage_*) path
// still works and is unaffected by the nested-usage addition.
func TestBuildUsageRecordNativeUsageUnchanged(t *testing.T) {
	result := &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{
			"_usage_prompt_tokens": 10,
			"_usage_total_tokens":  30,
		},
	}
	rec := buildUsageRecord(&Job{ID: "n1", Type: "llm_inference"}, "success", time.Now(), time.Now(), result, nil)
	if rec.PromptTokens != 10 || rec.TotalTokens != 30 {
		t.Errorf("native _usage_* read regressed: prompt=%d total=%d", rec.PromptTokens, rec.TotalTokens)
	}
	if rec.WorkAction != "" || rec.Origin != "" {
		t.Errorf("native path must not populate work attribution: action=%q origin=%q", rec.WorkAction, rec.Origin)
	}
}

// TestCreateLegacyHandlers_FileIndexSignsWhenSignerWired is the construction-site
// guard: when a signer is threaded through CreateLegacyHandlersWithOpts, a
// FILE_INDEX dispatch produces a SIGNED work receipt; without one it is unsigned.
// A future refactor that drops the signer threading flips this.
func TestCreateLegacyHandlers_FileIndexSignsWhenSignerWired(t *testing.T) {
	tei := fakeTEIServer(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "idx.db"))

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.md"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(signerWired bool) map[string]any {
		opts := LegacyHandlerOpts{WorkspaceDir: ws}
		if signerWired {
			opts.WorkReceiptSigner = newGoodSigner(t)
		}
		var h JobHandler
		for _, c := range CreateLegacyHandlersWithOpts(opts) {
			if c.CanHandle(JobTypeFileIndex) {
				h = c
				break
			}
		}
		if h == nil {
			t.Fatal("no FILE_INDEX handler registered")
		}
		res, err := h.Execute(context.Background(), &Job{ID: "ci", Type: JobTypeFileIndex, Payload: map[string]any{"path": ws}}, &MockStreamWriter{})
		if err != nil {
			t.Fatalf("FILE_INDEX execute: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Output["output"].(string)), &out); err != nil {
			t.Fatalf("decode output: %v", err)
		}
		return out
	}

	signed := run(true)
	if _, ok := signed["work_receipts"]; !ok {
		t.Error("a signer was wired but FILE_INDEX produced no signed receipt (construction-site wiring dropped?)")
	}
	if usg, ok := signed["usage"].(map[string]any); !ok || usg["signed"] != true || usg["origin"] != "dispatched" {
		t.Errorf("wired FILE_INDEX usage = %v, want signed dispatched", signed["usage"])
	}

	unsigned := run(false)
	if _, ok := unsigned["work_receipts"]; ok {
		t.Error("no signer wired but FILE_INDEX produced a receipt")
	}
}
