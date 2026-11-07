// Package releaseverify implements an inactive release-signing contract.
// No updater or publisher imports it. Trust-root selection and paired rollout
// require separate approval before any node enforcement is connected.
package releaseverify

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

const (
	Repository              = "aceteam-ai/citadel-cli"
	Schema                  = 1
	MaxManifestBytes        = 64 << 10
	MaxSignatureBytes       = 8 << 10
	MaxArtifacts            = 32
	MaxSignatures           = 8
	MaxArchiveBytes   int64 = 1 << 30
	signingDomain           = "aceteam.ai/citadel-release/v1\x00"
)

var (
	tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)
	keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	hexPattern = regexp.MustCompile(`^[0-9a-f]+$`)
)

// Manifest binds all archive digests to an exact repository, release, tag and
// source commit. Artifacts are sorted by name; its encoded bytes are signed.
type Manifest struct {
	Schema     int        `json:"schema"`
	Repository string     `json:"repository"`
	ReleaseID  int64      `json:"release_id"`
	Tag        string     `json:"tag"`
	Commit     string     `json:"commit"`
	Artifacts  []Artifact `json:"artifacts"`
}

type Artifact struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ObservedRelease must come from the future fixed-origin release adapter,
// including a resolved full commit. Missing immutable evidence fails closed.
// Caller-controlled job fields must never construct this observation.
type ObservedRelease struct {
	Repository string
	ReleaseID  int64
	Tag        string
	Commit     string
	Immutable  bool
	Draft      bool
}

// Root is supplied by a future compiled trust policy, never by release assets.
// This package provides no production roots or default trust fallback.
type Root struct {
	ID        string
	PublicKey ed25519.PublicKey
}

type Signature struct {
	KeyID string `json:"key_id"`
	Value string `json:"signature"`
}

type signatureSet struct {
	Schema     int         `json:"schema"`
	Signatures []Signature `json:"signatures"`
}

// Verified holds only authenticated artifact records. Its zero value refuses
// all archives; callers cannot construct a successful value themselves.
type Verified struct {
	artifacts map[string]Artifact
	checksums []byte
}

func validCommit(commit string) bool {
	return (len(commit) == 40 || len(commit) == 64) && hexPattern.MatchString(commit)
}

func validate(m Manifest) error {
	if m.Schema != Schema || m.Repository != Repository || m.ReleaseID <= 0 ||
		len(m.Tag) > 64 || !tagPattern.MatchString(m.Tag) || !validCommit(m.Commit) {
		return errors.New("invalid release manifest identity")
	}
	if len(m.Artifacts) == 0 || len(m.Artifacts) > MaxArtifacts {
		return errors.New("invalid release artifact count")
	}
	previous := ""
	for _, artifact := range m.Artifacts {
		allowed := false
		for _, suffix := range []string{"darwin_amd64.tar.gz", "darwin_arm64.tar.gz", "linux_amd64.tar.gz", "linux_arm64.tar.gz", "windows_amd64.zip", "windows_arm64.zip"} {
			if artifact.Name == "citadel_"+m.Tag+"_"+suffix {
				allowed = true
				break
			}
		}
		if !allowed || artifact.Name <= previous || artifact.Size <= 0 || artifact.Size > MaxArchiveBytes ||
			len(artifact.SHA256) != 64 || !hexPattern.MatchString(artifact.SHA256) {
			return errors.New("invalid release artifact record")
		}
		previous = artifact.Name
	}
	return nil
}

// Checksums returns the exact checksums.txt projection authenticated by the
// manifest signature. No substring matching or second authority is involved.
func Checksums(m Manifest) ([]byte, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	var data bytes.Buffer
	for _, artifact := range m.Artifacts {
		data.WriteString(artifact.SHA256 + "  " + artifact.Name + "\n")
	}
	return data.Bytes(), nil
}

// EncodeManifest returns the sole accepted JSON representation. Strict
// re-encoding also rejects duplicate fields, unknown fields and trailing data.
func EncodeManifest(m Manifest) ([]byte, error) {
	if err := validate(m); err != nil {
		return nil, err
	}
	data, err := json.Marshal(m)
	if err != nil || len(data) > MaxManifestBytes {
		return nil, errors.New("release manifest exceeds limit")
	}
	return data, nil
}

func decodeCanonical(data []byte, max int, target any) error {
	if len(data) == 0 || len(data) > max {
		return errors.New("release metadata exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid release metadata")
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(data, canonical) {
		return errors.New("noncanonical release metadata")
	}
	return nil
}

func parseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := decodeCanonical(data, MaxManifestBytes, &m); err != nil {
		return Manifest{}, err
	}
	if err := validate(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func signatureBytes(s Signature) ([]byte, error) {
	if !keyPattern.MatchString(s.KeyID) {
		return nil, errors.New("invalid signing key identifier")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s.Value)
	if err != nil || len(raw) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(raw) != s.Value {
		return nil, errors.New("invalid release signature encoding")
	}
	return raw, nil
}

func EncodeSignatures(signatures []Signature) ([]byte, error) {
	if len(signatures) == 0 || len(signatures) > MaxSignatures {
		return nil, errors.New("invalid signature count")
	}
	seen := make(map[string]bool)
	for _, s := range signatures {
		if _, err := signatureBytes(s); err != nil {
			return nil, err
		}
		if seen[s.KeyID] {
			return nil, errors.New("duplicate signing key identifier")
		}
		seen[s.KeyID] = true
	}
	data, err := json.Marshal(signatureSet{Schema, signatures})
	if err != nil || len(data) > MaxSignatureBytes {
		return nil, errors.New("release signatures exceed limit")
	}
	return data, nil
}

func signingMessage(data []byte, keyID string) []byte {
	return append([]byte(signingDomain+keyID+"\x00"), data...)
}

// Sign is a pure producer primitive, not a release command. It never loads,
// creates, persists or logs credentials. The future publisher supplies its key.
func Sign(data []byte, keyID string, privateKey ed25519.PrivateKey) (Signature, error) {
	if _, err := parseManifest(data); err != nil {
		return Signature{}, err
	}
	if !keyPattern.MatchString(keyID) || len(privateKey) != ed25519.PrivateKeySize {
		return Signature{}, errors.New("invalid release signing input")
	}
	// Ed25519 private keys include a cached public suffix; refuse inconsistent
	// input instead of producing a signature the configured root cannot verify.
	derived := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])
	if !bytes.Equal(derived, privateKey) {
		return Signature{}, errors.New("invalid release signing input")
	}
	value := ed25519.Sign(privateKey, signingMessage(data, keyID))
	return Signature{keyID, base64.StdEncoding.EncodeToString(value)}, nil
}

func Verify(data, signatures []byte, observed ObservedRelease, roots []Root) (Verified, error) {
	if !observed.Immutable || observed.Draft {
		return Verified{}, errors.New("release is not published and immutable")
	}
	m, err := parseManifest(data)
	if err != nil {
		return Verified{}, err
	}
	if m.Repository != observed.Repository || m.ReleaseID != observed.ReleaseID || m.Tag != observed.Tag || m.Commit != observed.Commit {
		return Verified{}, errors.New("signed release identity mismatch")
	}
	trusted := make(map[string]ed25519.PublicKey)
	for _, root := range roots {
		if !keyPattern.MatchString(root.ID) || len(root.PublicKey) != ed25519.PublicKeySize || trusted[root.ID] != nil {
			return Verified{}, errors.New("invalid release trust policy")
		}
		trusted[root.ID] = bytes.Clone(root.PublicKey)
	}
	var set signatureSet
	if err := decodeCanonical(signatures, MaxSignatureBytes, &set); err != nil {
		return Verified{}, err
	}
	if set.Schema != Schema {
		return Verified{}, errors.New("unsupported signature schema")
	}
	if _, err := EncodeSignatures(set.Signatures); err != nil {
		return Verified{}, err
	}
	authenticated := false
	for _, signature := range set.Signatures {
		value, _ := signatureBytes(signature)
		if key := trusted[signature.KeyID]; key != nil && ed25519.Verify(key, signingMessage(data, signature.KeyID), value) {
			authenticated = true
		}
	}
	if !authenticated {
		return Verified{}, errors.New("release signer is not trusted")
	}
	artifacts := make(map[string]Artifact, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		artifacts[artifact.Name] = artifact
	}
	checksums, _ := Checksums(m)
	return Verified{artifacts, checksums}, nil
}

// VerifyArchive consumes at most the signed size plus one byte. Callers must
// keep extraction and installation unreachable until this method succeeds.
func (v Verified) VerifyArchive(name string, archive io.Reader) error {
	artifact, ok := v.artifacts[name]
	if !ok || archive == nil {
		return errors.New("archive absent from signed manifest")
	}
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(archive, artifact.Size+1))
	if err != nil {
		return errors.New("archive read failed")
	}
	if size != artifact.Size {
		return errors.New("signed archive size mismatch")
	}
	if hex.EncodeToString(digest.Sum(nil)) != artifact.SHA256 {
		return errors.New("signed archive checksum mismatch")
	}
	return nil
}

// VerifyChecksums authenticates the entire canonical checksum file using the
// signed manifest, refusing additional records, altered names and duplicates.
func (v Verified) VerifyChecksums(data []byte) error {
	if v.artifacts == nil || len(data) > MaxManifestBytes || !bytes.Equal(data, v.checksums) {
		return errors.New("signed checksum file mismatch")
	}
	return nil
}
