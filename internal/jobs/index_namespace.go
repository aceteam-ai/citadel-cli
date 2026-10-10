// internal/jobs/index_namespace.go
//
// Per-org index namespaces for FILE_INDEX / FILE_SEMANTIC_SEARCH (aceteam#10876
// C2, citadel-cli#1263). A job payload's optional `index` field (shape
// `org_<id>/<name>`, set by the coordinator from the requesting org) resolves to
// a per-namespace SQLite DB under <node_config_dir>/indexes/, so two orgs sharing
// a node never share one index. When `index` is ABSENT the handlers fall back to
// the single legacy DB (resolveIndexDBPath: CITADEL_INDEX_DB or
// ~/citadel-node/index.db), so existing, un-namespaced behavior is unchanged.
//
// SECURITY: the namespace comes from a payload and must never escape the indexes
// dir. resolveIndexNamespaceDBPath validates the namespace SHAPE (a strict
// allowlist regex) AND verifies the cleaned, symlink-resolved absolute path is
// provably confined under the indexes dir (mirroring the instance state-volume
// resolver, citadel-cli#1163). It fails CLOSED: a present-but-unroutable `index`
// is an error, never a silent fall-through to the shared legacy DB (which would
// be the exact cross-org leak this feature closes).
package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
)

// indexNamespaceRe pins the coordinator-set namespace shape `org_<id>/<name>`:
// an `org_`-prefixed id segment of [A-Za-z0-9_-] (covers numeric ids and UUIDs),
// one `/`, then a name segment that must START with an alphanumeric (so a name
// can never be `.` / `..` / a dotfile-style traversal segment) and otherwise
// allows [A-Za-z0-9._-]. No other separators, no NUL, no backslash, no `..`.
var indexNamespaceRe = regexp.MustCompile(`^org_[A-Za-z0-9_-]+/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// indexesSubdir is the single source of truth for the per-node directory that
// holds namespaced index DBs, relative to the machine-convergent node config
// dir. Keeping it here (not duplicated at each cmd wiring site) means the
// convention can't drift between the worker and the CLI.
const indexesSubdir = "indexes"

// IndexesDirFor returns the base directory under which per-namespace index DBs
// live, given the machine-convergent node config dir. network.GetNodeConfigDir()
// is resolved by the cmd/worker layer and passed in — this leaf package never
// resolves it itself (the cross-context rule in CLAUDE.md). An empty
// nodeConfigDir yields an empty base, which downstream resolution treats as
// "namespaces unavailable" and fails closed.
func IndexesDirFor(nodeConfigDir string) string {
	if nodeConfigDir == "" {
		return ""
	}
	return filepath.Join(nodeConfigDir, indexesSubdir)
}

// resolveIndexNamespaceDBPath resolves a coordinator-set `index` namespace to the
// absolute path of its per-namespace SQLite DB, confined under indexesDir. It
// fails closed on an empty indexesDir (namespaces unavailable), a malformed
// namespace, or any path that resolves outside indexesDir.
func resolveIndexNamespaceDBPath(indexesDir, ns string) (string, error) {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return "", fmt.Errorf("index namespace is empty")
	}
	if indexesDir == "" {
		// Fail closed: without a base dir there is no confinement boundary, and a
		// silent fall-through to the shared legacy DB would leak one org's chunks
		// into another's index (the exact cross-org leak #1263 closes).
		return "", fmt.Errorf("index namespace %q requested but no indexes dir is configured on this node", ns)
	}
	if !indexNamespaceRe.MatchString(ns) {
		return "", fmt.Errorf("invalid index namespace %q (want org_<id>/<name>)", ns)
	}
	// The regex already forbids `.`/`..` segments and extra separators; the checks
	// below are defense in depth so a path that somehow escapes the shape rule is
	// still refused rather than written outside the indexes dir.
	abs := filepath.Clean(filepath.Join(indexesDir, ns+".db"))
	resolvedBase, err := resolveNearestAncestor(indexesDir)
	if err != nil {
		return "", fmt.Errorf("resolve indexes dir: %w", err)
	}
	resolvedTarget, err := resolveNearestAncestor(abs)
	if err != nil {
		return "", fmt.Errorf("resolve index path for namespace %q: %w", ns, err)
	}
	if !withinDir(resolvedBase, resolvedTarget) {
		return "", fmt.Errorf("index namespace %q resolves to %q, outside the indexes dir %q", ns, resolvedTarget, indexesDir)
	}
	return abs, nil
}

// ResolveIndexNamespaceDBPath exposes the per-namespace DB-path resolution (with
// its shape validation and path confinement) to callers outside this package —
// the rag service / `citadel rag --index` CLI — so a local surface lands on the
// SAME confined per-namespace DB a worker's namespaced FILE_INDEX writes.
func ResolveIndexNamespaceDBPath(indexesDir, ns string) (string, error) {
	return resolveIndexNamespaceDBPath(indexesDir, ns)
}

// openIndexStore opens the node-local index store for a FILE_INDEX /
// FILE_SEMANTIC_SEARCH job. When payloadIndex is non-empty it resolves (and
// confines) the per-namespace DB under indexesDir and creates its parent dir;
// otherwise it falls back to the legacy single DB (resolveIndexDBPath). It
// returns the opened store and the resolved DB path (for logging/diagnostics).
func openIndexStore(payloadIndex, indexesDir, dbPath, workspace string) (*nodeindex.Store, string, error) {
	resolved := resolveIndexDBPath(dbPath, workspace)
	if strings.TrimSpace(payloadIndex) != "" {
		nsPath, err := resolveIndexNamespaceDBPath(indexesDir, payloadIndex)
		if err != nil {
			return nil, "", err
		}
		// The org_<id>/ subdir under indexes/ won't exist on first use; create it
		// (0700, node-private) before Open, which does not create parents. The WAL
		// -wal/-shm sidecars land alongside the DB in this same confined dir.
		if err := os.MkdirAll(filepath.Dir(nsPath), 0o700); err != nil {
			return nil, "", fmt.Errorf("create index namespace dir: %w", err)
		}
		resolved = nsPath
	}
	store, err := nodeindex.Open(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("open node index: %w", err)
	}
	return store, resolved, nil
}

// resolveIndexMaxFileBytes returns the per-file read budget for FILE_INDEX.
// Precedence: payload `max_file_bytes` > CITADEL_INDEX_MAX_FILE_BYTES env >
// the maxIndexFileBytes default. A zero/negative/invalid value at any tier is
// ignored (falls through to the next tier), so a malformed override never
// disables the bound.
func resolveIndexMaxFileBytes(payload map[string]string) int64 {
	if v := atoi64Positive(payload["max_file_bytes"]); v > 0 {
		return v
	}
	if v := atoi64Positive(os.Getenv("CITADEL_INDEX_MAX_FILE_BYTES")); v > 0 {
		return v
	}
	return maxIndexFileBytes
}

// resolveIndexMaxChunks returns the per-file chunk cap for FILE_INDEX.
// Precedence: payload `max_chunks_per_file` > CITADEL_INDEX_MAX_CHUNKS_PER_FILE
// env > the maxChunksPerFile default.
func resolveIndexMaxChunks(payload map[string]string) int {
	if v := atoiPositive(payload["max_chunks_per_file"]); v > 0 {
		return v
	}
	if v := atoiPositive(os.Getenv("CITADEL_INDEX_MAX_CHUNKS_PER_FILE")); v > 0 {
		return v
	}
	return maxChunksPerFile
}

// atoiPositive parses s as an int, returning 0 on any error, empty input, or a
// non-positive value.
func atoiPositive(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// atoi64Positive parses s as an int64, returning 0 on any error, empty input, or
// a non-positive value.
func atoi64Positive(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
