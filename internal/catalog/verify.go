// internal/catalog/verify.go
//
// Image-signature verification for module installs (#344). When a trusted source
// is declared as a "verified publisher" with RequireSignature, the module's
// container image must carry a valid cosign signature before it is installed.
//
// Verification shells out to the `cosign` binary on PATH. It supports:
//   - keyful  verification: a configured public key (cosign verify --key ...)
//   - keyless verification: an expected OIDC identity + issuer
//     (cosign verify --certificate-identity ... --certificate-oidc-issuer ...)
//
// Images are verified BY DIGEST: we pin the image to repo@sha256:... using the
// digest resolved in the lockfile / ResolvedModule. If a signature is required
// but no digest can be resolved, verification refuses rather than silently
// downgrading to a tag-only check ("never silently pass").
//
// When cosign is absent from PATH, a declared-but-unverifiable requirement WARNS
// clearly and, because the publisher requires a signature, REFUSES the install.
package catalog

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// VerifyResult is the outcome of attempting signature verification for one image.
type VerifyResult struct {
	// Image is the digest-pinned reference that was (attempted) verified.
	Image string
	// Verified is true only when cosign confirmed a valid signature.
	Verified bool
	// Skipped is true when no signature was required for this source (no-op).
	Skipped bool
}

// CosignAvailable reports whether the cosign binary is on PATH. Pure-ish (only
// consults PATH); table-tested by manipulating PATH.
func CosignAvailable() bool {
	_, err := exec.LookPath("cosign")
	return err == nil
}

// cosignVerifyMode classifies how a publisher entry wants verification done.
type cosignVerifyMode int

const (
	// modeNone: nothing to verify (no key, no identity).
	modeNone cosignVerifyMode = iota
	// modeKeyful: verify against a configured public key.
	modeKeyful
	// modeKeyless: verify against an expected OIDC identity + issuer.
	modeKeyless
)

// publisherVerifyMode picks the verification mode for a publisher. Keyful takes
// precedence when a key is set; otherwise an identity selects keyless. Pure --
// table-tested.
func publisherVerifyMode(pub VerifiedPublisher) cosignVerifyMode {
	if strings.TrimSpace(pub.Key) != "" {
		return modeKeyful
	}
	if strings.TrimSpace(pub.Identity) != "" {
		return modeKeyless
	}
	return modeNone
}

// buildCosignArgs constructs the argv (after the leading "cosign") for verifying
// imageRef against pub. Returns an error if the publisher has neither a key nor
// an identity, or (for keyless) is missing an issuer. Pure -- table-tested, no
// cosign/network needed.
//
// Cosign v2 flag names (verified against current sigstore docs):
//   - keyful : verify --key <key> <imageRef>
//   - keyless: verify --certificate-identity <id> --certificate-oidc-issuer <iss> <imageRef>
func buildCosignArgs(pub VerifiedPublisher, imageRef string) ([]string, error) {
	switch publisherVerifyMode(pub) {
	case modeKeyful:
		return []string{"verify", "--key", strings.TrimSpace(pub.Key), imageRef}, nil
	case modeKeyless:
		issuer := strings.TrimSpace(pub.Issuer)
		if issuer == "" {
			return nil, fmt.Errorf("keyless verification requires an issuer (set --issuer when trusting %q)", pub.Pattern)
		}
		return []string{
			"verify",
			"--certificate-identity", strings.TrimSpace(pub.Identity),
			"--certificate-oidc-issuer", issuer,
			imageRef,
		}, nil
	default:
		return nil, fmt.Errorf("verified publisher %q declares no signing key or identity", pub.Pattern)
	}
}

// pinImageToDigest returns "repo@sha256:..." when a digest is known, pinning the
// image so verification targets an exact artifact rather than a mutable tag. If
// ref already contains an "@sha256:" it is returned unchanged. Returns "" when no
// digest is available. Pure -- table-tested.
func pinImageToDigest(ref, digest string) string {
	ref = strings.TrimSpace(ref)
	digest = strings.TrimSpace(digest)
	if strings.Contains(ref, "@sha256:") {
		return ref
	}
	if digest == "" {
		return ""
	}
	// Strip any tag (":tag") from the repo part before appending the digest. The
	// repo may contain a registry "host:port/", so only treat a ':' after the
	// last '/' as a tag separator.
	repo := ref
	if slash := strings.LastIndex(ref, "/"); slash >= 0 {
		if colon := strings.LastIndex(ref[slash:], ":"); colon >= 0 {
			repo = ref[:slash+colon]
		}
	} else if colon := strings.LastIndex(ref, ":"); colon >= 0 {
		repo = ref[:colon]
	}
	return repo + "@" + digest
}

// VerifyModule verifies the resolved module's image signatures against a matched
// verified-publisher entry. It is a NO-OP (Skipped=true, nil error) when no
// publisher matches the source or the matched publisher does not require a
// signature. When a signature IS required it returns an error (refusing install)
// on any failure: cosign absent, no resolvable digest, or a failed verification.
//
// images maps an image reference to its best-effort resolved digest (as recorded
// in the lockfile). Verification pins each image to its digest before calling
// cosign.
func VerifyModule(src Source, images []LockImage) (VerifyResult, error) {
	pub, ok := MatchVerifiedPublisher(src)
	if !ok || !pub.RequireSignature {
		return VerifyResult{Skipped: true}, nil
	}

	if !CosignAvailable() {
		return VerifyResult{}, fmt.Errorf(
			"source %q requires a verified signature but the 'cosign' binary was not found on PATH.\n"+
				"   Install cosign (https://docs.sigstore.dev/cosign/installation) or remove the signature\n"+
				"   requirement with: citadel module trust %s (without --require-signature).",
			src.Raw, pub.Pattern)
	}

	if len(images) == 0 {
		return VerifyResult{}, fmt.Errorf("source %q requires a verified signature but no container image was found to verify", src.Raw)
	}

	var verified string
	for _, img := range images {
		pinned := pinImageToDigest(img.Ref, img.Digest)
		if pinned == "" {
			return VerifyResult{}, fmt.Errorf(
				"source %q requires a verified signature but the digest for image %q could not be resolved.\n"+
					"   Verification must pin the image by digest; ensure docker can resolve %q (pull it, or check registry access).",
				src.Raw, img.Ref, img.Ref)
		}
		if err := runCosignVerify(pub, pinned); err != nil {
			return VerifyResult{Image: pinned}, fmt.Errorf("signature verification failed for %s: %w", pinned, err)
		}
		verified = pinned
	}

	return VerifyResult{Image: verified, Verified: true}, nil
}

// runCosignVerify executes `cosign verify ...` for a single pinned image. A
// non-nil error means the signature did not verify (or cosign errored). Bounded
// to 60s. The seam is a package var so callers/tests can stub it.
var runCosignVerify = func(pub VerifiedPublisher, pinnedImage string) error {
	args, err := buildCosignArgs(pub, pinnedImage)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cosign", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("cosign rejected the signature: %s", detail)
	}
	return nil
}
