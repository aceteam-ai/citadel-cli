// cmd/run_job_template_ops.go
//
// cmd-side live adapter for the RUN_JOB_TEMPLATE worker handler (citadel-cli#1149,
// aceteam#10288). The worker handler enforces the security invariants (per-node
// stream gate, manifest-hash recompute, builtin-only kind); this adapter does the
// side effects the worker package must not import: dispatch to a compiled-in
// builtin, write outputs under the node workspace, hash inputs/outputs, and sign
// the AEP receipt with the node key. Same seam shape as liveAppOps / liveExposeOps.
//
// Production builtins register through the exported
// RegisterBuiltinTemplateRunner seam from their own files, not here.
package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// BuiltinTemplateRunner is a compiled-in template handler. It receives the
// template's params, the on-disk absolute paths of per-run staged input
// snapshots, and an absolute per-run output directory (already inside the node
// workspace), and returns the paths of the outputs it wrote, RELATIVE to outDir.
// It must write only under outDir; the caller opens every returned path through
// an os.Root anchored at that exact directory.
type BuiltinTemplateRunner func(ctx context.Context, params json.RawMessage, inputs []string, outDir string) (outputRelPaths []string, err error)

var (
	builtinTemplateRunnersMu sync.RWMutex
	builtinTemplateRunners   = map[string]BuiltinTemplateRunner{}
)

// RegisterBuiltinTemplateRunner registers a compiled-in builtin under the name a
// template's runner.handler references. Real builtins (audio-mix,
// papercraft-render) register themselves from their own files.
func RegisterBuiltinTemplateRunner(name string, fn BuiltinTemplateRunner) {
	builtinTemplateRunnersMu.Lock()
	defer builtinTemplateRunnersMu.Unlock()
	builtinTemplateRunners[name] = fn
}

func lookupBuiltinTemplateRunner(name string) (BuiltinTemplateRunner, bool) {
	builtinTemplateRunnersMu.RLock()
	defer builtinTemplateRunnersMu.RUnlock()
	fn, ok := builtinTemplateRunners[name]
	return fn, ok
}

// liveTemplateRunOps implements worker.TemplateRunOps against the node workspace
// and the compiled-in builtin registry.
type liveTemplateRunOps struct {
	workspaceDir string
	nodeID       string
}

const (
	// These are code-owned execution-policy limits, not feature flags. The input
	// ceiling permits sizable media assets without allowing one delivery to stage
	// an unbounded snapshot. The output ceiling accommodates the two hour-scale
	// videos or one PCM mix produced by today's approved builtins.
	templateMaxInputBytes    int64 = 4 << 30
	templateMaxOutputFiles         = 16
	templateMaxOutputEntries       = 64
	templateMaxOutputBytes   int64 = 8 << 30

	// Lazy node:path consumers need successful outputs after the job completes.
	// Seven days covers delayed collection/retry without making artifacts
	// permanent. A sweep runs before each template attempt.
	templateOutputRetention = 7 * 24 * time.Hour
	templateCompleteMarker  = ".citadel-complete-v1"
	templateCompleteBody    = "complete\n"
)

var templateGCMu sync.Mutex

// Run dispatches an already-verified template request to its builtin, collects
// the outputs as node-backed references, and (when enabled) attaches a signed
// receipt. The worker handler has already checked the per-node gate, the manifest
// hash, and that the runner is a builtin with a non-empty handler name.
func (o liveTemplateRunOps) Run(ctx context.Context, req worker.TemplateRunRequest) (*worker.TemplateRunResult, error) {
	start := time.Now()

	if strings.TrimSpace(o.workspaceDir) == "" {
		return nil, fmt.Errorf("workspace directory is not configured")
	}

	runner, err := worker.ValidateTemplateRunRequest(req, o.nodeID)
	if err != nil {
		return nil, err
	}
	req.Params, err = jobs.NormalizeTemplateParams(req.Params)
	if err != nil {
		return nil, err
	}
	fn, ok := lookupBuiltinTemplateRunner(runner.Handler)
	if !ok {
		return nil, fmt.Errorf("%w: %q", worker.ErrTemplateUnknownHandler, runner.Handler)
	}
	inputFiles, err := jobs.ParseTemplateInputFiles(req.InputFiles, o.nodeID)
	if err != nil {
		return nil, err
	}

	workspaceDir, err := filepath.Abs(o.workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	workspaceDir, err = filepath.EvalSymlinks(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	workspaceRoot, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	defer workspaceRoot.Close()
	if err := pruneRetainedTemplateAttempts(workspaceRoot, time.Now(), templateOutputRetention); err != nil {
		return nil, fmt.Errorf("prune retained template outputs: %w", err)
	}

	// Each delivery attempt owns a unique namespace. executeWithDeadline can
	// abandon a handler that has not observed cancellation yet; a later retry
	// must never remove or reuse paths the old goroutine may still be touching.
	attempt, err := newTemplateAttemptSegment()
	if err != nil {
		return nil, err
	}
	relRun := filepath.Join("templates", sanitizeTemplateSegment(req.TemplateKey), sanitizeTemplateSegment(req.JobID), attempt)
	relInputs := filepath.Join(relRun, "inputs")
	relOut := filepath.Join(relRun, "out")
	if err := workspaceRoot.MkdirAll(relInputs, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("create input staging directory: %w", err), removeTemplateAttempt(workspaceRoot, relRun))
	}
	if err := workspaceRoot.MkdirAll(relOut, 0o755); err != nil {
		return nil, errors.Join(fmt.Errorf("create output directory: %w", err), removeTemplateAttempt(workspaceRoot, relRun))
	}
	inputRoot, err := workspaceRoot.OpenRoot(relInputs)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open input staging root: %w", err), removeTemplateAttempt(workspaceRoot, relRun))
	}
	inputs, inputRels, inputHashes, err := o.resolveInputs(ctx, inputFiles, workspaceRoot, inputRoot, filepath.Join(workspaceRoot.Name(), relInputs), templateMaxInputBytes)
	if err != nil {
		return nil, errors.Join(err, cleanupTemplateAttempt(workspaceRoot, inputRoot, nil, relRun))
	}
	outRoot, err := workspaceRoot.OpenRoot(relOut)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open output root: %w", err), cleanupTemplateAttempt(workspaceRoot, inputRoot, nil, relRun))
	}

	outRels, runErr := fn(ctx, req.Params, inputs, filepath.Join(workspaceRoot.Name(), relOut))
	if runErr != nil {
		return nil, errors.Join(fmt.Errorf("builtin %q: %w", runner.Handler, runErr), cleanupTemplateAttempt(workspaceRoot, inputRoot, outRoot, relRun))
	}
	if err := verifyStagedTemplateInputs(ctx, inputRoot, inputRels, inputHashes); err != nil {
		return nil, errors.Join(err, cleanupTemplateAttempt(workspaceRoot, inputRoot, outRoot, relRun))
	}

	outputs, outputDigests, err := o.collectOutputs(ctx, outRoot, relOut, outRels, templateMaxOutputFiles, templateMaxOutputEntries, templateMaxOutputBytes)
	if err != nil {
		return nil, errors.Join(err, cleanupTemplateAttempt(workspaceRoot, inputRoot, outRoot, relRun))
	}
	if err := finishTemplateAttempt(workspaceRoot, inputRoot, outRoot, relRun, relInputs); err != nil {
		return nil, err
	}

	res := &worker.TemplateRunResult{
		Outputs:     outputs,
		DurationMs:  time.Since(start).Milliseconds(),
		InputHashes: inputHashes,
	}
	res.Receipt = signRunJobTemplateReceipt(req, outputDigests)
	return res, nil
}

// resolveInputs opens every source through the already-open workspace root,
// copies it into a private per-run snapshot, and hashes the bytes while writing
// that snapshot. Builtins consume only the staged paths, so InputHashes describe
// the bytes they see rather than an earlier by-name read of a mutable source.
func (o liveTemplateRunOps) resolveInputs(ctx context.Context, files []jobs.TemplateInputFile, workspaceRoot, inputRoot *os.Root, inputDir string, maxBytes int64) ([]string, []string, []string, error) {
	paths := make([]string, 0, len(files))
	rels := make([]string, 0, len(files))
	hashes := make([]string, 0, len(files))
	stagedHashes := make(map[string]string, len(files))
	stagedSizes := make(map[string]int64, len(files))
	var declaredBytes int64
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		rel := filepath.FromSlash(f.NodePath)
		if digest, ok := stagedHashes[rel]; ok {
			size := stagedSizes[rel]
			if size > maxBytes-declaredBytes {
				return nil, nil, nil, fmt.Errorf("declared input bytes exceed %d-byte limit", maxBytes)
			}
			declaredBytes += size
			paths = append(paths, filepath.Join(inputDir, rel))
			rels = append(rels, rel)
			hashes = append(hashes, digest)
			continue
		}
		src, err := workspaceRoot.Open(rel)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open input %q: %w", f.NodePath, err)
		}
		info, statErr := src.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil, nil, nil, errors.Join(
				fmt.Errorf("input file %q is not present on this node", f.NodePath),
				wrapTemplateError(statErr, "stat input %q", f.NodePath),
				closeTemplateRootFile(src, "input source"),
			)
		}
		if info.Size() < 0 || info.Size() > maxBytes-declaredBytes {
			return nil, nil, nil, errors.Join(
				fmt.Errorf("declared input bytes exceed %d-byte limit at %q", maxBytes, f.NodePath),
				closeTemplateRootFile(src, "input source"),
			)
		}
		parent := filepath.Dir(rel)
		if parent != "." {
			if err := inputRoot.MkdirAll(parent, 0o700); err != nil {
				return nil, nil, nil, errors.Join(
					fmt.Errorf("create staging directory for input %q: %w", f.NodePath, err),
					closeTemplateRootFile(src, "input source"),
				)
			}
		}
		dst, err := inputRoot.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, nil, nil, errors.Join(
				fmt.Errorf("stage input %q: %w", f.NodePath, err),
				closeTemplateRootFile(src, "input source"),
			)
		}
		h := sha256.New()
		copied, copyErr := copyTemplateWithContextLimit(ctx, io.MultiWriter(dst, h), src, maxBytes-declaredBytes)
		closeSrcErr := src.Close()
		chmodErr := dst.Chmod(0o400)
		closeDstErr := dst.Close()
		if stageErr := errors.Join(
			wrapTemplateError(copyErr, "copy input %q", f.NodePath),
			wrapTemplateError(closeSrcErr, "close input %q", f.NodePath),
			wrapTemplateError(chmodErr, "make staged input %q read-only", f.NodePath),
			wrapTemplateError(closeDstErr, "close staged input %q", f.NodePath),
		); stageErr != nil {
			return nil, nil, nil, stageErr
		}
		digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
		stagedHashes[rel] = digest
		stagedSizes[rel] = copied
		declaredBytes += copied
		paths = append(paths, filepath.Join(inputDir, rel))
		rels = append(rels, rel)
		hashes = append(hashes, digest)
	}
	return paths, rels, hashes, nil
}

// verifyStagedTemplateInputs detects a builtin that changed one of its staged
// inputs. Together with copy-time hashing, this makes the reported digest the
// digest of the stable snapshot available throughout execution.
func verifyStagedTemplateInputs(ctx context.Context, inputRoot *os.Root, rels, hashes []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, rel := range rels {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := inputRoot.Open(rel)
		if err != nil {
			return fmt.Errorf("reopen staged input %q: %w", filepath.ToSlash(rel), err)
		}
		digest, _, hashErr := sha256OpenFile(ctx, f)
		closeErr := f.Close()
		if hashErr != nil {
			return errors.Join(
				fmt.Errorf("rehash staged input %q: %w", filepath.ToSlash(rel), hashErr),
				wrapTemplateError(closeErr, "close staged input %q", filepath.ToSlash(rel)),
			)
		}
		if closeErr != nil {
			return fmt.Errorf("close staged input %q: %w", filepath.ToSlash(rel), closeErr)
		}
		if got := "sha256:" + digest; got != hashes[i] {
			return fmt.Errorf("staged input %q changed while builtin was running", filepath.ToSlash(rel))
		}
	}
	return nil
}

// collectOutputs opens every builtin-returned path relative to the already-open
// per-run output root. os.Root follows only symlinks that remain beneath that
// root and opens the validated target directly, closing both the sibling-path
// gap and the validate-then-open race.
func (o liveTemplateRunOps) collectOutputs(ctx context.Context, outRoot *os.Root, relOut string, outRels []string, maxFiles, maxEntries int, maxBytes int64) ([]worker.TemplateOutput, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateTemplateOutputTree(outRoot, outRels, maxFiles, maxEntries, maxBytes); err != nil {
		return nil, nil, err
	}
	outputs := make([]worker.TemplateOutput, 0, len(outRels))
	digests := make([]string, 0, len(outRels))
	remainingBytes := maxBytes
	for _, rel := range outRels {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if rel == "" || rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) || filepath.Clean(rel) != rel {
			return nil, nil, fmt.Errorf("output %q is not a canonical path beneath the per-run output directory", rel)
		}
		f, err := outRoot.Open(rel)
		if err != nil {
			return nil, nil, fmt.Errorf("open output %q beneath the per-run output directory: %w", rel, err)
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil, nil, errors.Join(
				fmt.Errorf("output %q was not written as a file", rel),
				wrapTemplateError(statErr, "stat output %q", rel),
				closeTemplateRootFile(f, "output"),
			)
		}
		digest, size, hashErr := sha256OpenFileLimit(ctx, f, remainingBytes)
		closeErr := f.Close()
		if hashErr != nil {
			return nil, nil, errors.Join(
				fmt.Errorf("hash output %q: %w", rel, hashErr),
				wrapTemplateError(closeErr, "close output %q", rel),
			)
		}
		if closeErr != nil {
			return nil, nil, fmt.Errorf("close output %q: %w", rel, closeErr)
		}
		remainingBytes -= size
		outputs = append(outputs, worker.TemplateOutput{
			Path:   filepath.ToSlash(filepath.Join(relOut, rel)),
			SHA256: "sha256:" + digest,
			Bytes:  size,
		})
		digests = append(digests, "sha256:"+digest)
	}
	return outputs, digests, nil
}

// validateTemplateOutputTree bounds what will be retained, not merely what a
// builtin reports. Otherwise an unreported file could bypass both caps and live
// forever beside an accepted output. The total-entry cap counts directories as
// well as files so empty-directory/inode growth cannot bypass the byte and file
// ceilings. Symlinks and special files are rejected so counting and later lazy
// reads have one unambiguous regular-file meaning.
func validateTemplateOutputTree(outRoot *os.Root, outRels []string, maxFiles, maxEntries int, maxBytes int64) error {
	if len(outRels) > maxFiles {
		return fmt.Errorf("builtin reported %d outputs; limit is %d", len(outRels), maxFiles)
	}
	want := make(map[string]struct{}, len(outRels))
	for _, rel := range outRels {
		if rel == "" || rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) || filepath.Clean(rel) != rel {
			return fmt.Errorf("output %q is not a canonical path beneath the per-run output directory", rel)
		}
		rel = filepath.ToSlash(rel)
		if _, duplicate := want[rel]; duplicate {
			return fmt.Errorf("output %q was reported more than once", rel)
		}
		want[rel] = struct{}{}
	}

	seen := make(map[string]struct{}, len(outRels))
	var fileCount int
	var entryCount int
	var total int64
	err := fs.WalkDir(outRoot.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		entryCount++
		if entryCount > maxEntries {
			return fmt.Errorf("output tree contains %d entries; limit is %d", entryCount, maxEntries)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("output tree entry %q is not a regular file", name)
		}
		name = path.Clean(name)
		if _, reported := want[name]; !reported {
			return fmt.Errorf("builtin left unreported output file %q", name)
		}
		fileCount++
		if fileCount > maxFiles {
			return fmt.Errorf("output tree contains more than %d files", maxFiles)
		}
		if info.Size() < 0 || info.Size() > maxBytes-total {
			return fmt.Errorf("output bytes exceed %d-byte limit", maxBytes)
		}
		total += info.Size()
		seen[name] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	for name := range want {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("reported output %q was not written as a regular file", name)
		}
	}
	return nil
}

// signRunJobTemplateReceipt builds the signed AEP v2 run_job_template receipt
// when CITADEL_SIGN_AEP_RECEIPTS is truthy. Fail-open: any error yields no
// receipt (the run still succeeds), mirroring signAppDeployReceipt.
func signRunJobTemplateReceipt(req worker.TemplateRunRequest, outputDigests []string) map[string]any {
	if !update.IsTruthy(os.Getenv("CITADEL_SIGN_AEP_RECEIPTS")) {
		return nil
	}
	signer := nodeidentity.Convergent(network.GetNodeConfigDir())
	fabricNodeID := config.LoadDeviceCredsConverged().FabricNodeID
	nodeID, err := aep.ResolveNodeID(signer, fabricNodeID)
	if err != nil {
		Log("template %q: AEP receipt not signed (node id unresolved): %v", req.TemplateKey, err)
		return nil
	}
	// input_sha256 is the approved manifest content hash (what the run is pinned
	// to); output_sha256 hashes the ordered output digests so both sides agree.
	inputSHA := "sha256:" + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(req.ContentHash)), "sha256:")
	outputSHA := "sha256:" + sha256Hex(strings.Join(outputDigests, "\n"))
	receipt, err := aep.BuildSignedRunJobTemplateReceipt(signer, nodeID, req.JobID, inputSHA, outputSHA, time.Now())
	if err != nil {
		Log("template %q: AEP receipt not signed: %v", req.TemplateKey, err)
		return nil
	}
	m, err := receipt.ToMap()
	if err != nil {
		Log("template %q: AEP receipt not attached: %v", req.TemplateKey, err)
		return nil
	}
	return m
}

// sanitizeTemplateSegment keeps a payload-derived value safe as a single path
// segment. The resulting relative path is interpreted only by an os.Root.
func sanitizeTemplateSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "unknown"
	}
	return out
}

// sha256OpenFile streams an already-open file into sha256. Keeping validation,
// hashing, and sizing on one handle avoids reopening a raced path by name.
func sha256OpenFile(ctx context.Context, f *os.File) (string, int64, error) {
	h := sha256.New()
	n, err := copyTemplateWithContext(ctx, h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sha256OpenFileLimit(ctx context.Context, f *os.File, maxBytes int64) (string, int64, error) {
	h := sha256.New()
	n, err := copyTemplateWithContextLimit(ctx, h, f, maxBytes)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyTemplateWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return copyTemplateWithContextLimit(ctx, dst, src, int64(^uint64(0)>>1))
}

// copyTemplateWithContextLimit refuses before writing the byte that would cross
// maxBytes. The pre-copy Stat check makes ordinary oversized inputs cheap; this
// streaming check closes the race where a source grows after Stat.
func copyTemplateWithContextLimit(ctx context.Context, dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	if maxBytes < 0 {
		return 0, fmt.Errorf("byte limit must not be negative")
	}
	buf := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		nr, readErr := src.Read(buf)
		if nr > 0 {
			if err := ctx.Err(); err != nil {
				return written, err
			}
			if int64(nr) > maxBytes-written {
				return written, fmt.Errorf("byte limit of %d exceeded", maxBytes)
			}
			nw, writeErr := dst.Write(buf[:nr])
			written += int64(nw)
			if writeErr != nil {
				return written, writeErr
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := ctx.Err(); err != nil {
					return written, err
				}
				return written, nil
			}
			return written, readErr
		}
	}
}

func newTemplateAttemptSegment() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("allocate template attempt namespace: %w", err)
	}
	return "attempt-" + hex.EncodeToString(id[:]), nil
}

func cleanupTemplateAttempt(workspaceRoot, inputRoot, outRoot *os.Root, relRun string) error {
	return errors.Join(
		closeTemplateRoot(inputRoot, "input staging root"),
		closeTemplateRoot(outRoot, "output root"),
		removeTemplateAttempt(workspaceRoot, relRun),
	)
}

func finishTemplateAttempt(workspaceRoot, inputRoot, outRoot *os.Root, relRun, relInputs string) error {
	if err := errors.Join(
		closeTemplateRoot(inputRoot, "input staging root"),
		closeTemplateRoot(outRoot, "output root"),
		wrapTemplateError(workspaceRoot.RemoveAll(relInputs), "remove input staging directory"),
	); err != nil {
		return errors.Join(err, removeTemplateAttempt(workspaceRoot, relRun))
	}
	marker := filepath.Join(relRun, templateCompleteMarker)
	if err := workspaceRoot.WriteFile(marker, []byte(templateCompleteBody), 0o400); err != nil {
		return errors.Join(
			fmt.Errorf("mark template attempt complete: %w", err),
			removeTemplateAttempt(workspaceRoot, relRun),
		)
	}
	return nil
}

func removeTemplateAttempt(workspaceRoot *os.Root, relRun string) error {
	return wrapTemplateError(workspaceRoot.RemoveAll(relRun), "remove template attempt directory")
}

func closeTemplateRoot(root *os.Root, name string) error {
	if root == nil {
		return nil
	}
	return wrapTemplateError(root.Close(), "close %s", name)
}

func closeTemplateRootFile(file *os.File, name string) error {
	if file == nil {
		return nil
	}
	return wrapTemplateError(file.Close(), "close %s", name)
}

func wrapTemplateError(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}

// pruneRetainedTemplateAttempts removes only completed, attempt-shaped
// namespaces older than retention. An attempt with an inputs directory is
// active or incomplete and is preserved even if very old; this prevents a
// watchdog-abandoned goroutine from losing paths it may still own. Successful
// pre-marker attempts are recognized by the legacy out-without-inputs shape.
func pruneRetainedTemplateAttempts(workspaceRoot *os.Root, now time.Time, retention time.Duration) error {
	if retention <= 0 {
		return fmt.Errorf("template output retention must be positive")
	}
	templateGCMu.Lock()
	defer templateGCMu.Unlock()

	if _, err := workspaceRoot.Stat("templates"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	templates, err := fs.ReadDir(workspaceRoot.FS(), "templates")
	if err != nil {
		return err
	}
	cutoff := now.Add(-retention)
	for _, templateEntry := range templates {
		if !templateEntry.IsDir() {
			continue
		}
		templateRel := path.Join("templates", templateEntry.Name())
		jobsEntries, err := fs.ReadDir(workspaceRoot.FS(), templateRel)
		if err != nil {
			return err
		}
		for _, jobEntry := range jobsEntries {
			if !jobEntry.IsDir() {
				continue
			}
			jobRel := path.Join(templateRel, jobEntry.Name())
			attempts, err := fs.ReadDir(workspaceRoot.FS(), jobRel)
			if err != nil {
				return err
			}
			for _, attemptEntry := range attempts {
				if !attemptEntry.IsDir() || !isTemplateAttemptSegment(attemptEntry.Name()) {
					continue
				}
				attemptRelSlash := path.Join(jobRel, attemptEntry.Name())
				attemptRel := filepath.FromSlash(attemptRelSlash)
				completedAt, eligible, err := templateAttemptCompletionTime(workspaceRoot, attemptRel)
				if err != nil {
					return fmt.Errorf("inspect %s: %w", attemptRelSlash, err)
				}
				// "Older than" is strict: an attempt exactly at the cutoff is
				// retained until the next sweep.
				if !eligible || !completedAt.Before(cutoff) {
					continue
				}
				if err := workspaceRoot.RemoveAll(attemptRel); err != nil {
					return fmt.Errorf("remove expired attempt %s: %w", attemptRelSlash, err)
				}
			}
		}
	}
	return nil
}

func templateAttemptCompletionTime(workspaceRoot *os.Root, relRun string) (time.Time, bool, error) {
	// An input snapshot is authoritative evidence that the attempt is active or
	// incomplete. Check it before any marker so a corrupt, forged, or partially
	// transitioned marker can never make GC remove paths a live goroutine owns.
	if _, err := workspaceRoot.Lstat(filepath.Join(relRun, "inputs")); err == nil {
		return time.Time{}, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, err
	}

	markerPath := filepath.Join(relRun, templateCompleteMarker)
	markerLstat, err := workspaceRoot.Lstat(markerPath)
	if err == nil {
		if !markerLstat.Mode().IsRegular() {
			return time.Time{}, false, fmt.Errorf("completion marker is not a regular file")
		}
		marker, openErr := workspaceRoot.Open(markerPath)
		if openErr != nil {
			return time.Time{}, false, fmt.Errorf("open completion marker: %w", openErr)
		}
		markerInfo, statErr := marker.Stat()
		if statErr != nil {
			return time.Time{}, false, errors.Join(
				fmt.Errorf("stat completion marker: %w", statErr),
				closeTemplateRootFile(marker, "completion marker"),
			)
		}
		if !markerInfo.Mode().IsRegular() || !os.SameFile(markerLstat, markerInfo) {
			return time.Time{}, false, errors.Join(
				fmt.Errorf("completion marker changed while it was inspected"),
				closeTemplateRootFile(marker, "completion marker"),
			)
		}
		if markerInfo.Size() != int64(len(templateCompleteBody)) {
			return time.Time{}, false, errors.Join(
				fmt.Errorf("completion marker has invalid size"),
				closeTemplateRootFile(marker, "completion marker"),
			)
		}
		body, readErr := io.ReadAll(io.LimitReader(marker, int64(len(templateCompleteBody)+1)))
		closeErr := marker.Close()
		if readErr != nil || closeErr != nil {
			return time.Time{}, false, errors.Join(
				wrapTemplateError(readErr, "read completion marker"),
				wrapTemplateError(closeErr, "close completion marker"),
			)
		}
		if string(body) != templateCompleteBody {
			return time.Time{}, false, fmt.Errorf("completion marker has invalid contents")
		}
		return markerInfo.ModTime(), true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, err
	}

	// Backward compatibility for successful v2.175.0 attempts: successful
	// completion removed inputs and retained out, but wrote no marker.
	outInfo, err := workspaceRoot.Lstat(filepath.Join(relRun, "out"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	if !outInfo.IsDir() {
		return time.Time{}, false, fmt.Errorf("legacy output path is not a directory")
	}
	attemptInfo, err := workspaceRoot.Lstat(relRun)
	if err != nil {
		return time.Time{}, false, err
	}
	return attemptInfo.ModTime(), true, nil
}

func isTemplateAttemptSegment(name string) bool {
	const prefix = "attempt-"
	if len(name) != len(prefix)+32 || !strings.HasPrefix(name, prefix) {
		return false
	}
	for _, c := range name[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
