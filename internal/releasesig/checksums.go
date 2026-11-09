package releasesig

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxChecksumsFileSize bounds a fetched checksums.txt.
const MaxChecksumsFileSize = 64 << 10

// tagPattern is the release tag shape accepted before a tag is used in a URL
// or bound against a checksums file: v<major>.<minor>.<patch> with an optional
// semver pre-release suffix.
var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

var checksumLine = regexp.MustCompile(`^([0-9a-f]{64}) [ *]([A-Za-z0-9._-]+)$`)

// ValidTag reports whether tag is an acceptable release tag.
func ValidTag(tag string) bool { return tagPattern.MatchString(tag) }

// ArchivePrefix is the filename prefix every archive of release tag carries.
func ArchivePrefix(tag string) string { return "citadel_" + tag + "_" }

// ParseChecksums parses checksums.txt content (sha256sum output) and binds it
// to tag: every entry must name an archive of exactly that release
// (citadel_<tag>_...). Because the signature covers these bytes, a validly
// signed checksums file from one release cannot be presented for another.
// Callers must verify the signature before calling this.
func ParseChecksums(data []byte, tag string) (map[string]string, error) {
	if !ValidTag(tag) {
		return nil, fmt.Errorf("invalid release tag %q", tag)
	}
	if len(data) == 0 || len(data) > MaxChecksumsFileSize {
		return nil, fmt.Errorf("checksums file has unexpected size %d", len(data))
	}
	text := string(data)
	if !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("checksums file must end with a newline")
	}
	prefix := ArchivePrefix(tag)
	out := map[string]string{}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		m := checksumLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("checksums line %d is malformed", i+1)
		}
		digest, name := m[1], m[2]
		if !strings.HasPrefix(name, prefix) {
			return nil, fmt.Errorf("checksums line %d names %q, which is not an archive of %s", i+1, name, tag)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("checksums file lists %q more than once", name)
		}
		out[name] = digest
	}
	return out, nil
}
