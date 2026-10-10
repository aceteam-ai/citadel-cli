// Package desktopmodel contains unreachable synthetic archive-validation and
// approved pure model-planning foundations. It does not install, execute,
// download, activate, or serve a model.
package desktopmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

type foundationError string

func (e foundationError) Error() string { return string(e) }

const (
	errManifest     foundationError = "manifest_invalid"
	errState        foundationError = "state_invalid"
	errArchive      foundationError = "archive_invalid"
	errStorage      foundationError = "storage_unavailable"
	errUncertain    foundationError = "publication_uncertain"
	errUnsupported  foundationError = "storage_unsupported"
	manifestLimit                   = 1024
	stateLimit                      = 4096
	compressedLimit                 = 8 * 1024 * 1024
)

type manifest struct {
	SchemaVersion   uint64 `json:"schema_version"`
	ArchiveFormat   string `json:"archive_format"`
	CompressedBytes uint64 `json:"compressed_bytes"`
	ArchiveSHA256   string `json:"archive_sha256"`
}

func readBounded(r io.Reader, limit int64, failure foundationError) ([]byte, error) {
	if r == nil || limit < 0 || limit == int64(^uint64(0)>>1) {
		return nil, failure
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, failure
	}
	return b, nil
}

// flatObject refuses duplicates before a map can discard their provenance.
func flatObject(b []byte, fields []string, limit int, failure foundationError) (map[string]json.RawMessage, error) {
	if len(b) > limit || !utf8.Valid(b) {
		return nil, failure
	}
	d := json.NewDecoder(bytes.NewReader(b))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, failure
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	values := make(map[string]json.RawMessage, len(fields))
	for d.More() {
		t, err = d.Token()
		key, ok := t.(string)
		if err != nil || !ok || !allowed[key] {
			return nil, failure
		}
		if _, exists := values[key]; exists {
			return nil, failure
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, failure
		}
		values[key] = value
	}
	t, err = d.Token()
	if err != nil || t != json.Delim('}') || len(values) != len(fields) {
		return nil, failure
	}
	var extra json.RawMessage
	if d.Decode(&extra) != io.EOF {
		return nil, failure
	}
	return values, nil
}

func unsigned(raw json.RawMessage) (uint64, bool) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || (len(b) > 1 && b[0] == '0') {
		return 0, false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(string(b), 10, 64)
	return n, err == nil
}

func jsonString(raw json.RawMessage) (string, bool) {
	b := bytes.TrimSpace(raw)
	if len(b) < 2 || b[0] != '"' {
		return "", false
	}
	var s string
	err := json.Unmarshal(b, &s)
	return s, err == nil
}

func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (m manifest) valid() bool {
	return m.SchemaVersion == 1 && m.ArchiveFormat == "gzip-ustar" && m.CompressedBytes > 0 && m.CompressedBytes <= compressedLimit && lowerHex(m.ArchiveSHA256, 64)
}

func parseManifest(r io.Reader) (manifest, error) {
	b, err := readBounded(r, manifestLimit, errManifest)
	if err != nil {
		return manifest{}, err
	}
	v, err := flatObject(b, []string{"schema_version", "archive_format", "compressed_bytes", "archive_sha256"}, manifestLimit, errManifest)
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	var ok bool
	if m.SchemaVersion, ok = unsigned(v["schema_version"]); !ok {
		return manifest{}, errManifest
	}
	if m.ArchiveFormat, ok = jsonString(v["archive_format"]); !ok {
		return manifest{}, errManifest
	}
	if m.CompressedBytes, ok = unsigned(v["compressed_bytes"]); !ok {
		return manifest{}, errManifest
	}
	if m.ArchiveSHA256, ok = jsonString(v["archive_sha256"]); !ok || !m.valid() {
		return manifest{}, errManifest
	}
	return m, nil
}

func (m manifest) canonical() ([]byte, error) {
	if !m.valid() {
		return nil, errManifest
	}
	return json.Marshal(m)
}

func (m manifest) fingerprint() (string, error) {
	b, err := m.canonical()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
