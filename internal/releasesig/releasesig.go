// Package releasesig owns the detached release-signature format shared by the
// release signer (scripts/releasesign) and the updater (internal/update).
//
// A release publishes checksums.txt (sha256sum output for every archive) and
// checksums.txt.sig, a small JSON envelope carrying a pure Ed25519 signature
// over the exact checksums.txt bytes:
//
//	{"v":1,"key_id":"<16 hex>","sig":"<std base64 of the 64-byte signature>"}
//
// key_id is KeyID(pub): the first 16 hex characters of sha256 over the raw
// 32-byte Ed25519 public key. It only selects a key from the compiled trusted
// set; a signature verifies only against a key in that set. See
// docs/release-signing.md for the format, rotation and threat model.
//
// This package is a stdlib-only leaf so the signer and verifier share one
// derivation of every byte that matters.
package releasesig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// FormatVersion is the only signature-envelope version this package accepts.
const FormatVersion = 1

// MaxSignatureFileSize bounds a fetched checksums.txt.sig.
const MaxSignatureFileSize = 4 << 10

// TrustedKey is one compiled-in release-signing public key.
type TrustedKey struct {
	// ID must equal KeyID(PublicKey); a test pins this for every entry.
	ID        string
	PublicKey ed25519.PublicKey
}

// trustedKeys is the compiled-in trusted release-signing key set. It is a
// slice so a rotation can ship a release trusting both the outgoing and the
// incoming key (docs/release-signing.md). There is intentionally no setter,
// environment variable or flag that changes it.
var trustedKeys = []TrustedKey{
	mustKey("acc280898c1b8791", "rTLR1+BJtYKtxOJJan2OUod9xZNl9F3nbPyAf0np0n8="),
}

func mustKey(id, b64 string) TrustedKey {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		panic(fmt.Sprintf("releasesig: malformed compiled public key %s", id))
	}
	return TrustedKey{ID: id, PublicKey: ed25519.PublicKey(raw)}
}

// TrustedKeys returns a copy of the compiled-in trusted key set.
func TrustedKeys() []TrustedKey {
	out := make([]TrustedKey, len(trustedKeys))
	for i, k := range trustedKeys {
		out[i] = TrustedKey{ID: k.ID, PublicKey: append(ed25519.PublicKey(nil), k.PublicKey...)}
	}
	return out
}

// KeyID derives the key identifier for an Ed25519 public key: the first 16
// hex characters of sha256 over the raw 32-byte key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

// Envelope is the JSON content of checksums.txt.sig.
type Envelope struct {
	V     int    `json:"v"`
	KeyID string `json:"key_id"`
	Sig   string `json:"sig"`
}

// Errors returned by Verify. Callers may match them with errors.Is.
var (
	ErrMalformedSignature = errors.New("malformed release signature")
	ErrUnknownKey         = errors.New("release signature key is not trusted")
	ErrBadSignature       = errors.New("release signature does not verify")
)

// Sign produces the checksums.txt.sig bytes for the exact message bytes.
func Sign(priv ed25519.PrivateKey, message []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid ed25519 private key")
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("invalid ed25519 private key")
	}
	env := Envelope{
		V:     FormatVersion,
		KeyID: KeyID(pub),
		Sig:   base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message)),
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Verify checks sigFile (the checksums.txt.sig bytes) over the exact message
// bytes against the compiled trusted key set and returns the signing key id.
func Verify(message, sigFile []byte) (string, error) {
	return VerifyWithKeys(message, sigFile, trustedKeys)
}

// VerifyWithKeys is Verify against an explicit key set. Production callers use
// Verify; this exists so tests can exercise generated keys.
func VerifyWithKeys(message, sigFile []byte, keys []TrustedKey) (string, error) {
	env, err := parseEnvelope(sigFile)
	if err != nil {
		return "", err
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(env.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: signature is not a %d-byte base64 value", ErrMalformedSignature, ed25519.SignatureSize)
	}
	for _, k := range keys {
		if k.ID != env.KeyID {
			continue
		}
		if len(k.PublicKey) != ed25519.PublicKeySize || KeyID(k.PublicKey) != k.ID {
			return "", fmt.Errorf("%w: trusted key %s is inconsistent", ErrUnknownKey, k.ID)
		}
		if !ed25519.Verify(k.PublicKey, message, sig) {
			return "", fmt.Errorf("%w (key %s)", ErrBadSignature, k.ID)
		}
		return k.ID, nil
	}
	return "", fmt.Errorf("%w: key id %q", ErrUnknownKey, env.KeyID)
}

func parseEnvelope(sigFile []byte) (Envelope, error) {
	var env Envelope
	if len(sigFile) == 0 || len(sigFile) > MaxSignatureFileSize {
		return env, fmt.Errorf("%w: unexpected size %d", ErrMalformedSignature, len(sigFile))
	}
	dec := json.NewDecoder(bytes.NewReader(sigFile))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return env, fmt.Errorf("%w: %v", ErrMalformedSignature, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return env, fmt.Errorf("%w: trailing data", ErrMalformedSignature)
	}
	if env.V != FormatVersion {
		return env, fmt.Errorf("%w: unsupported version %d", ErrMalformedSignature, env.V)
	}
	if len(env.KeyID) != 16 {
		return env, fmt.Errorf("%w: key id must be 16 hex characters", ErrMalformedSignature)
	}
	if _, err := hex.DecodeString(env.KeyID); err != nil {
		return env, fmt.Errorf("%w: key id must be 16 hex characters", ErrMalformedSignature)
	}
	return env, nil
}
