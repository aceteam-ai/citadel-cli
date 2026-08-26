// internal/compose/nodedir.go
//
// Embedded-service container-name namespacing under --node-dir/CITADEL_NODE_DIR
// (citadel#860, the container-identity follow-up to #856's compose-project
// scoping — see cmd/nodedir.go's package doc comment for the full incident
// history and citadel#856's compose "-p" scoping, which this complements).
//
// #856 made a compose ACTION against an override dir safe-and-loud relative to
// the REAL node (down -> no-op, up -> loud cross-project failure), by deriving a
// "-p"/"--project-name" from a hash of the resolved override directory. It did
// NOT stop two DIFFERENT override dirs from colliding with EACH OTHER: every
// embedded compose file (services/compose/*.yml, services.ServiceMap) pins a
// GLOBAL `container_name: citadel-<svc>`, unaffected by which directory citadel
// materialized/read the compose file from. Two operators (or two agent
// invocations) running `citadel run vllm --node-dir A` and
// `--node-dir B` both materialize a `vllm.yml` naming the SAME `citadel-vllm`
// container.
//
// This file is the LOW-LEVEL, override-string-in/name-out half of the fix: it
// has no knowledge of the --node-dir flag or CITADEL_NODE_DIR env var, only of
// an already-resolved override directory (or its absence). It lives here
// (internal/compose), not in cmd, specifically so BOTH `cmd` (which resolves
// the override from the --node-dir flag + env var, cmd/nodedir.go) and
// internal/jobs (which cannot import cmd — cmd already imports internal/jobs —
// but mirrors cmd's compose materialization for the SERVICE_START job path,
// internal/jobs/service_handler.go) can derive an IDENTICAL container name from
// the SAME override directory string. Do not duplicate this hashing elsewhere;
// a second implementation that drifts from this one silently reintroduces the
// exact collision this file exists to prevent.
package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// NodeDirHash returns a deterministic 12-hex-char identifier for an override
// directory: sha256 of the absolute, cleaned path, truncated to 12 hex chars.
// This is the EXACT derivation cmd/nodedir.go's composeProjectOverride uses for
// the compose "-p" project name (as "citadel-nodedir-"+NodeDirHash(dir)) — do
// not invent a second hashing scheme here or the two will disagree, defeating
// the point of scoping both the compose project AND the container it starts to
// the same override.
//
// Returns "" for an empty/whitespace-only dir (the "no override" case); callers
// use that as the "apply no namespacing" signal, matching
// composeProjectOverride's "" convention.
func NodeDirHash(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return hex.EncodeToString(sum[:])[:12]
}

// ContainerName returns the container name to materialize/expect for an
// EMBEDDED service (a services.ServiceMap entry) named svc: "citadel-<svc>"
// unchanged when overrideDir is empty (byte-identical to pre-#860 behavior —
// every doc, dashboard query, and `docker ps` habit assumes this), or
// "citadel-<hash>-<svc>" under an active override, where <hash> is
// NodeDirHash(overrideDir) — the SAME value the compose "-p" project for that
// override derives, so a compose action's project and the container it starts
// under always name-agree on which override owns them.
//
// SCOPE: for services.ServiceMap entries only. Catalog/third-party module
// compose files author their own container_name and are not namespaced by this
// function — callers must not call it for a module name that isn't a known
// embedded service (see cmd/nodedir.go's embeddedContainerName, the intended
// call site, which already gates on ServiceMap membership).
func ContainerName(svc, overrideDir string) string {
	hash := NodeDirHash(overrideDir)
	if hash == "" {
		return "citadel-" + svc
	}
	return "citadel-" + hash + "-" + svc
}

// RewriteContainerNameLine rewrites the single "container_name: citadel-<svc>"
// line an embedded compose file pins to newName, and returns the modified
// content. It is a targeted string replace (not a full YAML round-trip) so
// vendored formatting/comments in the embedded template survive untouched —
// matching how the rest of this materialization path treats these files
// (ensureComposeFile / ensureEmbeddedComposeFile write the embedded template
// verbatim otherwise).
//
// Errors (rather than silently no-op'ing) if the exact expected line is not
// present, because a silent no-op here means the compose file materializes
// with the OLD (colliding) container_name — exactly the incident this file
// exists to prevent. "container_name: citadel-<svc>" (not just "citadel-<svc>")
// is deliberately the search string: some services also reference
// "citadel-<svc>" in an `image:` tag (e.g. bonsai's locally-built
// `citadel-bonsai:local`), and a bare-substring replace would corrupt that too.
func RewriteContainerNameLine(content, svc, newName string) (string, error) {
	old := "container_name: citadel-" + svc
	if !strings.Contains(content, old) {
		return "", fmt.Errorf("embedded compose for %q does not contain expected line %q; refusing to materialize under a --node-dir override without container-name isolation", svc, old)
	}
	return strings.Replace(content, old, "container_name: "+newName, 1), nil
}

// EnsureNamespacedContainerName reconciles the container_name an
// ALREADY-EXISTING (or about-to-be-written) embedded compose file's content
// carries against what it should carry under overrideDir. This exists because
// ensureComposeFile/ensureEmbeddedComposeFile's "the .yml already exists,
// leave it alone" fast path predates #860 and, left unchanged, would leave a
// file materialized by a pre-#860 binary -- or by THIS binary before an
// override was ever set for that dir -- carrying the UNnamespaced
// "citadel-<svc>" forever. That is not just cosmetic: a later `citadel run`/
// `module start` under that override would then `up` a compose file pinning
// the REAL node's global container name, scoped only by the override's
// compose project (#856) -- and #856's "safe and loud" guarantee depends on
// the real container currently existing; if it's stopped/absent, that `up`
// SUCCEEDS and silently annexes the real node's container name under the
// override project. Reconciling on every materialization call (not just at
// first-write) closes that.
//
// Contract:
//   - overrideDir == "" -> (existing, false, nil): a no-op. Callers should not
//     need to invoke this in the no-override path at all; kept as a documented
//     no-op rather than a precondition so it is safe to call unconditionally.
//   - existing already carries the expected namespaced container_name (this
//     override already reconciled it, or it was freshly written) -> no-op.
//   - existing carries the UNnamespaced default "citadel-<svc>" -> rewrite
//     that line to the namespaced name (the pre-#860 / stale-file case above).
//   - existing carries NEITHER the default nor the expected namespaced name --
//     hand-edited by an operator, or materialized under a DIFFERENT
//     override's hash (e.g. --node-dir pointed at this same services/ dir
//     under two different override paths, or a relative --node-dir resolved
//     against a different CWD) -- refuse loudly rather than silently
//     overwriting content this function cannot prove is safe to replace.
func EnsureNamespacedContainerName(existing, svc, overrideDir string) (content string, changed bool, err error) {
	if strings.TrimSpace(overrideDir) == "" {
		return existing, false, nil
	}
	expected := ContainerName(svc, overrideDir)
	if strings.Contains(existing, "container_name: "+expected) {
		return existing, false, nil
	}
	defaultLine := "container_name: citadel-" + svc
	if !strings.Contains(existing, defaultLine) {
		return "", false, fmt.Errorf(
			"materialized compose for %q carries neither the expected namespaced container_name (%q) nor the "+
				"unnamespaced default (%q) -- refusing to overwrite what may be a hand-edited file or one "+
				"materialized under a DIFFERENT --node-dir override", svc, expected, defaultLine)
	}
	rewritten, err := RewriteContainerNameLine(existing, svc, expected)
	if err != nil {
		return "", false, err
	}
	return rewritten, true, nil
}
