// cmd/aep.go
//
// `citadel aep verify <receipt.json>` verifies the ECDSA signature of a signed
// AEP ("AceTeam Execution Proof") receipt — the signed grounding/trust receipt
// internal/aep produces and internal/worker attaches to a chat-completion job's
// output when CITADEL_SIGN_AEP_RECEIPTS is on (see the "Node identity
// persistence + signed AEP receipt" section of CLAUDE.md and
// docs/design-node-identity-receipts.md).
//
// The verify math is exactly the round-trip internal/aep's own tests pin:
//
//	sigDER = base64.StdEncoding.DecodeString(receipt.signature)
//	digest = sha256(CanonicalizeV2(&r))   // or Canonicalize(&r) for v1
//	ecdsa.VerifyASN1(pub, digest[:], sigDER)
//
// signature and public_key_fingerprint are excluded from the canonical bytes by
// construction, so unmarshalling the receipt JSON back into the struct and
// re-canonicalizing reproduces the exact bytes that were signed.
//
// The verifying public key comes from (in precedence order) --cert, --pubkey, or
// the local node identity (nodeidentity.Convergent — the SAME key the signer
// used). The command recomputes the key's fingerprint and compares it to the
// receipt's public_key_fingerprint BEFORE checking the signature, so "signed by
// a different node" is reported distinctly from "signature is invalid".
package cmd

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/spf13/cobra"
)

var (
	aepVerifyJSON       bool
	aepVerifyPubKeyPath string
	aepVerifyCertPath   string
	aepVerifyShowCanon  bool
)

// errAEPVerifyFailed is a silent sentinel: returned from RunE purely to make
// Execute() exit non-zero (SilenceErrors is set, so cobra prints nothing extra —
// the command has already printed its own human/JSON result).
var errAEPVerifyFailed = errors.New("aep receipt verification failed")

var aepCmd = &cobra.Command{
	Use:   "aep",
	Short: "AceTeam Execution Proof receipt tools",
	Long: "Tools for working with signed AEP (AceTeam Execution Proof) receipts.\n\n" +
		"An AEP receipt is the signed grounding/trust proof a Citadel node attaches to\n" +
		"an inference job's output. `citadel aep verify` checks its ECDSA signature\n" +
		"offline against a node's public identity.",
}

var aepVerifyCmd = &cobra.Command{
	Use:   "verify <receipt.json>",
	Short: "Verify the ECDSA signature of a signed AEP receipt",
	Long: "Verify the ECDSA signature of a signed AEP receipt (v1 or v2).\n\n" +
		"The verifying public key is resolved in this order:\n" +
		"  --cert <leaf.pem>    take the public key from an X.509 certificate\n" +
		"  --pubkey <spki.pem>  a PKIX/SPKI PEM public key\n" +
		"  (default)            this node's local identity key\n\n" +
		"The key's fingerprint is compared to the receipt's public_key_fingerprint\n" +
		"before the signature is checked, so a receipt signed by a different node is\n" +
		"reported distinctly from a genuinely invalid signature.",
	Args: cobra.ExactArgs(1),
	// NOTE: SilenceUsage/SilenceErrors are deliberately NOT set on the literal.
	// Cobra checks them for EVERY error on the subcommand path, including the
	// arg/flag validation that runs BEFORE RunE — setting them here would make
	// `citadel aep verify` (missing arg) or an unknown flag exit non-zero with no
	// message at all. runAEPVerify sets them itself (cobra reads them after RunE
	// returns), so only its own already-reported verification failure is
	// silenced, while genuine usage errors still print normally.
	RunE: runAEPVerify,
}

func init() {
	aepVerifyCmd.Flags().BoolVar(&aepVerifyJSON, "json", false, "Emit a machine-readable JSON result")
	aepVerifyCmd.Flags().StringVar(&aepVerifyPubKeyPath, "pubkey", "", "Path to a PKIX/SPKI PEM public key to verify against")
	aepVerifyCmd.Flags().StringVar(&aepVerifyCertPath, "cert", "", "Path to an X.509 certificate (PEM) whose public key verifies the receipt")
	aepVerifyCmd.Flags().BoolVar(&aepVerifyShowCanon, "show-canonical", false, "Print the exact canonical bytes that were signed to stderr")

	aepCmd.AddCommand(aepVerifyCmd)
	rootCmd.AddCommand(aepCmd)
}

// verifyOptions are the inputs to the pure verification core. nodeKeyFn, when
// non-nil, overrides the default node-identity resolution — set by tests to a
// stub so the real (machine-convergent) node identity is never read.
type verifyOptions struct {
	receiptPath string
	pubkeyPath  string
	certPath    string
	nodeKeyFn   func() (*ecdsa.PublicKey, error)
}

// receiptSummary carries the human-facing fields extracted from the receipt,
// independent of whether the signature verified.
type receiptSummary struct {
	engine        string
	model         string
	grounded      bool
	score         float64
	claimsChecked int
	action        string // v2 only
	verdictHash   string // v2 only
}

// verifyOutcome is the result of a verification attempt. Every expected failure
// (malformed receipt, unsigned, key mismatch, bad signature, missing key) is
// folded into Valid=false + Reason rather than a Go error, so JSON mode can
// always emit a complete body. The JSON-serialized fields are exactly the four
// the issue specifies; the rest are human-summary state.
type verifyOutcome struct {
	Valid                bool   `json:"valid"`
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
	ReceiptVersion       string `json:"receipt_version,omitempty"`
	Reason               string `json:"reason,omitempty"`

	canonical []byte          `json:"-"`
	summary   *receiptSummary `json:"-"`
	keySource string          `json:"-"` // human label for where the verifying key came from
}

func runAEPVerify(cmd *cobra.Command, args []string) error {
	// Silence cobra's own error/usage printing for whatever THIS function
	// returns — cobra reads these after RunE returns, so arg/flag validation
	// (which ran before we got here) is unaffected and still prints normally.
	cmd.SilenceErrors, cmd.SilenceUsage = true, true

	opts := verifyOptions{
		receiptPath: args[0],
		pubkeyPath:  aepVerifyPubKeyPath,
		certPath:    aepVerifyCertPath,
	}
	outcome := verifyAEPReceipt(opts)

	if aepVerifyShowCanon && len(outcome.canonical) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "--- canonical bytes (%d) ---\n%s\n--- end canonical ---\n", len(outcome.canonical), string(outcome.canonical))
	}

	if aepVerifyJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(outcome); err != nil {
			return err
		}
		if !outcome.Valid {
			return errAEPVerifyFailed
		}
		return nil
	}

	renderVerifyHuman(cmd, outcome)
	if !outcome.Valid {
		return errAEPVerifyFailed
	}
	return nil
}

func renderVerifyHuman(cmd *cobra.Command, o verifyOutcome) {
	out := cmd.OutOrStdout()
	if !o.Valid {
		fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s\n", o.Reason)
		return
	}
	fmt.Fprintln(out, "✓ signature valid")
	fmt.Fprintf(out, "  signed by node   %s\n", o.PublicKeyFingerprint)
	if o.keySource != "" {
		fmt.Fprintf(out, "  verifying key    %s\n", o.keySource)
	}
	fmt.Fprintf(out, "  receipt version  %s\n", o.ReceiptVersion)
	if o.summary != nil {
		s := o.summary
		fmt.Fprintf(out, "  engine/model     %s / %s\n", s.engine, s.model)
		fmt.Fprintf(out, "  grounded         %t (score %s, %d claims checked)\n",
			s.grounded, formatScore(s.score), s.claimsChecked)
		if s.action != "" {
			fmt.Fprintf(out, "  action           %s\n", s.action)
		}
		if s.verdictHash != "" {
			fmt.Fprintf(out, "  verdict_hash     %s\n", s.verdictHash)
		}
	}
}

// formatScore renders the score with the SAME fixed 6-decimal precision the
// canonical form uses, so the human display matches the signed bytes.
func formatScore(f float64) string { return fmt.Sprintf("%.6f", f) }

// verifyAEPReceipt is the pure verification core. It never returns a Go error;
// all outcomes are expressed on verifyOutcome so callers (human + JSON) share
// one code path.
func verifyAEPReceipt(opts verifyOptions) verifyOutcome {
	data, err := os.ReadFile(opts.receiptPath)
	if err != nil {
		return verifyOutcome{Valid: false, Reason: fmt.Sprintf("read receipt: %v", err)}
	}

	// Detect version off just receipt_version, then unmarshal into the right
	// struct and compute the canonical bytes that were signed.
	var probe struct {
		ReceiptVersion string `json:"receipt_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return verifyOutcome{Valid: false, Reason: fmt.Sprintf("malformed receipt: not valid JSON: %v", err)}
	}

	var (
		canonical    []byte
		signatureB64 string
		fingerprint  string
		version      string
		summary      *receiptSummary
	)

	switch probe.ReceiptVersion {
	case "2":
		var r aep.AEPReceiptV2
		if err := json.Unmarshal(data, &r); err != nil {
			return verifyOutcome{Valid: false, Reason: fmt.Sprintf("malformed v2 receipt: %v", err)}
		}
		canonical = aep.CanonicalizeV2(&r)
		signatureB64 = r.Signature
		fingerprint = r.PublicKeyFingerprint
		version = aep.ReceiptVersionV2
		summary = &receiptSummary{
			engine: r.Engine, model: r.Model, grounded: r.Grounded, score: r.Score,
			claimsChecked: r.ClaimsChecked, action: r.Action, verdictHash: r.VerdictHash,
		}
	case "", "1":
		var r aep.AEPReceiptV1
		if err := json.Unmarshal(data, &r); err != nil {
			return verifyOutcome{Valid: false, Reason: fmt.Sprintf("malformed v1 receipt: %v", err)}
		}
		canonical = aep.Canonicalize(&r)
		signatureB64 = r.Signature
		fingerprint = r.PublicKeyFingerprint
		version = "1"
		summary = &receiptSummary{
			engine: r.Engine, model: r.Model, grounded: r.Grounded, score: r.Score,
			claimsChecked: r.ClaimsChecked,
		}
	default:
		return verifyOutcome{Valid: false, Reason: fmt.Sprintf("unsupported receipt_version %q (expected \"1\" or \"2\")", probe.ReceiptVersion)}
	}

	base := verifyOutcome{
		ReceiptVersion:       version,
		PublicKeyFingerprint: fingerprint,
		canonical:            canonical,
		summary:              summary,
	}

	// Unsigned receipt: distinct, honest reason (never a mismatch message with an
	// empty fingerprint), checked before we even resolve a verifying key.
	if signatureB64 == "" || fingerprint == "" {
		base.Reason = "receipt is unsigned (no signature or public_key_fingerprint)"
		return base
	}

	pub, keySource, err := resolveVerifyingKey(opts)
	if err != nil {
		base.Reason = fmt.Sprintf("resolve verifying key: %v", err)
		return base
	}
	base.keySource = keySource

	verifyingFP, err := nodeidentity.FingerprintPublicKey(pub)
	if err != nil {
		base.Reason = fmt.Sprintf("fingerprint verifying key: %v", err)
		return base
	}

	// Fingerprint compare BEFORE signature check: a key that doesn't match the
	// receipt's claimed signer is a DIFFERENT-NODE error, distinct from an
	// invalid signature.
	if verifyingFP != fingerprint {
		base.Reason = fmt.Sprintf("receipt was signed by a different node (receipt fingerprint %s, verifying key %s)", fingerprint, verifyingFP)
		return base
	}

	sigDER, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		base.Reason = fmt.Sprintf("signature is not valid base64: %v", err)
		return base
	}

	digest := sha256.Sum256(canonical)
	if !ecdsa.VerifyASN1(pub, digest[:], sigDER) {
		base.Reason = "signature is invalid (does not verify against the signing key)"
		return base
	}

	base.Valid = true
	return base
}

// resolveVerifyingKey returns the ECDSA public key to verify against, plus a
// human-readable source label, following the --cert > --pubkey > node-identity
// precedence.
func resolveVerifyingKey(opts verifyOptions) (*ecdsa.PublicKey, string, error) {
	switch {
	case opts.certPath != "":
		pub, err := publicKeyFromCertFile(opts.certPath)
		return pub, "cert:" + opts.certPath, err
	case opts.pubkeyPath != "":
		pub, err := publicKeyFromPEMFile(opts.pubkeyPath)
		return pub, "pubkey:" + opts.pubkeyPath, err
	default:
		if opts.nodeKeyFn != nil {
			pub, err := opts.nodeKeyFn()
			return pub, "node identity", err
		}
		store := nodeidentity.Convergent(network.GetNodeConfigDir())
		pub, err := store.PublicKey()
		return pub, "node identity", err
	}
}

// publicKeyFromCertFile parses an X.509 certificate PEM and returns its ECDSA
// public key.
func publicKeyFromCertFile(path string) (*ecdsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s is not valid PEM", path)
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("expected a CERTIFICATE PEM block, got %q", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("certificate public key is %T, not ECDSA", cert.PublicKey)
	}
	return pub, nil
}

// publicKeyFromPEMFile parses a PKIX/SPKI PEM public key and returns its ECDSA
// public key.
func publicKeyFromPEMFile(path string) (*ecdsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s is not valid PEM", path)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is %T, not ECDSA", parsed)
	}
	return pub, nil
}
