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
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gowebpki/jcs"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// This file pins the v3 (RFC 8785 JCS) canon two ways:
//
//   - TestGoldenReceiptV3: a signed v3 receipt whose canonical bytes are frozen
//     in testdata/v3/canonical.bin, alongside receipt.json / key.pem / leaf.pem /
//     verdict_preimage.json -- the cross-repo contract a follow-up aceteam PR
//     asserts its Python rfc8785 verifier reproduces byte-for-byte.
//   - TestJCSConformanceCorpus: testdata/v3/corpus.json, a language-neutral set
//     of edge cases (the v1-colliding pair, JSON 1==1.0, UTF-16 key ordering,
//     escaping, null/empty, the number-notation boundaries). Four cases carry a
//     sha256_16 lifted from docs/design-canon-framework.md §9.2 -- an INDEPENDENT
//     three-library (Go + two Python) run, so matching it is real cross-language
//     evidence, not Go self-consistency.
//
// Both reuse the -update-golden flag declared in receipt_v2_golden_test.go.
// Regenerate ONLY v3 with:
//
//	go test ./internal/aep -run 'TestGoldenReceiptV3|TestJCSConformanceCorpus' -update-golden
//
// Do NOT use -run Golden (it also matches the v2 golden and would rewrite v2's
// randomized signature/cert -- a spurious v2 diff).

func v3TestdataDir() string { return filepath.Join("testdata", "v3") }

// goldenV3Inputs reuses the shared golden sample (goldenInput/goldenContent/...
// from receipt_v2_golden_test.go) but computes verdict_hash via the JCS path
// (VerdictHashV3), the v3-specific value.
func goldenV3Inputs(t *testing.T, verdict trust.Verdict, grounding trust.GroundingResult) V3Inputs {
	t.Helper()
	vh, err := VerdictHashV3(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash)
	if err != nil {
		t.Fatalf("VerdictHashV3: %v", err)
	}
	return V3Inputs{
		InputSHA256:  sha256Prefixed(goldenInput),
		OutputSHA256: sha256Prefixed(goldenContent),
		PolicyHash:   EmptyPolicyHash,
		Action:       verdict.Action,
		VerdictHash:  vh,
	}
}

// TestGoldenReceiptV3 mirrors TestGoldenReceiptV2 but for the JCS canon: it
// rebuilds the fixed sample from the committed key + fixed issued_at, asserts
// CanonicalizeV3 reproduces canonical.bin byte-for-byte, that the digest fields
// match, and that the committed (randomized) signature verifies against the
// committed leaf. With -update-golden it regenerates the directory instead.
func TestGoldenReceiptV3(t *testing.T) {
	dir := v3TestdataDir()

	grounding := goldenGrounding()
	verdict := trust.BuildVerdict(goldenInput, goldenContent, grounding, trust.DefaultDetectors(), EmptyPolicyHash)
	inputs := goldenV3Inputs(t, verdict, grounding)

	if *updateGolden {
		regenerateGoldenV3(t, dir, grounding, verdict, inputs)
		return
	}

	key := loadGoldenKey(t, filepath.Join(dir, "key.pem"))
	signer := &fakeSigner{key: key}

	receipt, err := BuildSignedReceiptV3(signer, mustFingerprint(t, signer), goldenJobID, goldenEngine, goldenModel, inputs, grounding, mustTime(t, goldenIssuedAt))
	if err != nil {
		t.Fatalf("BuildSignedReceiptV3: %v", err)
	}

	// 1) Canonical bytes reproduce the committed golden exactly.
	gotCanon, err := CanonicalizeV3(receipt)
	if err != nil {
		t.Fatalf("CanonicalizeV3: %v", err)
	}
	wantCanon := readFixture(t, filepath.Join(dir, "canonical.bin"))
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("CanonicalizeV3 mismatch:\n got %q\nwant %q\n(run -update-golden if the canon changed deliberately)", gotCanon, wantCanon)
	}

	// 2) Digest fields match the committed source bytes.
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
	var committed AEPReceiptV3
	if err := json.Unmarshal(readFixture(t, filepath.Join(dir, "receipt.json")), &committed); err != nil {
		t.Fatalf("unmarshal receipt.json: %v", err)
	}
	if committed.ReceiptVersion != ReceiptVersionV3 {
		t.Errorf("committed receipt_version = %q, want %q", committed.ReceiptVersion, ReceiptVersionV3)
	}
	committedCanon, err := CanonicalizeV3(&committed)
	if err != nil {
		t.Fatalf("CanonicalizeV3(committed): %v", err)
	}
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

	// 4) The verdict preimage the node builds canonicalizes (via the SHARED JCS
	//    canonicalizer) to the verdict_hash the committed receipt carries -- the
	//    value a future aceteam-aep verifier reproduces with its own rfc8785 over
	//    verdict_preimage.json.
	vhCanon, err := CanonicalizeJCS(trust.VerdictHashPreimage(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash))
	if err != nil {
		t.Fatalf("CanonicalizeJCS(verdict preimage): %v", err)
	}
	sum := sha256.Sum256(vhCanon)
	if got, want := "sha256:"+hex.EncodeToString(sum[:]), committed.VerdictHash; got != want {
		t.Errorf("sha256(JCS(verdict preimage)) = %q, want committed verdict_hash %q", got, want)
	}
	// The committed verdict_preimage.json must be the current preimage object AND
	// its own JCS form (committed as verdict_preimage.jcs) so the file a future
	// aceteam PR loads is never stale.
	wantPreimageJSON, err := json.MarshalIndent(trust.VerdictHashPreimage(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wantPreimageJSON = append(wantPreimageJSON, '\n')
	if got := readFixture(t, filepath.Join(dir, "verdict_preimage.json")); string(got) != string(wantPreimageJSON) {
		t.Errorf("verdict_preimage.json is stale; run -update-golden")
	}
	if got := readFixture(t, filepath.Join(dir, "verdict_preimage.jcs")); string(got) != string(vhCanon) {
		t.Errorf("verdict_preimage.jcs is stale; run -update-golden")
	}
}

func regenerateGoldenV3(t *testing.T, dir string, grounding trust.GroundingResult, verdict trust.Verdict, inputs V3Inputs) {
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

	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "citadel-golden-node-v3"},
		NotBefore:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, leafTmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "leaf.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))

	signer := &fakeSigner{key: key}
	receipt, err := BuildSignedReceiptV3(signer, mustFingerprint(t, signer), goldenJobID, goldenEngine, goldenModel, inputs, grounding, mustTime(t, goldenIssuedAt))
	if err != nil {
		t.Fatal(err)
	}

	canon, err := CanonicalizeV3(receipt)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "input.bin"), []byte(goldenInput))
	writeFixture(t, filepath.Join(dir, "output.txt"), []byte(goldenContent))
	writeFixture(t, filepath.Join(dir, "policy.json"), []byte("{}"))
	writeFixture(t, filepath.Join(dir, "canonical.bin"), canon)

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
	vhCanon, err := CanonicalizeJCS(preimage)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "verdict_preimage.jcs"), vhCanon)

	t.Logf("regenerated v3 golden fixtures in %s", dir)
}

// --- conformance corpus ---
//

type corpusCase struct {
	Name     string          `json:"name"`
	Note     string          `json:"note,omitempty"`
	Input    json.RawMessage `json:"input"`
	JCS      string          `json:"jcs"`
	SHA256   string          `json:"sha256"`
	SHA256At string          `json:"sha256_16,omitempty"` // independent reference (design §9.2): sha256(jcs)[:16] hex
}

type corpusFile struct {
	Description string       `json:"description"`
	Cases       []corpusCase `json:"cases"`
}

func corpusPath() string { return filepath.Join(v3TestdataDir(), "corpus.json") }

// TestJCSConformanceCorpus verifies the shared JCS canonicalizer (jcs.Transform,
// the same one CanonicalizeJCS wraps) against testdata/v3/corpus.json. Under
// -update-golden it recomputes the jcs+sha256 fields from the authored
// input/note/sha256_16 (asserting any committed sha256_16 still matches, so a
// regen can never silently paper over a cross-language divergence).
func TestJCSConformanceCorpus(t *testing.T) {
	var cf corpusFile
	if err := json.Unmarshal(readFixture(t, corpusPath()), &cf); err != nil {
		t.Fatalf("parse corpus.json: %v", err)
	}

	if *updateGolden {
		for i := range cf.Cases {
			out, err := jcs.Transform([]byte(cf.Cases[i].Input))
			if err != nil {
				t.Fatalf("case %q: jcs.Transform: %v", cf.Cases[i].Name, err)
			}
			sum := sha256.Sum256(out)
			full := hex.EncodeToString(sum[:])
			if ref := cf.Cases[i].SHA256At; ref != "" && full[:16] != ref {
				t.Fatalf("case %q: computed sha256[:16]=%s but committed reference sha256_16=%s (design §9.2 divergence -- do NOT overwrite)", cf.Cases[i].Name, full[:16], ref)
			}
			cf.Cases[i].JCS = string(out)
			cf.Cases[i].SHA256 = full
		}
		data, err := json.MarshalIndent(cf, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, corpusPath(), append(data, '\n'))
		t.Logf("regenerated %s (%d cases)", corpusPath(), len(cf.Cases))
		return
	}

	byName := make(map[string]string, len(cf.Cases))
	for _, c := range cf.Cases {
		out, err := jcs.Transform([]byte(c.Input))
		if err != nil {
			t.Errorf("case %q: jcs.Transform: %v", c.Name, err)
			continue
		}
		if c.JCS != "" && string(out) != c.JCS {
			t.Errorf("case %q: jcs mismatch:\n got %q\nwant %q", c.Name, out, c.JCS)
		}
		sum := sha256.Sum256(out)
		full := hex.EncodeToString(sum[:])
		if c.SHA256 != "" && full != c.SHA256 {
			t.Errorf("case %q: sha256 = %s, want %s", c.Name, full, c.SHA256)
		}
		if c.SHA256At != "" && full[:16] != c.SHA256At {
			t.Errorf("case %q: sha256[:16] = %s, want reference %s (design §9.2, cross-language)", c.Name, full[:16], c.SHA256At)
		}
		byName[c.Name] = string(out)
	}

	// The headline injectivity property: the two v1-colliding receipts produce
	// DISTINCT JCS bytes.
	a, okA := byName["collision_pair_a"]
	b, okB := byName["collision_pair_b"]
	if !okA || !okB {
		t.Fatal("corpus must contain collision_pair_a and collision_pair_b")
	}
	if a == b {
		t.Error("collision_pair_a and collision_pair_b canonicalized identically; the v3 canon must be injective")
	}

	// The 1 == 1.0 transport-robustness property.
	one, ok1 := byName["number_int_one"]
	oneF, ok1f := byName["number_float_one"]
	if !ok1 || !ok1f {
		t.Fatal("corpus must contain number_int_one and number_float_one")
	}
	if one != oneF {
		t.Errorf("JSON 1 and 1.0 must canonicalize identically under JCS: %q vs %q", one, oneF)
	}
}
