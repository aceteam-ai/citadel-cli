package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIndexesDirFor(t *testing.T) {
	if got := IndexesDirFor(""); got != "" {
		t.Fatalf("empty node config dir should yield empty indexes dir, got %q", got)
	}
	want := filepath.Join("/node", "indexes")
	if got := IndexesDirFor("/node"); got != want {
		t.Fatalf("IndexesDirFor = %q, want %q", got, want)
	}
}

func TestResolveIndexNamespaceDBPath_Valid(t *testing.T) {
	indexesDir := t.TempDir()
	got, err := resolveIndexNamespaceDBPath(indexesDir, "org_42/docs")
	if err != nil {
		t.Fatalf("valid namespace rejected: %v", err)
	}
	want := filepath.Join(indexesDir, "org_42", "docs.db")
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
	// UUID-style id and dotted/hyphenated name are accepted.
	if _, err := resolveIndexNamespaceDBPath(indexesDir, "org_a1b2-c3d4/my.index_v2"); err != nil {
		t.Fatalf("valid uuid/dotted namespace rejected: %v", err)
	}
}

// TestResolveIndexNamespaceDBPath_RefusesTraversal proves a payload-supplied
// namespace can never escape the indexes dir (the C2 path-confinement invariant,
// mirroring the instance state-volume resolver, citadel-cli#1163).
func TestResolveIndexNamespaceDBPath_RefusesTraversal(t *testing.T) {
	indexesDir := t.TempDir()
	for _, ns := range []string{
		"",              // empty
		"../../etc/x",   // parent traversal, no org_ prefix
		"/etc/x",        // absolute
		"org_1/../../x", // traversal inside name
		"org_1/..",      // name is ..
		"org_1/.",       // name is .
		"org_1//x",      // double separator
		`org_1\x`,       // backslash
		"org_1",         // missing /name
		"org_1/",        // empty name
		"notorg/x",      // wrong id prefix
		"org_1/a/b",     // too many segments
		"org_ 1/x",      // space in id
		"org_1/.hidden", // name starts with dot
		"org_1/x\x00y",  // embedded NUL
	} {
		if _, err := resolveIndexNamespaceDBPath(indexesDir, ns); err == nil {
			t.Errorf("namespace %q was accepted; want refusal", ns)
		}
	}
}

// TestResolveIndexNamespaceDBPath_RefusesSymlinkEscape pins that a symlinked
// org_ subdir pointing outside the indexes dir is refused — the symlink-resolved
// confinement check, not just a lexical prefix test (citadel-cli#1163).
func TestResolveIndexNamespaceDBPath_RefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	indexesDir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(indexesDir, "org_evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if got, err := resolveIndexNamespaceDBPath(indexesDir, "org_evil/x"); err == nil {
		t.Fatalf("symlinked org dir escaping indexes dir was accepted (resolved %q); want refusal", got)
	}
}

// TestResolveIndexNamespaceDBPath_FailsClosedWithoutIndexesDir pins that a
// namespace request with no configured indexes dir is an ERROR, never a silent
// fall-through to the shared legacy DB (the cross-org leak #1263 closes).
func TestResolveIndexNamespaceDBPath_FailsClosedWithoutIndexesDir(t *testing.T) {
	if _, err := resolveIndexNamespaceDBPath("", "org_1/docs"); err == nil {
		t.Fatal("namespace with empty indexes dir was accepted; want fail-closed error")
	}
}

// TestOpenIndexStore_DefaultAbsentUsesLegacyDB pins that an absent `index`
// routes to the EXACT legacy resolveIndexDBPath result (back-compat), never a
// namespace DB — even when an indexes dir is configured.
func TestOpenIndexStore_DefaultAbsentUsesLegacyDB(t *testing.T) {
	t.Setenv("CITADEL_INDEX_DB", "")
	// An explicit dbPath in an existing dir; resolveIndexDBPath returns it
	// verbatim, which is exactly what an absent-index open must land on.
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	wantLegacy := resolveIndexDBPath(dbPath, "")

	store, resolved, err := openIndexStore("", t.TempDir(), dbPath, "")
	if err != nil {
		t.Fatalf("openIndexStore (no index): %v", err)
	}
	defer store.Close()
	if resolved != wantLegacy || resolved != dbPath {
		t.Fatalf("absent index resolved to %q, want legacy %q", resolved, wantLegacy)
	}
}

// TestOpenIndexStore_NamespaceCreatesConfinedDB pins that a namespaced request
// opens the confined per-namespace DB (and creates its parent dir).
func TestOpenIndexStore_NamespaceCreatesConfinedDB(t *testing.T) {
	indexesDir := t.TempDir()
	store, resolved, err := openIndexStore("org_7/team", indexesDir, "", "")
	if err != nil {
		t.Fatalf("openIndexStore (namespace): %v", err)
	}
	defer store.Close()
	want := filepath.Join(indexesDir, "org_7", "team.db")
	if resolved != want {
		t.Fatalf("namespace resolved to %q, want %q", resolved, want)
	}
	if _, err := os.Stat(filepath.Join(indexesDir, "org_7")); err != nil {
		t.Fatalf("namespace parent dir not created: %v", err)
	}
}

// TestOpenIndexStore_FreshNodeCreatesIndexesDir pins the most common production
// path: the first namespaced job on a fresh node where <node_config>/indexes/
// does not yet exist. resolveNearestAncestor must walk past the absent leaf,
// confinement must still hold, and the org_<id>/ subdir must be created.
func TestOpenIndexStore_FreshNodeCreatesIndexesDir(t *testing.T) {
	indexesDir := filepath.Join(t.TempDir(), "indexes") // deliberately NOT created
	store, resolved, err := openIndexStore("org_1/docs", indexesDir, "", "")
	if err != nil {
		t.Fatalf("openIndexStore on fresh node: %v", err)
	}
	defer store.Close()
	want := filepath.Join(indexesDir, "org_1", "docs.db")
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
	if _, err := os.Stat(filepath.Join(indexesDir, "org_1")); err != nil {
		t.Fatalf("org_1 subdir not created on fresh node: %v", err)
	}
}

func TestOpenIndexStore_NamespaceTraversalRefused(t *testing.T) {
	if _, _, err := openIndexStore("../escape", t.TempDir(), "", ""); err == nil {
		t.Fatal("traversal namespace accepted by openIndexStore; want refusal")
	}
}

func TestResolveIndexCaps(t *testing.T) {
	t.Setenv("CITADEL_INDEX_MAX_FILE_BYTES", "")
	t.Setenv("CITADEL_INDEX_MAX_CHUNKS_PER_FILE", "")

	// Defaults when nothing set.
	if got := resolveIndexMaxFileBytes(map[string]string{}); got != maxIndexFileBytes {
		t.Fatalf("default max_file_bytes = %d, want %d", got, maxIndexFileBytes)
	}
	if got := resolveIndexMaxChunks(map[string]string{}); got != maxChunksPerFile {
		t.Fatalf("default max_chunks = %d, want %d", got, maxChunksPerFile)
	}

	// Env overrides default.
	t.Setenv("CITADEL_INDEX_MAX_FILE_BYTES", "4096")
	t.Setenv("CITADEL_INDEX_MAX_CHUNKS_PER_FILE", "7")
	if got := resolveIndexMaxFileBytes(map[string]string{}); got != 4096 {
		t.Fatalf("env max_file_bytes = %d, want 4096", got)
	}
	if got := resolveIndexMaxChunks(map[string]string{}); got != 7 {
		t.Fatalf("env max_chunks = %d, want 7", got)
	}

	// Payload wins over env.
	payload := map[string]string{"max_file_bytes": "8192", "max_chunks_per_file": "3"}
	if got := resolveIndexMaxFileBytes(payload); got != 8192 {
		t.Fatalf("payload max_file_bytes = %d, want 8192", got)
	}
	if got := resolveIndexMaxChunks(payload); got != 3 {
		t.Fatalf("payload max_chunks = %d, want 3", got)
	}

	// Invalid/non-positive values fall through to the next tier (never disable).
	bad := map[string]string{"max_file_bytes": "-1", "max_chunks_per_file": "0"}
	if got := resolveIndexMaxFileBytes(bad); got != 4096 {
		t.Fatalf("invalid payload max_file_bytes should fall through to env 4096, got %d", got)
	}
	if got := resolveIndexMaxChunks(bad); got != 7 {
		t.Fatalf("invalid payload max_chunks should fall through to env 7, got %d", got)
	}
}

func TestChunkTextLimited_ReportsCapping(t *testing.T) {
	var sb []byte
	for i := 0; i < 10; i++ {
		sb = append(sb, []byte("paragraph number "+string(rune('0'+i))+"\n\n")...)
	}
	chunks, capped := chunkTextLimited(string(sb), 10, 3)
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3 (capped)", len(chunks))
	}
	if !capped {
		t.Fatal("capped = false, want true when the cap truncates output")
	}
	// Under the cap: not capped.
	_, capped2 := chunkTextLimited("one small paragraph", 1000, 200)
	if capped2 {
		t.Fatal("capped = true for a single small chunk, want false")
	}
}
