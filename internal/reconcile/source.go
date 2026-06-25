package reconcile

import "strings"

// NameFromSource derives a canonical module name from a source string when the
// control plane did not provide an explicit Name. It mirrors how the module
// install path names a service, WITHOUT importing internal/catalog (so the wire
// contract stays decoupled):
//
//   - catalog name "embedding"          -> "embedding"
//   - "owner/repo@v1.2.0"               -> "repo"
//   - "https://host/owner/repo.git@ref" -> "repo"
//   - "git@github.com:owner/repo.git"   -> "repo"
//
// Note: deriving the name from the source means a source-ref change does NOT
// change the key (good — it reads as an update, not remove+add), but two
// different repos with the same trailing name would collide. The control plane
// SHOULD therefore set ModuleAssignment.Name explicitly; this is a best-effort
// fallback.
func NameFromSource(source string) string {
	s := strings.TrimSpace(source)
	if s == "" {
		return ""
	}

	// Strip an "@ref" suffix (but not the "@" in an scp-style git URL such as
	// "git@github.com:owner/repo.git", where "@" precedes the host).
	if at := strings.LastIndex(s, "@"); at > 0 {
		tail := s[at+1:]
		// A ref never contains "/" or ":"; an scp host part does.
		if tail != "" && !strings.ContainsAny(tail, "/:") {
			s = s[:at]
		}
	}

	// A bare catalog name has no path separators.
	if !strings.ContainsAny(s, "/:") {
		return s
	}

	// Take the last path segment, splitting on both "/" and ":" (scp form).
	seg := s
	if i := strings.LastIndexAny(seg, "/:"); i >= 0 {
		seg = seg[i+1:]
	}
	seg = strings.TrimSuffix(seg, ".git")
	return seg
}
