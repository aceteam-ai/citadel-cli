// cmd/run_job_template_ops.go
//
// cmd-side live adapter for the RUN_JOB_TEMPLATE worker handler (citadel-cli#1149,
// aceteam#10288). The worker handler enforces the security invariants (per-node
// stream gate, manifest-hash recompute, builtin-only kind); this adapter does the
// side effects the worker package must not import: dispatch to a compiled-in
// builtin, write outputs under the node workspace, hash inputs/outputs, and sign
// the AEP receipt with the node key. Same seam shape as liveAppOps / liveExposeOps.
//
// The builtin registry ships EMPTY with an exported RegisterBuiltinTemplateRunner
// seam. The first real builtins (audio-mix, papercraft-render) are S3
// (aceteam#10286), registered from their own files, not here.
package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
// template's params, the on-disk absolute paths of the run's input files, and an
// absolute per-run output directory (already inside the node workspace), and
// returns the paths of the outputs it wrote, RELATIVE to outDir. It must write
// only under outDir; the caller re-validates every returned path against the
// workspace boundary.
type BuiltinTemplateRunner func(ctx context.Context, params json.RawMessage, inputs []string, outDir string) (outputRelPaths []string, err error)

var (
	builtinTemplateRunnersMu sync.RWMutex
	builtinTemplateRunners   = map[string]BuiltinTemplateRunner{}
)

// RegisterBuiltinTemplateRunner registers a compiled-in builtin under the name a
// template's runner.handler references. Real builtins (audio-mix,
// papercraft-render) register themselves from their own files (S3); the registry
// is intentionally empty in this slice.
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
}

type templateInputFile struct {
	Path     string `json:"path"`
	NodeID   string `json:"node_id"`
	NodePath string `json:"node_path"`
}

// Run dispatches an already-verified template request to its builtin, collects
// the outputs as node-backed references, and (when enabled) attaches a signed
// receipt. The worker handler has already checked the per-node gate, the manifest
// hash, and that the runner is a builtin with a non-empty handler name.
func (o liveTemplateRunOps) Run(ctx context.Context, req worker.TemplateRunRequest) (*worker.TemplateRunResult, error) {
	start := time.Now()

	if strings.TrimSpace(o.workspaceDir) == "" {
		return nil, fmt.Errorf("workspace directory is not configured")
	}

	var runner struct {
		Kind    string `json:"kind"`
		Handler string `json:"handler"`
	}
	if err := json.Unmarshal(req.Runner, &runner); err != nil {
		return nil, fmt.Errorf("decode runner: %w", err)
	}
	fn, ok := lookupBuiltinTemplateRunner(strings.TrimSpace(runner.Handler))
	if !ok {
		return nil, fmt.Errorf("%w: %q", worker.ErrTemplateUnknownHandler, runner.Handler)
	}

	// Per-run output directory, confined to the workspace.
	relOut := filepath.Join("templates", sanitizeTemplateSegment(req.TemplateKey), sanitizeTemplateSegment(req.JobID), "out")
	outDir, err := jobs.ValidatePath(o.workspaceDir, relOut)
	if err != nil {
		return nil, fmt.Errorf("resolve output dir: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	inputs, inputHashes, err := o.resolveInputs(req.InputFiles)
	if err != nil {
		return nil, err
	}

	outRels, err := fn(ctx, req.Params, inputs, outDir)
	if err != nil {
		return nil, fmt.Errorf("builtin %q: %w", runner.Handler, err)
	}

	outputs, outputDigests, err := o.collectOutputs(outDir, outRels)
	if err != nil {
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

// resolveInputs maps each declared input file to its on-disk path inside the
// workspace and hashes it. A file not present on this node is out of scope for
// this slice and is a terminal error.
func (o liveTemplateRunOps) resolveInputs(raw json.RawMessage) ([]string, []string, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var files []templateInputFile
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, nil, fmt.Errorf("decode input_files: %w", err)
	}
	var paths []string
	var hashes []string
	for _, f := range files {
		rel := strings.TrimSpace(f.NodePath)
		if rel == "" {
			rel = strings.TrimSpace(f.Path)
		}
		if rel == "" {
			return nil, nil, fmt.Errorf("input file entry has no path")
		}
		abs, err := jobs.ValidateReadPath(o.workspaceDir, rel, false)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve input %q: %w", rel, err)
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return nil, nil, fmt.Errorf("input file %q is not present on this node", rel)
		}
		digest, err := sha256File(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("hash input %q: %w", rel, err)
		}
		paths = append(paths, abs)
		hashes = append(hashes, "sha256:"+digest)
	}
	return paths, hashes, nil
}

// collectOutputs re-validates each builtin-returned output path against the
// workspace boundary (a builtin cannot write outside it), then records the
// workspace-relative path, digest, and size for each.
func (o liveTemplateRunOps) collectOutputs(outDir string, outRels []string) ([]worker.TemplateOutput, []string, error) {
	resolvedWorkspace, err := filepath.EvalSymlinks(o.workspaceDir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve workspace: %w", err)
	}
	var outputs []worker.TemplateOutput
	var digests []string
	for _, rel := range outRels {
		abs, err := jobs.ValidatePath(o.workspaceDir, filepath.Join(outDir, rel))
		if err != nil {
			return nil, nil, fmt.Errorf("output %q escapes the workspace: %w", rel, err)
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return nil, nil, fmt.Errorf("output %q was not written as a file", rel)
		}
		digest, err := sha256File(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("hash output %q: %w", rel, err)
		}
		wsRel, err := filepath.Rel(resolvedWorkspace, abs)
		if err != nil {
			return nil, nil, fmt.Errorf("relativize output %q: %w", rel, err)
		}
		outputs = append(outputs, worker.TemplateOutput{
			Path:   filepath.ToSlash(wsRel),
			SHA256: "sha256:" + digest,
			Bytes:  info.Size(),
		})
		digests = append(digests, "sha256:"+digest)
	}
	return outputs, digests, nil
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
// segment (defense in depth; ValidatePath already confines the join).
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

// sha256File streams a file into sha256 and returns the bare hex digest.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
