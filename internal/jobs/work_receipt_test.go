package jobs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// inMemSigner is a hermetic ECDSA P-256 aep.Signer for tests — it NEVER touches
// disk or the real node identity (node 1795 on this box), so these tests can
// exercise signing without minting/reading real key material.
type inMemSigner struct {
	key *ecdsa.PrivateKey
}

func newInMemSigner(t *testing.T) *inMemSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &inMemSigner{key: key}
}

func (s *inMemSigner) Sign(payload []byte) ([]byte, error) {
	digest := sha256.Sum256(payload)
	return ecdsa.SignASN1(rand.Reader, s.key, digest[:])
}

func (s *inMemSigner) PublicKeyFingerprint() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// signErrSigner resolves a fingerprint (so Preflight passes) but fails to Sign,
// exercising the emit-time fail-closed path.
type signErrSigner struct{ inMemSigner }

func (s *signErrSigner) Sign([]byte) ([]byte, error) { return nil, errors.New("no key") }

func writeIndexFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("The cat sat on the mat."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("A database stores rows."), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decodeOut(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return m
}

func firstWorkReceipt(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	recs, ok := out["work_receipts"].([]any)
	if !ok || len(recs) == 0 {
		t.Fatalf("expected a work_receipts list, got %v", out["work_receipts"])
	}
	m, ok := recs[0].(map[string]any)
	if !ok {
		t.Fatalf("work receipt is %T", recs[0])
	}
	return m
}

// TestFileIndexDispatchedSignsReceiptAndRecord: a dispatched FILE_INDEX produces
// a signed v2 work receipt whose input_sha256 == the result's
// work_manifest_sha256, plus a usage record stamped origin=dispatched/signed.
func TestFileIndexDispatchedSignsReceiptAndRecord(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", "")

	ws := t.TempDir()
	writeIndexFixture(t, ws)
	dbPath := filepath.Join(t.TempDir(), "index.db")

	h := NewFileIndexHandler(ws, dbPath)
	h.Signer = newInMemSigner(t)
	h.Dispatched = true
	h.Origin = OriginDispatched

	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "idx-1", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("FILE_INDEX: %v", err)
	}
	res := decodeOut(t, out)

	// Existing C1/C2 result keys are still present & unchanged in shape.
	for _, k := range []string{"files_indexed", "files_skipped", "status", "checkpoint", "chunks_upserted"} {
		if _, ok := res[k]; !ok {
			t.Errorf("result missing pre-existing key %q", k)
		}
	}

	manifestSHA, _ := res["work_manifest_sha256"].(string)
	if manifestSHA == "" {
		t.Fatal("missing work_manifest_sha256")
	}
	receipt := firstWorkReceipt(t, res)
	if receipt["input_sha256"] != manifestSHA {
		t.Errorf("receipt input_sha256 %v != work_manifest_sha256 %q", receipt["input_sha256"], manifestSHA)
	}
	if receipt["action"] != WorkActionIndex {
		t.Errorf("receipt action = %v, want %q", receipt["action"], WorkActionIndex)
	}
	if receipt["signature"] == "" || receipt["public_key_fingerprint"] == "" {
		t.Error("receipt is not signed")
	}

	usg, ok := res["usage"].(map[string]any)
	if !ok {
		t.Fatal("missing usage object")
	}
	if usg["origin"] != OriginDispatched {
		t.Errorf("usage origin = %v, want %q", usg["origin"], OriginDispatched)
	}
	if usg["signed"] != true {
		t.Errorf("usage signed = %v, want true", usg["signed"])
	}
}

// TestFileIndexDispatchedFailsClosedWithoutSigner: dispatched work with NO signer
// fails closed with ErrReceiptSigningUnavailable (and fast — at preflight).
func TestFileIndexDispatchedFailsClosedWithoutSigner(t *testing.T) {
	ws := t.TempDir()
	writeIndexFixture(t, ws)
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	h.Dispatched = true // but Signer is nil

	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "idx-nf", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if !errors.Is(err, ErrReceiptSigningUnavailable) {
		t.Fatalf("expected ErrReceiptSigningUnavailable, got %v", err)
	}
}

// TestFileIndexDispatchedFailsClosedOnSignError: dispatched work whose signer
// resolves a fingerprint (preflight passes) but fails to Sign still fails closed
// at emit time rather than succeeding unsigned.
func TestFileIndexDispatchedFailsClosedOnSignError(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	ws := t.TempDir()
	writeIndexFixture(t, ws)
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	h.Signer = &signErrSigner{*newInMemSigner(t)}
	h.Dispatched = true

	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "idx-se", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if !errors.Is(err, ErrReceiptSigningUnavailable) {
		t.Fatalf("expected ErrReceiptSigningUnavailable on sign error, got %v", err)
	}
}

// TestFileIndexLocalUnsignedIndexesFine: the local_cli path (no signer,
// Dispatched=false) indexes fine, writes NO receipt, and never fails closed.
func TestFileIndexLocalUnsignedIndexesFine(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	ws := t.TempDir()
	writeIndexFixture(t, ws)
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	h.Origin = OriginLocalCLI // no signer, Dispatched stays false

	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "idx-local", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}})
	if err != nil {
		t.Fatalf("local index should not fail without a signer: %v", err)
	}
	res := decodeOut(t, out)
	if _, present := res["work_receipts"]; present {
		t.Error("local index must not produce a signed receipt")
	}
	usg := res["usage"].(map[string]any)
	if usg["origin"] != OriginLocalCLI {
		t.Errorf("usage origin = %v, want %q", usg["origin"], OriginLocalCLI)
	}
	if usg["signed"] != false {
		t.Errorf("usage signed = %v, want false", usg["signed"])
	}
	if _, ok := res["work_manifest_sha256"].(string); !ok {
		t.Error("local index should still record the manifest hash")
	}
}

// TestFileIndexNamespaceBindsReceipt: two orgs indexing BYTE-IDENTICAL content
// under different namespaces produce DISTINCT receipts (the namespace is bound
// into input_sha256). aceteam#10876 C7.
func TestFileIndexNamespaceBindsReceipt(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", "")

	indexesDir := t.TempDir()
	signer := newInMemSigner(t)

	// Isolate the NAMESPACE as the only difference: index the SAME workspace (same
	// path, byte-identical content) twice under two different org namespaces.
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "doc.md"), []byte("identical content for both orgs"), 0o644); err != nil {
		t.Fatal(err)
	}
	index := func(ns string) map[string]any {
		h := NewFileIndexHandler(ws, "")
		h.IndexesDir = indexesDir
		h.Signer = signer
		h.Dispatched = true
		h.Origin = OriginDispatched
		out, err := h.Execute(JobContext{}, &nexus.Job{ID: "ns2-" + ns, Type: "FILE_INDEX", Payload: map[string]string{"path": ws, "index": ns}})
		if err != nil {
			t.Fatalf("index ns %s: %v", ns, err)
		}
		return decodeOut(t, out)
	}

	a := index("org_1/docs")
	b := index("org_2/docs")
	ra := firstWorkReceipt(t, a)
	rb := firstWorkReceipt(t, b)
	if ra["input_sha256"] == rb["input_sha256"] {
		t.Fatalf("two orgs indexing identical content produced the SAME receipt input_sha256 %v — namespace not bound", ra["input_sha256"])
	}
	// The org attribution is derived from the namespace.
	if a["usage"].(map[string]any)["org_id"] != "org_1" {
		t.Errorf("org_id = %v, want org_1", a["usage"].(map[string]any)["org_id"])
	}
}

// TestEmbeddingDispatchedSignsAndMetersTokensBytes: a dispatched embedding job
// signs an embed_served receipt and records NON-ZERO tokens AND bytes (the
// aceteam#10876 C7 acceptance).
func TestEmbeddingDispatchedSignsAndMetersTokensBytes(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)

	h := &EmbeddingHandler{Signer: newInMemSigner(t), Dispatched: true, Origin: OriginDispatched}
	payload := map[string]string{"model": "gte-multilingual-base", "input": `["hello world","second text"]`}
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "emb-1", Type: "embedding", Payload: payload})
	if err != nil {
		t.Fatalf("embedding: %v", err)
	}
	res := decodeOut(t, out)

	// Existing embedding result keys survive.
	if _, ok := res["embeddings"]; !ok {
		t.Error("result missing embeddings")
	}
	usg := res["usage"].(map[string]any)
	if toI(usg["total_tokens"]) <= 0 {
		t.Errorf("total_tokens = %v, want > 0", usg["total_tokens"])
	}
	if toI(usg["request_bytes"]) <= 0 || toI(usg["response_bytes"]) <= 0 {
		t.Errorf("bytes must be non-zero: request=%v response=%v", usg["request_bytes"], usg["response_bytes"])
	}
	if usg["action"] != WorkActionEmbedServed {
		t.Errorf("usage action = %v, want %q", usg["action"], WorkActionEmbedServed)
	}
	receipt := firstWorkReceipt(t, res)
	if receipt["action"] != WorkActionEmbedServed || receipt["signature"] == "" {
		t.Errorf("embed_served receipt not signed correctly: %v", receipt)
	}
}

// TestEmbeddingDispatchedFailsClosedWithoutSigner: dispatched embedding with no
// signer fails closed.
func TestEmbeddingDispatchedFailsClosedWithoutSigner(t *testing.T) {
	h := &EmbeddingHandler{Dispatched: true} // nil signer
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "emb-nf", Type: "embedding", Payload: map[string]string{"model": "m", "input": `["x"]`}})
	if !errors.Is(err, ErrReceiptSigningUnavailable) {
		t.Fatalf("expected ErrReceiptSigningUnavailable, got %v", err)
	}
}

func toI(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
