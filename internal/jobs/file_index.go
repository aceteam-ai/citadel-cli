// internal/jobs/file_index.go
package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
)

// defaultEmbeddingModel is the TEI model used when a FILE_INDEX /
// FILE_SEMANTIC_SEARCH job does not specify one. It must match the model the
// node's TEI service actually serves (see services/compose/tei.yml). Overridable
// per-job via the "model" payload field or globally via CITADEL_EMBEDDING_MODEL.
const defaultEmbeddingModel = "gte-multilingual-base"

// maxIndexFileBytes caps the size of a single file that FILE_INDEX will read and
// embed. Larger files are skipped (recorded as skipped, not failed) to bound
// embedding cost and memory.
const maxIndexFileBytes = 1 << 20 // 1 MiB

// chunkTargetBytes is the approximate size of one embedded chunk. Chunking is
// paragraph-aware: paragraphs are accumulated until this budget is exceeded.
const chunkTargetBytes = 1000

// maxChunksPerFile bounds how many chunks a single file contributes, so a
// pathological file cannot dominate the index or the embedding batch.
const maxChunksPerFile = 200

// FileIndexHandler handles FILE_INDEX jobs. It walks a workspace path, computes
// each text file's content hash, and (re)embeds only files whose content changed
// since the last index, upserting their chunk vectors into the node-local index.
// Files previously indexed under the same root that no longer exist on disk are
// pruned.
//
// It is the node tier of aceteam#6087's two-tier index. Path validation and
// binary/noise-dir skipping mirror FileSearchHandler so the two stay consistent.
type FileIndexHandler struct {
	// WorkspaceDir is the sandbox root, used to validate the index path.
	WorkspaceDir string
	// DBPath is the node-local index database path. If empty, it is resolved
	// from CITADEL_INDEX_DB or a default beside the workspace.
	DBPath string
	// AllowOutsideWorkspace mirrors the read-handler relaxation flag.
	AllowOutsideWorkspace bool
	// embeddingOp is an optional hermetic test seam. Production always creates
	// one fresh operation per Execute call below.
	embeddingOp *teiEmbeddingOperation
}

// NewFileIndexHandler creates a FileIndexHandler rooted at workspace.
func NewFileIndexHandler(workspace, dbPath string) *FileIndexHandler {
	return &FileIndexHandler{WorkspaceDir: workspace, DBPath: dbPath}
}

// Execute walks the requested path and incrementally updates the node-local
// index.
//
// Payload fields:
//   - path: root directory (or single file) to index; workspace-relative or
//     absolute. Required.
//   - model: TEI embedding model. Optional; defaults to CITADEL_EMBEDDING_MODEL
//     or defaultEmbeddingModel.
//   - file_pattern: optional glob to restrict indexed filenames (e.g. "*.md").
//   - prune: "true" (default) removes index entries for files that no longer
//     exist under the indexed root; "false" leaves them. Note: prune compares
//     against the files this run actually visited, so combining prune with a
//     narrowing file_pattern will also drop previously-indexed files that no
//     longer match the pattern. The blast radius is only the recreatable index
//     (never source data); the default whole-root, no-pattern run is safe.
func (h *FileIndexHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}
	model := job.Payload["model"]
	if model == "" {
		model = defaultEmbeddingModel
		if env := os.Getenv("CITADEL_EMBEDDING_MODEL"); env != "" {
			model = env
		}
	}
	filePattern := job.Payload["file_pattern"]
	prune := job.Payload["prune"] != "false"

	validated, err := ValidateReadPath(h.WorkspaceDir, path, h.AllowOutsideWorkspace)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	store, err := nodeindex.Open(resolveIndexDBPath(h.DBPath, h.WorkspaceDir))
	if err != nil {
		return nil, fmt.Errorf("open node index: %w", err)
	}
	defer store.Close()

	ctx.Log("info", "     - [Job %s] FILE_INDEX %s model=%q pattern=%q", job.ID, validated, model, filePattern)

	// Cheap pre-count of candidate files so progress can report done/total. It
	// only stats entries (no reads/embeds) and honors cancellation, so it is a
	// small fraction of the embedding cost. It over-counts (a candidate may later
	// be skipped as binary/unchanged/too-large), but gives a stable denominator.
	total := countIndexCandidates(validated, filePattern, ctx)

	// Enumerate candidate files first so pruning can compare against on-disk state.
	seen := make(map[string]struct{})
	var indexed, skipped, failed, embedded, chunksUpserted, processed int
	dim := 0
	cancelled := false
	embedOp := h.embeddingOp
	if embedOp == nil {
		embedOp = newTEIEmbeddingOperation(teiBaseURL())
	}
	// emitProgress reports the current walk position, throttled by the caller's
	// wiring (jobs.NewThrottledProgress). Counts/basename only — never a full path.
	emitProgress := func(current string, final bool) {
		ctx.EmitProgress(ProgressEvent{
			Stage:   indexStageWalk,
			Done:    processed,
			Total:   total,
			Unit:    "files",
			Current: current,
			Final:   final,
			Counts: map[string]int{
				"indexed": indexed,
				"skipped": skipped,
				"failed":  failed,
			},
		})
	}

	walkErr := filepath.WalkDir(validated, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == validated {
				return err
			}
			return nil // skip unreadable entries
		}
		// Cooperative (graceful) cancel is checked at the file boundary ONLY, so
		// the file currently being embedded/upserted finishes and commits before
		// we stop — "cancel commits the current file" (aceteam#10876 C1). SkipAll
		// stops the walk WITHOUT an error, so a partial, consistent index remains.
		if ctx.CancelRequestedNow() {
			cancelled = true
			return filepath.SkipAll
		}
		// A HARD ctx cancellation (watchdog deadline / worker shutdown) remains
		// fatal exactly as before: it returns the error and leaves the prior index
		// untouched (prune is skipped below on any non-clean exit).
		if err := ctx.Context().Err(); err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "__pycache__" || name == ".venv" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filePattern != "" {
			matched, perr := filepath.Match(filePattern, d.Name())
			if perr != nil || !matched {
				return nil
			}
		}
		// A candidate file: count it and surface progress (throttled downstream).
		processed++
		emitProgress(filepath.Base(p), false)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxIndexFileBytes {
			skipped++
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if len(content) == 0 || isBinaryContent(content) {
			skipped++
			return nil
		}

		seen[p] = struct{}{}
		hash := hashContent(content)

		prev, wasIndexed, herr := store.FileHash(p)
		if herr != nil {
			return fmt.Errorf("read prior hash for %s: %w", p, herr)
		}
		if wasIndexed && prev == hash {
			skipped++
			return nil // unchanged
		}

		chunks := chunkText(string(content))
		if len(chunks) == 0 {
			skipped++
			return nil
		}
		vecs, err := embedTexts(ctx.Context(), embedOp, model, chunks)
		if err != nil {
			// A graceful cancel can land mid-embed (local Ctrl-C, where the soft
			// signal and the hard ctx share a source): stop gracefully and keep
			// the files committed so far rather than reporting a fatal error.
			if ctx.CancelRequestedNow() {
				cancelled = true
				return filepath.SkipAll
			}
			if ctxErr := ctx.Context().Err(); ctxErr != nil {
				return fmt.Errorf("embed %s: %w", p, ctxErr)
			}
			if errors.Is(err, errTEINotReady) {
				// Readiness belongs to the entire operation. A dead service is
				// fatal, while individual requests against a ready service may
				// fail without abandoning the remaining files.
				return fmt.Errorf("embed %s: %w", p, err)
			}
			failed++
			ctx.Log("error", "     - [Job %s] Failed to embed %s (%s); continuing", job.ID, p, teiFailureCategory(err))
			return nil
		}
		// No cooperative-cancel check between a successful embed and its upsert:
		// the current file must COMMIT once embedded (the C1 guarantee). A hard
		// ctx cancellation here is still fatal before the write (unchanged).
		if err := ctx.Context().Err(); err != nil {
			return fmt.Errorf("embed %s: %w", p, err)
		}
		idxChunks := make([]nodeindex.Chunk, len(chunks))
		for i := range chunks {
			idxChunks[i] = nodeindex.Chunk{Index: i, Text: chunks[i], Embedding: vecs[i]}
		}
		if len(vecs) > 0 {
			dim = len(vecs[0])
		}
		if err := store.UpsertFile(p, hash, info.ModTime().Unix(), info.Size(), model, dim, idxChunks); err != nil {
			return fmt.Errorf("upsert %s: %w", p, err)
		}
		indexed++
		embedded += len(chunks)
		chunksUpserted += len(idxChunks)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("index walk failed: %w", walkErr)
	}
	// A non-cancelled walk must not have been interrupted by a hard ctx cancel.
	// On a cooperative cancel we skip this so the partial result is returned
	// rather than a fatal error (even when the local Ctrl-C path also cancelled
	// the hard ctx).
	if !cancelled {
		if err := ctx.Context().Err(); err != nil {
			return nil, fmt.Errorf("index walk cancelled: %w", err)
		}
	}

	// Prune entries whose files vanished from disk under the indexed root. Only
	// on a COMPLETE walk: on a cancelled walk `seen` is partial by construction,
	// so pruning against it would delete every not-yet-visited file's index entry
	// (data loss). A partial index is left consistent and a re-dispatch resumes
	// via the content-hash skip.
	removed := 0
	if !cancelled && prune {
		known, err := store.IndexedPaths()
		if err != nil {
			return nil, fmt.Errorf("list indexed paths: %w", err)
		}
		rootPrefix := validated + string(filepath.Separator)
		for known_path := range known {
			if err := ctx.Context().Err(); err != nil {
				return nil, fmt.Errorf("index prune cancelled: %w", err)
			}
			if known_path != validated && !strings.HasPrefix(known_path, rootPrefix) {
				continue // outside the indexed root; leave it
			}
			if _, present := seen[known_path]; present {
				continue
			}
			if err := store.DeleteFile(known_path); err != nil {
				return nil, fmt.Errorf("prune %s: %w", known_path, err)
			}
			removed++
		}
	}
	if !cancelled {
		if err := ctx.Context().Err(); err != nil {
			return nil, fmt.Errorf("index operation cancelled: %w", err)
		}
	}

	status := "completed"
	checkpoint := "complete"
	if cancelled {
		status = "cancelled"
		checkpoint = "partial"
	}
	emitProgress("", true)

	out := map[string]any{
		"files_seen":      processed,
		"files_indexed":   indexed,
		"files_skipped":   skipped,
		"files_failed":    failed,
		"files_removed":   removed,
		"chunks_upserted": chunksUpserted,
		"chunks_embedded": embedded,
		"model":           model,
		"dim":             dim,
		"status":          status,
		"checkpoint":      checkpoint,
	}
	return json.Marshal(out)
}

// indexStageWalk is the progress stage label for the file walk. C1 has a single
// walk-and-embed stage; the design's plan/extract/embed/upsert stages arrive
// with later slices.
const indexStageWalk = "index"

// countIndexCandidates walks root and counts files matching filePattern (and not
// under a skipped noise directory) WITHOUT reading them, so FILE_INDEX progress
// can report done/total. It stops early (returning the partial count) when a
// cooperative or hard cancellation is observed, so it never blocks the graceful
// stop; a best-effort denominator is fine.
func countIndexCandidates(root, filePattern string, ctx JobContext) int {
	count := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if ctx.CancelRequestedNow() || ctx.Context().Err() != nil {
			return filepath.SkipAll
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "__pycache__" || name == ".venv" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filePattern != "" {
			matched, perr := filepath.Match(filePattern, d.Name())
			if perr != nil || !matched {
				return nil
			}
		}
		count++
		return nil
	})
	return count
}

// hashContent returns the full lowercase hex SHA-256 of content. The two-tier
// index dedups across the central and node tiers by this full hash (aceteam#6087
// standardizes on full sha256, replacing memory's 16-hex truncation).
func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// chunkText splits text into paragraph-aware chunks of roughly chunkTargetBytes.
// Paragraphs (blank-line separated) are accumulated until the target is
// exceeded; a single paragraph larger than the target is hard-split so no chunk
// grows unbounded. Returns at most maxChunksPerFile chunks.
func chunkText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	paras := splitParagraphs(text)

	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	for _, para := range paras {
		for len(para) > chunkTargetBytes {
			// Hard-split an oversized paragraph on a rune boundary.
			cut := runeSafeCut(para, chunkTargetBytes)
			flush()
			chunks = append(chunks, strings.TrimSpace(para[:cut]))
			para = para[cut:]
			if len(chunks) >= maxChunksPerFile {
				return chunks[:maxChunksPerFile]
			}
		}
		if cur.Len() > 0 && cur.Len()+len(para) > chunkTargetBytes {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(para)
		if len(chunks) >= maxChunksPerFile {
			return chunks[:maxChunksPerFile]
		}
	}
	flush()
	if len(chunks) > maxChunksPerFile {
		chunks = chunks[:maxChunksPerFile]
	}
	return chunks
}

// splitParagraphs splits on blank lines, dropping empty paragraphs.
func splitParagraphs(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// runeSafeCut returns a cut index <= maxBytes that does not split a UTF-8 rune.
func runeSafeCut(s string, maxBytes int) int {
	if maxBytes >= len(s) {
		return len(s)
	}
	cut := maxBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return maxBytes // no rune boundary found; fall back to a hard cut
	}
	return cut
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// 10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}

// resolveIndexDBPath returns the node-local index database path. Precedence:
// explicit dbPath > CITADEL_INDEX_DB env > "index.db" beside the workspace's
// parent (default ~/citadel-node/index.db when workspace is ~/citadel-node/workspace).
func resolveIndexDBPath(dbPath, workspace string) string {
	if dbPath != "" {
		return dbPath
	}
	if env := os.Getenv("CITADEL_INDEX_DB"); env != "" {
		return env
	}
	if workspace != "" {
		return filepath.Join(filepath.Dir(filepath.Clean(workspace)), "index.db")
	}
	return "index.db"
}

// DefaultEmbeddingModel is the TEI model used for node-local indexing/search
// when none is specified. Exported so local surfaces (the rag package, the
// `citadel rag` CLI) report the same model the job handlers embed with.
const DefaultEmbeddingModel = defaultEmbeddingModel

// ResolveIndexDBPath exposes the node-local index db-path resolution so callers
// outside this package (the rag service, CLI) land on the SAME index.db a
// running worker's FILE_INDEX writes to. Precedence: explicit dbPath >
// CITADEL_INDEX_DB > "index.db" beside the workspace's parent.
func ResolveIndexDBPath(dbPath, workspace string) string {
	return resolveIndexDBPath(dbPath, workspace)
}

// ResolveEmbeddingModel returns the effective embedding model for a given
// (optional) override, applying the same defaulting the FILE_INDEX /
// FILE_SEMANTIC_SEARCH handlers use: override > CITADEL_EMBEDDING_MODEL >
// DefaultEmbeddingModel. Keeping this in one place stops the local surface from
// drifting from the model the handlers actually embed with.
func ResolveEmbeddingModel(override string) string {
	if override != "" {
		return override
	}
	if env := os.Getenv("CITADEL_EMBEDDING_MODEL"); env != "" {
		return env
	}
	return defaultEmbeddingModel
}

// atoiDefault parses s as an int, returning def on any error or empty input.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Ensure FileIndexHandler implements JobHandler.
var _ JobHandler = (*FileIndexHandler)(nil)
