package aep

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestBuildSignedRunJobTemplateReceipt_BindsRun(t *testing.T) {
	signer := newFakeSigner(t)
	receipt, err := BuildSignedRunJobTemplateReceipt(signer, "n1", "job-1", "sha256:manifest", "sha256:outputs", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Action != "run_job_template" || receipt.NodeID != "n1" || receipt.JobID != "job-1" || receipt.InputSHA256 != "sha256:manifest" || receipt.OutputSHA256 != "sha256:outputs" || receipt.PolicyHash != EmptyPolicyHash || receipt.Engine != "" || receipt.Model != "" {
		t.Fatalf("wrong receipt fields: %+v", receipt)
	}
	sig, err := base64.StdEncoding.DecodeString(receipt.Signature)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := publicKeyOf(t, signer)
	if !ok {
		t.Fatal("missing signer public key")
	}
	digest := sha256.Sum256(CanonicalizeV2(receipt))
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Fatal("signature did not verify")
	}
	receipt.InputSHA256 = "sha256:tampered"
	digest = sha256.Sum256(CanonicalizeV2(receipt))
	if ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Fatal("signature accepted tampered manifest")
	}
	if _, err := BuildSignedRunJobTemplateReceipt(signer, "n1", "job\ninjected", "sha256:manifest", "sha256:outputs", time.Unix(0, 0)); err == nil {
		t.Fatal("newline collision signed")
	}
}
