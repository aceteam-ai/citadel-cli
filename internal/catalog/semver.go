package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Version resolution for module sources. A module ref can be:
//
//   - an exact tag/branch/SHA (handled by the existing source.go path) -- e.g.
//     "v1.2.3", "main", "deadbeef..."; left UNCHANGED by this package.
//   - a semver constraint -- e.g. "^1.2", "~1.2.3", ">=1.0 <2.0", "1.2.3".
//   - a channel -- "stable" (highest non-prerelease tag) or "latest" (highest
//     tag, including prereleases).
//
// All logic here is PURE: it takes a list of tag strings (as returned by
// `git ls-remote --tags` / `git tag --list`) and resolves the best match,
// without touching git or the filesystem. This keeps it table-testable under the
// only test path allowed in this repo (./internal/catalog/...).

// Channel names.
const (
	ChannelStable = "stable"
	ChannelLatest = "latest"
)

// IsChannel reports whether ref names a release channel (stable/latest).
func IsChannel(ref string) bool {
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case ChannelStable, ChannelLatest:
		return true
	}
	return false
}

// IsVersionConstraint reports whether ref should be resolved against the tag
// list as a semver constraint or channel, as opposed to an exact tag/branch/SHA.
//
// A ref is treated as a constraint/channel when it is a channel name OR it parses
// as a semver constraint AND is NOT a bare exact tag we should pin verbatim.
// Crucially, a value that "looks like a SHA" or an arbitrary branch name
// ("main", "dev") must NOT be treated as a constraint -- those keep the old exact
// behavior. A plain "1.2.3" / "v1.2.3" IS treated as a constraint (per the task),
// which still resolves to that exact tag if present.
func IsVersionConstraint(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if IsChannel(ref) {
		return true
	}
	// A raw commit SHA is an exact pin, never a constraint.
	if looksLikeSHA(ref) {
		return false
	}
	// Must contain a digit somewhere to be a version constraint; this excludes
	// branch names like "main"/"dev"/"feature-x" that would otherwise be rejected
	// by NewConstraint anyway, but we short-circuit for clarity.
	if !strings.ContainsAny(ref, "0123456789") {
		return false
	}
	if _, err := semver.NewConstraint(ref); err != nil {
		return false
	}
	return true
}

// ResolveVersion resolves a constraint/channel ref against a list of tag strings,
// returning the best-matching tag (verbatim, as it appears in tags, so the caller
// can check it out). Tags may be "v"-prefixed or bare; both are handled.
//
//   - channel "stable": highest non-prerelease semver tag.
//   - channel "latest": highest semver tag, including prereleases.
//   - a constraint: highest tag satisfying it. By default prereleases are
//     excluded unless the constraint itself references a prerelease (Masterminds
//     semantics), which is the sane default for "^1.2" etc.
//
// Returns an error if ref is not a constraint/channel, or if no tag matches.
func ResolveVersion(ref string, tags []string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty version ref")
	}

	type taggedVersion struct {
		raw string
		ver *semver.Version
	}
	var parsed []taggedVersion
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		v, err := semver.NewVersion(t)
		if err != nil {
			continue // non-semver tag (e.g. "nightly") -- ignored for resolution
		}
		parsed = append(parsed, taggedVersion{raw: t, ver: v})
	}
	if len(parsed) == 0 {
		return "", fmt.Errorf("no semver tags available to resolve %q", ref)
	}

	// Sort descending by semver precedence, with a stable raw-string tie-break so
	// equal-precedence tags (e.g. "v1.0.0" vs "1.0.0") resolve deterministically.
	sort.Slice(parsed, func(i, j int) bool {
		if parsed[i].ver.Equal(parsed[j].ver) {
			return parsed[i].raw < parsed[j].raw
		}
		return parsed[i].ver.GreaterThan(parsed[j].ver)
	})

	if IsChannel(ref) {
		includePre := strings.EqualFold(ref, ChannelLatest)
		for _, tv := range parsed {
			if !includePre && tv.ver.Prerelease() != "" {
				continue
			}
			return tv.raw, nil
		}
		return "", fmt.Errorf("no matching tag for channel %q (tags: %s)", ref, strings.Join(tags, ", "))
	}

	constraint, err := semver.NewConstraint(ref)
	if err != nil {
		return "", fmt.Errorf("invalid version constraint %q: %w", ref, err)
	}
	for _, tv := range parsed {
		if constraint.Check(tv.ver) {
			return tv.raw, nil
		}
	}
	return "", fmt.Errorf("no tag satisfies constraint %q (tags: %s)", ref, strings.Join(tags, ", "))
}

// parseLsRemoteTags parses the output of `git ls-remote --tags <url>` into a
// de-duplicated list of tag names. Each line is "<sha>\trefs/tags/<name>".
// Annotated-tag dereference lines ("refs/tags/<name>^{}") are normalized to the
// underlying tag name. Pure -- table-tested.
func parseLsRemoteTags(out string) []string {
	seen := make(map[string]bool)
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ref := fields[len(fields)-1]
		const prefix = "refs/tags/"
		if !strings.HasPrefix(ref, prefix) {
			continue
		}
		name := strings.TrimPrefix(ref, prefix)
		name = strings.TrimSuffix(name, "^{}") // dereferenced annotated tag
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		tags = append(tags, name)
	}
	return tags
}
