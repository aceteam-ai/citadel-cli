package cmd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/trust"
	"github.com/spf13/cobra"
)

// aepTestSigner is an in-memory ECDSA signer for these tests. Its
// PublicKeyFingerprint uses nodeidentity.FingerprintPublicKey — the SAME
// derivation the CLI recomputes from the resolved verifying key — so a receipt
// it signs carries a fingerprint the verifier will match.
type aepTestSigner struct{ key *ecdsa.PrivateKey }

func newAEPTestSigner(t *testing.T) *aepTestSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &aepTestSigner{key: key}
}

func (s *aepTestSigner) Sign(payload []byte) ([]byte, error) {
	d := sha256.Sum256(payload)
	return ecdsa.SignASN1(rand.Reader, s.key, d[:])
}

func (s *aepTestSigner) PublicKeyFingerprint() (string, error) {
	return nodeidentity.FingerprintPublicKey(&s.key.PublicKey)
}

// mustNodeKeyFn returns a nodeKeyFn stub that fails the test if the default
// node-identity path is ever reached. This box may be a live node with real
// /etc/citadel state, so a test that forgot --pubkey/--cert must fail loudly
// rather than read live identity.
func mustNotReachNodeIdentity(t *testing.T) func() (*ecdsa.PublicKey, error) {
	t.Helper()
	return func() (*ecdsa.PublicKey, error) {
		t.Fatalf("test reached real node identity; a verifying key should have been supplied")
		return nil, nil
	}
}

func writePubKeyPEM(t *testing.T, dir string, pub *ecdsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}
	p := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write pubkey: %v", err)
	}
	return p
}

func writeReceiptJSON(t *testing.T, dir, name string, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	return p
}

func signedV2ReceiptMap(t *testing.T, signer aep.Signer) map[string]any {
	t.Helper()
	nodeID, err := aep.ResolveNodeID(signer, "")
	if err != nil {
		t.Fatalf("resolve node id: %v", err)
	}
	result := trust.GroundingResult{
		Grounded:      false,
		Score:         0.5,
		ClaimsChecked: 2,
		Flagged:       []trust.Claim{{Value: "68%", Kind: trust.ClaimPercent, Reason: "no support in input"}},
	}
	in := aep.V2Inputs{
		InputSHA256:  "sha256:" + strings.Repeat("a", 64),
		OutputSHA256: "sha256:" + strings.Repeat("b", 64),
		PolicyHash:   aep.EmptyPolicyHash,
		Action:       "flag",
		VerdictHash:  "sha256:" + strings.Repeat("c", 64),
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	r, err := aep.BuildSignedReceiptV2(signer, nodeID, "job-cli-test", "bonsai", "bonsai-27b", in, result, now)
	if err != nil {
		t.Fatalf("BuildSignedReceiptV2: %v", err)
	}
	m, err := r.ToMap()
	if err != nil {
		t.Fatalf("ToMap: %v", err)
	}
	return m
}

func TestVerifyAEPReceipt_V2RoundTrip(t *testing.T) {
	dir := t.TempDir()
	signer := newAEPTestSigner(t)
	receiptPath := writeReceiptJSON(t, dir, "receipt.json", signedV2ReceiptMap(t, signer))
	pubPath := writePubKeyPEM(t, dir, &signer.key.PublicKey)

	out := verifyAEPReceipt(verifyOptions{
		receiptPath: receiptPath,
		pubkeyPath:  pubPath,
		nodeKeyFn:   mustNotReachNodeIdentity(t),
	})

	if !out.Valid {
		t.Fatalf("expected valid, got reason %q", out.Reason)
	}
	if out.ReceiptVersion != "2" {
		t.Errorf("ReceiptVersion = %q, want 2", out.ReceiptVersion)
	}
	if out.summary == nil || out.summary.action != "flag" || out.summary.engine != "bonsai" {
		t.Errorf("summary = %+v, want action=flag engine=bonsai", out.summary)
	}
	wantFP, _ := signer.PublicKeyFingerprint()
	if out.PublicKeyFingerprint != wantFP {
		t.Errorf("fingerprint = %q, want %q", out.PublicKeyFingerprint, wantFP)
	}
}

func TestVerifyAEPReceipt_TamperedScoreFails(t *testing.T) {
	dir := t.TempDir()
	signer := newAEPTestSigner(t)
	m := signedV2ReceiptMap(t, signer)
	m["score"] = 0.9 // tamper a signed field; signature no longer covers this
	receiptPath := writeReceiptJSON(t, dir, "receipt.json", m)
	pubPath := writePubKeyPEM(t, dir, &signer.key.PublicKey)

	out := verifyAEPReceipt(verifyOptions{receiptPath: receiptPath, pubkeyPath: pubPath, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if out.Valid {
		t.Fatalf("tampered receipt verified, want failure")
	}
	if !strings.Contains(out.Reason, "signature is invalid") {
		t.Errorf("reason = %q, want signature-invalid", out.Reason)
	}
}

func TestVerifyAEPReceipt_TamperedSignatureFails(t *testing.T) {
	dir := t.TempDir()
	signer := newAEPTestSigner(t)
	m := signedV2ReceiptMap(t, signer)
	// Flip a byte of the DER signature but keep it valid base64.
	sigDER, err := base64.StdEncoding.DecodeString(m["signature"].(string))
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	sigDER[len(sigDER)-1] ^= 0xFF
	m["signature"] = base64.StdEncoding.EncodeToString(sigDER)
	receiptPath := writeReceiptJSON(t, dir, "receipt.json", m)
	pubPath := writePubKeyPEM(t, dir, &signer.key.PublicKey)

	out := verifyAEPReceipt(verifyOptions{receiptPath: receiptPath, pubkeyPath: pubPath, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if out.Valid {
		t.Fatalf("tampered signature verified, want failure")
	}
	if !strings.Contains(out.Reason, "signature is invalid") {
		t.Errorf("reason = %q, want signature-invalid", out.Reason)
	}
}

func TestVerifyAEPReceipt_FingerprintMismatchIsDistinct(t *testing.T) {
	dir := t.TempDir()
	signerA := newAEPTestSigner(t)
	signerB := newAEPTestSigner(t)
	receiptPath := writeReceiptJSON(t, dir, "receipt.json", signedV2ReceiptMap(t, signerA))
	// Verify against a DIFFERENT node's key.
	pubPath := writePubKeyPEM(t, dir, &signerB.key.PublicKey)

	out := verifyAEPReceipt(verifyOptions{receiptPath: receiptPath, pubkeyPath: pubPath, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if out.Valid {
		t.Fatalf("mismatched key verified, want failure")
	}
	if !strings.Contains(out.Reason, "signed by a different node") {
		t.Errorf("reason = %q, want different-node message", out.Reason)
	}
	if strings.Contains(out.Reason, "signature is invalid") {
		t.Errorf("mismatch must be distinct from a generic invalid-signature; got %q", out.Reason)
	}
}

func TestVerifyAEPReceipt_V1RoundTrip(t *testing.T) {
	dir := t.TempDir()
	signer := newAEPTestSigner(t)
	nodeID, _ := aep.ResolveNodeID(signer, "")
	result := trust.GroundingResult{Grounded: true, Score: 1.0, ClaimsChecked: 0}
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	r, err := aep.BuildSignedReceipt(signer, nodeID, "job-v1", "vllm", "m", result, now)
	if err != nil {
		t.Fatalf("BuildSignedReceipt: %v", err)
	}
	m, err := r.ToMap()
	if err != nil {
		t.Fatalf("ToMap: %v", err)
	}
	receiptPath := writeReceiptJSON(t, dir, "receipt.json", m)
	pubPath := writePubKeyPEM(t, dir, &signer.key.PublicKey)

	out := verifyAEPReceipt(verifyOptions{receiptPath: receiptPath, pubkeyPath: pubPath, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if !out.Valid {
		t.Fatalf("v1 receipt failed to verify: %q", out.Reason)
	}
	if out.ReceiptVersion != "1" {
		t.Errorf("ReceiptVersion = %q, want 1", out.ReceiptVersion)
	}
}

func TestVerifyAEPReceipt_UnsignedIsDistinct(t *testing.T) {
	dir := t.TempDir()
	signer := newAEPTestSigner(t)
	m := signedV2ReceiptMap(t, signer)
	delete(m, "signature")
	delete(m, "public_key_fingerprint")
	receiptPath := writeReceiptJSON(t, dir, "receipt.json", m)
	pubPath := writePubKeyPEM(t, dir, &signer.key.PublicKey)

	out := verifyAEPReceipt(verifyOptions{receiptPath: receiptPath, pubkeyPath: pubPath, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if out.Valid {
		t.Fatalf("unsigned receipt verified, want failure")
	}
	if !strings.Contains(out.Reason, "unsigned") {
		t.Errorf("reason = %q, want unsigned", out.Reason)
	}
}

func TestVerifyAEPReceipt_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "receipt.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := verifyAEPReceipt(verifyOptions{receiptPath: p, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if out.Valid || !strings.Contains(out.Reason, "malformed") {
		t.Errorf("out = %+v, want malformed failure", out)
	}
}

func TestVerifyAEPReceipt_UnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	p := writeReceiptJSON(t, dir, "receipt.json", map[string]any{"receipt_version": "99"})
	out := verifyAEPReceipt(verifyOptions{receiptPath: p, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if out.Valid || !strings.Contains(out.Reason, "unsupported receipt_version") {
		t.Errorf("out = %+v, want unsupported-version failure", out)
	}
}

// TestVerifyAEPReceipt_GoldenCertPath is the only test that independently
// checks (a) x509 cert -> ECDSA pubkey parsing and (b) that
// FingerprintPublicKey(leafPub) equals the fingerprint the real build path
// wrote into the committed golden receipt — not a fingerprint this test also
// computed. cwd for a package test is the package dir, so the sibling package's
// testdata is reachable via a relative path.
func TestVerifyAEPReceipt_GoldenCertPath(t *testing.T) {
	receiptPath := filepath.Join("..", "internal", "aep", "testdata", "v2", "receipt.json")
	certPath := filepath.Join("..", "internal", "aep", "testdata", "v2", "leaf.pem")
	// The fixture is committed in-repo, so a missing path is a broken checkout /
	// moved fixture, not a reason to skip — fail loudly rather than let this
	// silently pass without exercising cert parsing + independent fingerprint.
	if _, err := os.Stat(receiptPath); err != nil {
		t.Fatalf("golden fixture not present at %s: %v", receiptPath, err)
	}

	out := verifyAEPReceipt(verifyOptions{receiptPath: receiptPath, certPath: certPath, nodeKeyFn: mustNotReachNodeIdentity(t)})
	if !out.Valid {
		t.Fatalf("golden receipt failed to verify against committed leaf: %q", out.Reason)
	}
	if out.PublicKeyFingerprint != "sha256:bff23802e4de79935010493046546f766b69fd76442936955d7f23744b98fe45" {
		t.Errorf("fingerprint = %q, want the golden fingerprint", out.PublicKeyFingerprint)
	}
	if out.ReceiptVersion != "2" {
		t.Errorf("ReceiptVersion = %q, want 2", out.ReceiptVersion)
	}
}

// TestRunAEPVerify_JSONAndExitCode exercises the RunE rendering + exit-code
// contract directly (not through rootCmd, whose PersistentPreRun has side
// effects). It uses a throwaway command carrying capture buffers.
func TestRunAEPVerify_JSONAndExitCode(t *testing.T) {
	dir := t.TempDir()
	signer := newAEPTestSigner(t)
	receiptPath := writeReceiptJSON(t, dir, "receipt.json", signedV2ReceiptMap(t, signer))
	pubPath := writePubKeyPEM(t, dir, &signer.key.PublicKey)

	// Save/restore package-level flag vars mutated here.
	defer func(j bool, pk, ct string, sc bool) {
		aepVerifyJSON, aepVerifyPubKeyPath, aepVerifyCertPath, aepVerifyShowCanon = j, pk, ct, sc
	}(aepVerifyJSON, aepVerifyPubKeyPath, aepVerifyCertPath, aepVerifyShowCanon)

	// Success + JSON.
	aepVerifyJSON = true
	aepVerifyPubKeyPath = pubPath
	aepVerifyCertPath = ""
	aepVerifyShowCanon = false

	var outBuf, errBuf bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&outBuf)
	c.SetErr(&errBuf)
	if err := runAEPVerify(c, []string{receiptPath}); err != nil {
		t.Fatalf("runAEPVerify (valid) returned err: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(outBuf.Bytes(), &body); err != nil {
		t.Fatalf("json output not parseable: %v\n%s", err, outBuf.String())
	}
	if body["valid"] != true {
		t.Errorf("json valid = %v, want true", body["valid"])
	}

	// Failure (wrong key) must return the sentinel error (non-zero exit) and
	// still emit a JSON body.
	other := newAEPTestSigner(t)
	aepVerifyPubKeyPath = writePubKeyPEM(t, dir, &other.key.PublicKey)
	outBuf.Reset()
	errBuf.Reset()
	c2 := &cobra.Command{}
	c2.SetOut(&outBuf)
	c2.SetErr(&errBuf)
	err := runAEPVerify(c2, []string{receiptPath})
	if err == nil {
		t.Fatalf("runAEPVerify (invalid) returned nil err, want sentinel for non-zero exit")
	}
	body = nil
	if uerr := json.Unmarshal(outBuf.Bytes(), &body); uerr != nil {
		t.Fatalf("failure json not parseable: %v\n%s", uerr, outBuf.String())
	}
	if body["valid"] != false {
		t.Errorf("json valid = %v, want false", body["valid"])
	}
}

// TestAEPCommandWiring pins that `aep verify` is registered under a parent
// `aep` command with the documented flags.
func TestAEPCommandWiring(t *testing.T) {
	var aepCommand *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "aep" {
			aepCommand = c
			break
		}
	}
	if aepCommand == nil {
		t.Fatalf("aep command not registered under root")
	}
	var verify *cobra.Command
	for _, c := range aepCommand.Commands() {
		if c.Name() == "verify" {
			verify = c
			break
		}
	}
	if verify == nil {
		t.Fatalf("verify subcommand not registered under aep")
	}
	for _, f := range []string{"json", "pubkey", "cert", "show-canonical"} {
		if verify.Flags().Lookup(f) == nil {
			t.Errorf("verify missing --%s flag", f)
		}
	}
}
