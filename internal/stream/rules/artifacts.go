package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"
)

const PackArtifactOrigin = "aceteam-ai/citadel-cli"
const maxPackIndexBytes = 256 << 10
const maxPackBundleBytes = 12 << 20

// ArtifactPin belongs in reviewed consumer source, not in request parameters.
// Commit is an exact immutable Git SHA at the fixed origin. This offline
// validator checks syntax and bytes; it does not contact GitHub or establish
// that a commit was reviewed. The caller owns that trusted publication check.
type ArtifactPin struct {
	Commit      string
	IndexDigest string
}

type PackArtifactEntry struct {
	ID           string `json:"id" yaml:"id"`
	Version      string `json:"version" yaml:"version"`
	Digest       string `json:"digest" yaml:"digest"`
	BundlePath   string `json:"bundle_path" yaml:"bundle_path"`
	BundleSHA256 string `json:"bundle_sha256" yaml:"bundle_sha256"`
}

type PackIndex struct {
	SchemaVersion string              `json:"schema_version" yaml:"schema_version"`
	Origin        string              `json:"origin" yaml:"origin"`
	Packs         []PackArtifactEntry `json:"packs" yaml:"packs"`
}

type artifactRule struct {
	File         string `json:"file" yaml:"file"`
	SourceBase64 string `json:"source_base64" yaml:"source_base64"`
}

type artifactBundle struct {
	SchemaVersion  string         `json:"schema_version" yaml:"schema_version"`
	ManifestBase64 string         `json:"manifest_base64" yaml:"manifest_base64"`
	Rules          []artifactRule `json:"rules" yaml:"rules"`
}

// ParsePinnedPackIndex validates an exact-byte SHA-256 index pin. No locator
// comes from a caller: every bundle path is derived from reviewed id/version.
func ParsePinnedPackIndex(pin ArtifactPin, indexData []byte) (*PackIndex, error) {
	if !artifactHex(pin.Commit, 40) || pin.Commit == strings.Repeat("0", 40) || !artifactDigest(pin.IndexDigest) {
		return nil, errors.New("invalid reviewed pack pin")
	}
	if len(indexData) > maxPackIndexBytes || artifactSHA(indexData) != pin.IndexDigest {
		return nil, errors.New("pack index digest mismatch or byte limit exceeded")
	}
	var index PackIndex
	if err := decodeArtifactJSON(indexData, &index); err != nil {
		return nil, fmt.Errorf("pack index: %w", err)
	}
	if index.SchemaVersion != "stream.pack-index.v1" || index.Origin != PackArtifactOrigin || len(index.Packs) == 0 || len(index.Packs) > maxRules {
		return nil, errors.New("unsupported pack index schema/origin or entry count")
	}
	seen := make(map[string]bool)
	for _, entry := range index.Packs {
		key := entry.ID + "\x00" + entry.Version
		if !validID(entry.ID) || !validID(entry.Version) || !artifactDigest(entry.Digest) || !artifactDigest(entry.BundleSHA256) || seen[key] ||
			!fs.ValidPath(entry.BundlePath) || entry.BundlePath != "packs/"+entry.ID+"/"+entry.Version+"/bundle.json" {
			return nil, errors.New("invalid or duplicate pack index entry")
		}
		seen[key] = true
	}
	return &index, nil
}

func (index *PackIndex) Find(id, version, digest string) (PackArtifactEntry, error) {
	if index == nil {
		return PackArtifactEntry{}, errors.New("pack index is required")
	}
	for _, entry := range index.Packs {
		if entry.ID == id && entry.Version == version && entry.Digest == digest {
			return entry, nil
		}
	}
	return PackArtifactEntry{}, errors.New("pack identity is not in the reviewed index")
}

// CompilePinnedPack is pure offline validation. It verifies both outer byte
// pins, canonical manifest bytes, the complete ordered file set and the inner
// pack digest before returning the same compiled Pack used by LoadPack.
func CompilePinnedPack(pin ArtifactPin, indexData, bundleData []byte, id, version, digest string) (*Pack, error) {
	index, err := ParsePinnedPackIndex(pin, indexData)
	if err != nil {
		return nil, err
	}
	entry, err := index.Find(id, version, digest)
	if err != nil {
		return nil, err
	}
	if len(bundleData) > maxPackBundleBytes || artifactSHA(bundleData) != entry.BundleSHA256 {
		return nil, errors.New("pack bundle digest mismatch or byte limit exceeded")
	}
	var bundle artifactBundle
	if err := decodeArtifactJSON(bundleData, &bundle); err != nil {
		return nil, fmt.Errorf("pack bundle: %w", err)
	}
	if bundle.SchemaVersion != "stream.pack-bundle.v1" || len(bundle.Rules) == 0 || len(bundle.Rules) > maxRules {
		return nil, errors.New("unsupported pack bundle schema or rule count")
	}
	// Check all decoded sizes before any base64 payload allocation. The JSON
	// envelope itself is bounded before decoding; no archive extraction exists.
	total, err := artifactDecodedSize(bundle.ManifestBase64, 256<<10)
	if err != nil {
		return nil, err
	}
	for _, rule := range bundle.Rules {
		size, err := artifactDecodedSize(rule.SourceBase64, maxRuleBytes)
		if err != nil {
			return nil, err
		}
		total += size
		if total > maxPackBytes {
			return nil, errors.New("decoded pack exceeds byte limit")
		}
	}
	manifestBytes, err := artifactDecodeBase64(bundle.ManifestBase64)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := decodeStrict(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("artifact manifest: %w", err)
	}
	canonical, err := CanonicalManifestBytes(manifest)
	if err != nil || !bytes.Equal(canonical, manifestBytes) {
		return nil, errors.New("artifact manifest bytes are not canonical")
	}
	if manifest.ID != entry.ID || manifest.Version != entry.Version || len(manifest.Rules) != len(bundle.Rules) {
		return nil, errors.New("artifact manifest identity or rule file set mismatch")
	}
	files := make(map[string][]byte, len(bundle.Rules))
	for i, rule := range bundle.Rules {
		if !fs.ValidPath(rule.File) || strings.Contains(rule.File, "\\") || rule.File != manifest.Rules[i].File {
			return nil, errors.New("artifact rule file set/order mismatch")
		}
		if _, duplicate := files[rule.File]; duplicate {
			return nil, errors.New("duplicate artifact rule file")
		}
		data, err := artifactDecodeBase64(rule.SourceBase64)
		if err != nil {
			return nil, err
		}
		files[rule.File] = data
	}
	compiled, err := loadPackBytes(manifestBytes, func(name string) ([]byte, error) {
		data, found := files[name]
		if !found {
			return nil, errors.New("artifact rule file is missing")
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	if compiled.Digest != entry.Digest {
		return nil, errors.New("artifact pack digest mismatch")
	}
	return compiled, nil
}

func decodeArtifactJSON(data []byte, out any) error {
	if !utf8.Valid(data) || !json.Valid(data) {
		return errors.New("artifact must be UTF-8 JSON")
	}
	// Reuse the shared structural/duplicate checks, then decode through JSON
	// types. YAML alone would coerce a numeric JSON id into a Go string.
	var structural any
	if err := decodeStrict(data, &structural); err != nil {
		return err
	}
	// encoding/json also accepts case-folded aliases for tagged struct fields.
	// Validate exact member names before typed decoding so id/ID cannot alias
	// or overwrite each other across implementations.
	var topKeys, listKeys []string
	var listName string
	switch out.(type) {
	case *PackIndex:
		topKeys = []string{"schema_version", "origin", "packs"}
		listName = "packs"
		listKeys = []string{"id", "version", "digest", "bundle_path", "bundle_sha256"}
	case *artifactBundle:
		topKeys = []string{"schema_version", "manifest_base64", "rules"}
		listName = "rules"
		listKeys = []string{"file", "source_base64"}
	default:
		return errors.New("unsupported artifact JSON target")
	}
	object, err := artifactExactMembers(structural, topKeys)
	if err != nil {
		return err
	}
	items, ok := object[listName].([]any)
	if !ok {
		return errors.New("artifact member must be an array")
	}
	for _, item := range items {
		if _, err := artifactExactMembers(item, listKeys); err != nil {
			return err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

func artifactExactMembers(value any, names []string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("artifact must contain an object")
	}
	for key := range object {
		found := false
		for _, name := range names {
			found = found || key == name
		}
		if !found {
			return nil, errors.New("unknown artifact member; names are case-sensitive")
		}
	}
	if len(object) != len(names) {
		return nil, errors.New("artifact object has missing required members")
	}
	return object, nil
}

func artifactDecodedSize(value string, maximum int) (int, error) {
	if len(value) == 0 || len(value)%4 != 0 || len(value) > (maximum+2)/3*4 || strings.ContainsAny(value, "\r\n") {
		return 0, errors.New("artifact base64 exceeds limit or is noncanonical")
	}
	size := len(value) / 4 * 3
	if strings.HasSuffix(value, "=") {
		size--
	}
	if strings.HasSuffix(value, "==") {
		size--
	}
	if size > maximum {
		return 0, errors.New("artifact base64 exceeds decoded byte limit")
	}
	return size, nil
}

func artifactDecodeBase64(value string) ([]byte, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || base64.StdEncoding.EncodeToString(data) != value {
		return nil, errors.New("artifact base64 is not canonical padded RFC4648")
	}
	return data, nil
}

func artifactHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func artifactDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && artifactHex(strings.TrimPrefix(value, "sha256:"), 64)
}
func artifactSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
