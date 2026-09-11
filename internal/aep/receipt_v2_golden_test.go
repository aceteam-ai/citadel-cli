package aep

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// updateGolden regenerates the cross-repo golden fixtures under
// internal/aep/testdata/v2 (run: `go test ./internal/aep -run Golden -update-golden`).
// Use it only when the v2 canon or the sample is deliberately changed; the
// committed fixtures are the contract a follow-up aceteam PR asserts its
// verifier reproduces.
var updateGolden = flag.Bool("update-golden", false, "regenerate internal/aep/testdata/v2 golden fixtures")

// The fixed golden sample. Deliberately PROMPT-based (not messages): input.bin
// is the exact prompt bytes, so BOTH the production node path (sha256Hex of
// payload.Prompt in applyTrustEngine) and the backend's _expected_input_sha256
// (sha256 of the bare prompt) reproduce input_sha256 — a messages fixture would
// pass BuildSignedReceiptV2 while misrepresenting what the wiring can bind.
const (
	goldenInput    = "In the survey, a majority agreed and a small fraction disagreed."
	goldenContent  = "A large majority agreed; only a small fraction disagreed."
	goldenJobID    = "job-golden-v2"
	goldenEngine   = "bonsai"
	goldenModel    = "bonsai-27b"
	goldenIssuedAt = "2026-09-06T12:00:00Z"
)

// goldenGrounding is constructed directly (not via trust.CheckGrounding) so the
// fixture is decoupled from the grounding heuristic's evolution: a fabricated
// percentage flagged, ungrounded, two claims checked.
func goldenGrounding() trust.GroundingResult {
	return trust.GroundingResult{
		Grounded:      false,
		Score:         0.5,
		ClaimsChecked: 2,
		Flagged: []trust.Claim{
			{Value: "68%", Kind: trust.ClaimPercent, Reason: "not supported by input"},
		},
	}
}

func sha256Prefixed(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func goldenV2Inputs(verdict trust.Verdict) V2Inputs {
	return V2Inputs{
		InputSHA256:  sha256Prefixed(goldenInput),
		OutputSHA256: sha256Prefixed(goldenContent),
		PolicyHash:   EmptyPolicyHash,
		Action:       verdict.Action,
		VerdictHash:  verdict.VerdictHash,
	}
}

func v2TestdataDir() string { return filepath.Join("testdata", "v2") }

// TestGoldenReceiptV2 is the cross-repo golden: it rebuilds the fixed sample
// receipt from the committed test key and fixed issued_at, asserts CanonicalizeV2
// reproduces the committed canonical.bin byte-for-byte, that each digest field
// equals its committed value, and that the committed (randomized) signature in
// receipt.json verifies against the committed leaf — never byte-comparing the
// signature (ECDSA is non-deterministic). With -update-golden it regenerates the
// whole directory instead.
func TestGoldenReceiptV2(t *testing.T) {
	dir := v2TestdataDir()

	grounding := goldenGrounding()
	verdict := trust.BuildVerdict(goldenInput, goldenContent, grounding, trust.DefaultDetectors(), EmptyPolicyHash)
	inputs := goldenV2Inputs(verdict)

	if *updateGolden {
		regenerateGoldenV2(t, dir, grounding, verdict, inputs)
		return
	}

	key := loadGoldenKey(t, filepath.Join(dir, "key.pem"))
	signer := &fakeSigner{key: key}

	receipt, err := BuildSignedReceiptV2(signer, mustFingerprint(t, signer), goldenJobID, goldenEngine, goldenModel, inputs, grounding, mustTime(t, goldenIssuedAt))
	if err != nil {
		t.Fatalf("BuildSignedReceiptV2: %v", err)
	}

	// 1) Canonical bytes reproduce the committed golden exactly.
	gotCanon := CanonicalizeV2(receipt)
	wantCanon := readFixture(t, filepath.Join(dir, "canonical.bin"))
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("CanonicalizeV2 mismatch:\n got %q\nwant %q\n(run with -update-golden if the canon changed deliberately)", gotCanon, wantCanon)
	}

	// 2) Digest fields match input.bin / output.txt / policy.json.
	if got := string(readFixture(t, filepath.Join(dir, "input.bin"))); got != goldenInput {
		t.Errorf("input.bin = %q, want %q", got, goldenInput)
	}
	if got := string(readFixture(t, filepath.Join(dir, "output.txt"))); got != goldenContent {
		t.Errorf("output.txt = %q, want %q", got, goldenContent)
	}
	if receipt.InputSHA256 != sha256Prefixed(goldenInput) {
		t.Errorf("input_sha256 = %q, want %q", receipt.InputSHA256, sha256Prefixed(goldenInput))
	}
	if receipt.OutputSHA256 != sha256Prefixed(goldenContent) {
		t.Errorf("output_sha256 = %q, want %q", receipt.OutputSHA256, sha256Prefixed(goldenContent))
	}
	if receipt.PolicyHash != EmptyPolicyHash {
		t.Errorf("policy_hash = %q, want %q", receipt.PolicyHash, EmptyPolicyHash)
	}

	// 3) The committed receipt.json canonicalizes to the same bytes and its
	//    committed signature verifies against the committed leaf's key.
	var committed AEPReceiptV2
	if err := json.Unmarshal(readFixture(t, filepath.Join(dir, "receipt.json")), &committed); err != nil {
		t.Fatalf("unmarshal receipt.json: %v", err)
	}
	if committed.ReceiptVersion != ReceiptVersionV2 {
		t.Errorf("committed receipt_version = %q, want %q", committed.ReceiptVersion, ReceiptVersionV2)
	}
	committedCanon := CanonicalizeV2(&committed)
	if string(committedCanon) != string(wantCanon) {
		t.Errorf("committed receipt.json canonicalizes differently than canonical.bin:\n got %q\nwant %q", committedCanon, wantCanon)
	}

	leafKey := loadGoldenLeafPublicKey(t, filepath.Join(dir, "leaf.pem"))
	sigDER, err := base64.StdEncoding.DecodeString(committed.Signature)
	if err != nil {
		t.Fatalf("decode committed signature: %v", err)
	}
	digest := sha256.Sum256(committedCanon)
	if !ecdsa.VerifyASN1(leafKey, digest[:], sigDER) {
		t.Error("committed signature does not verify against the committed leaf's public key")
	}
	if wantFP := mustFingerprint(t, signer); committed.PublicKeyFingerprint != wantFP {
		t.Errorf("committed public_key_fingerprint = %q, want %q (the leaf's SPKI)", committed.PublicKeyFingerprint, wantFP)
	}

	// 4) The verdict preimage the node builds canonicalizes (via the trust
	//    canonical_json port) to the hash the committed receipt carries — the
	//    value a future aceteam-aep verifier reproduces with its own
	//    canonical_json over verdict_preimage.json.
	canonJSON, err := trust.CanonicalJSON(trust.VerdictHashPreimage(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash))
	if err != nil {
		t.Fatalf("CanonicalJSON(verdict preimage): %v", err)
	}
	sum := sha256.Sum256(canonJSON)
	if got, want := "sha256:"+hex.EncodeToString(sum[:]), committed.VerdictHash; got != want {
		t.Errorf("sha256(canonical_json(verdict preimage)) = %q, want the committed verdict_hash %q", got, want)
	}
	// The committed verdict_preimage.json must be the current preimage object,
	// so the file a future aceteam PR loads is never stale.
	wantPreimageJSON, err := json.MarshalIndent(trust.VerdictHashPreimage(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wantPreimageJSON = append(wantPreimageJSON, '\n')
	if got := readFixture(t, filepath.Join(dir, "verdict_preimage.json")); string(got) != string(wantPreimageJSON) {
		t.Errorf("verdict_preimage.json is stale; run -update-golden")
	}
}

// regenerateGoldenV2 writes every fixture in dir from a freshly generated (or
// existing) test key and self-signed leaf.
func regenerateGoldenV2(t *testing.T, dir string, grounding trust.GroundingResult, verdict trust.Verdict, inputs V2Inputs) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(dir, "key.pem")
	var key *ecdsa.PrivateKey
	if _, err := os.Stat(keyPath); err == nil {
		key = loadGoldenKey(t, keyPath)
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	}

	// Self-signed leaf: the verifier only extracts the SPKI, never chain-validates.
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "citadel-golden-node"},
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, leafTmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "leaf.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))

	signer := &fakeSigner{key: key}
	receipt, err := BuildSignedReceiptV2(signer, mustFingerprint(t, signer), goldenJobID, goldenEngine, goldenModel, inputs, grounding, mustTime(t, goldenIssuedAt))
	if err != nil {
		t.Fatal(err)
	}

	writeFixture(t, filepath.Join(dir, "input.bin"), []byte(goldenInput))
	writeFixture(t, filepath.Join(dir, "output.txt"), []byte(goldenContent))
	writeFixture(t, filepath.Join(dir, "policy.json"), []byte("{}"))
	writeFixture(t, filepath.Join(dir, "canonical.bin"), CanonicalizeV2(receipt))

	receiptJSON, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "receipt.json"), append(receiptJSON, '\n'))

	preimage := trust.VerdictHashPreimage(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash)
	preimageJSON, err := json.MarshalIndent(preimage, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "verdict_preimage.json"), append(preimageJSON, '\n'))

	t.Logf("regenerated golden fixtures in %s", dir)
}

// --- helpers ---

func mustFingerprint(t *testing.T, s Signer) string {
	t.Helper()
	fp, err := s.PublicKeyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v (regenerate with -update-golden)", path, err)
	}
	return b
}

func writeFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func loadGoldenKey(t *testing.T, path string) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode(readFixture(t, path))
	if block == nil {
		t.Fatalf("no PEM block in %s", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse EC key %s: %v", path, err)
	}
	return key
}

func loadGoldenLeafPublicKey(t *testing.T, path string) *ecdsa.PublicKey {
	t.Helper()
	block, _ := pem.Decode(readFixture(t, path))
	if block == nil {
		t.Fatalf("no PEM block in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf %s: %v", path, err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("leaf %s public key is %T, want *ecdsa.PublicKey", path, cert.PublicKey)
	}
	return pub
}
