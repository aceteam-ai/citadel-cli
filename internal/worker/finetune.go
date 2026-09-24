package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

var fineTuneID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

type FineTuneParameters struct {
	Epochs       int     `json:"epochs"`
	LearningRate float64 `json:"learning_rate"`
	BatchSize    int     `json:"batch_size"`
	LoRAR        int     `json:"lora_r"`
	LoRAAlpha    int     `json:"lora_alpha"`
	LoRADropout  float64 `json:"lora_dropout"`
	MaxSeqLength int     `json:"max_seq_length"`
}

type FineTuneSpec struct {
	JobID       string             `json:"job_id"`
	Model       string             `json:"model"`
	Method      string             `json:"method"`
	DatasetPath string             `json:"dataset_node_path"`
	Suffix      string             `json:"suffix"`
	Parameters  FineTuneParameters `json:"hyperparameters"`
	OutputDir   string             `json:"output_dir"`
}

func parseFineTune(job *Job, workspace, outputRoot string) (FineTuneSpec, error) {
	var spec FineTuneSpec
	if job == nil || !fineTuneID.MatchString(job.ID) {
		return spec, errors.New("FINETUNE_START: invalid job id")
	}
	if job.Payload == nil {
		return spec, errors.New("FINETUNE_START: missing payload")
	}
	spec.JobID = job.ID
	spec.Model, _ = job.Payload["model"].(string)
	if spec.Model != "Qwen/Qwen3-8B" && spec.Model != "Qwen/Qwen3-0.6B" {
		return spec, fmt.Errorf("FINETUNE_START: model %q is not approved", spec.Model)
	}
	spec.Method, _ = job.Payload["method"].(string)
	if spec.Method != "lora" && spec.Method != "qlora" && spec.Method != "full" {
		return spec, fmt.Errorf("FINETUNE_START: unsupported method %q", spec.Method)
	}
	if _, hasURL := job.Payload["dataset_url"]; hasURL {
		return spec, errors.New("FINETUNE_START: dataset_url is unsupported; dataset_node_path is required")
	}
	path, _ := job.Payload["dataset_node_path"].(string)
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) == "." || strings.HasPrefix(filepath.Clean(path), "..") {
		return spec, errors.New("FINETUNE_START: dataset_node_path must be workspace-relative")
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return spec, fmt.Errorf("FINETUNE_START: workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, path))
	if err != nil {
		return spec, fmt.Errorf("FINETUNE_START: dataset: %w", err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return spec, errors.New("FINETUNE_START: dataset escapes workspace")
	}
	stat, err := os.Stat(resolved)
	if err != nil || !stat.Mode().IsRegular() {
		return spec, errors.New("FINETUNE_START: dataset must be a regular file")
	}
	spec.DatasetPath = resolved
	spec.Suffix, _ = job.Payload["suffix"].(string)
	if spec.Suffix != "" && !fineTuneID.MatchString(spec.Suffix) {
		return spec, errors.New("FINETUNE_START: invalid suffix")
	}
	hp, ok := job.Payload["hyperparameters"].(map[string]any)
	if !ok {
		return spec, errors.New("FINETUNE_START: hyperparameters are required")
	}
	raw, err := json.Marshal(hp)
	if err != nil {
		return spec, err
	}
	if err := json.Unmarshal(raw, &spec.Parameters); err != nil {
		return spec, fmt.Errorf("FINETUNE_START: hyperparameters: %w", err)
	}
	p := spec.Parameters
	if p.Epochs < 1 || p.Epochs > 100 || p.LearningRate <= 0 || p.LearningRate > 1 || math.IsNaN(p.LearningRate) || p.BatchSize < 1 || p.BatchSize > 128 || p.LoRAR < 1 || p.LoRAR > 256 || p.LoRAAlpha < 1 || p.LoRAAlpha > 512 || p.LoRADropout < 0 || p.LoRADropout > 1 || p.MaxSeqLength < 128 || p.MaxSeqLength > 32768 {
		return spec, errors.New("FINETUNE_START: hyperparameters outside approved ranges")
	}
	spec.OutputDir = filepath.Join(outputRoot, job.ID)
	return spec, nil
}

// FineTuneControl is the authenticated, job-scoped status/cancel contract. The
// API proxy implementation is intentionally absent until its backend companion
// exposes this exact scope; a generic KV endpoint is not authorized for these keys.
type FineTuneControl interface {
	Cancelled(context.Context, string) (bool, error)
	Update(context.Context, string, map[string]any) error
	FailCritical(context.Context, string, map[string]any) error
	Progress(context.Context, string, map[string]any) error
}

type FineTuneReservation interface {
	Reserve(context.Context, string) ([]string, error)
	Release(context.Context, string) error
}

type FineTuneConfig struct {
	NodeID       string
	WorkspaceDir string
	OutputRoot   string
	CacheDir     string
	Image        string
	Control      FineTuneControl
	Reservation  FineTuneReservation
	Run          func(context.Context, FineTuneSpec, func(map[string]any) error) error
}

type FineTuneHandler struct {
	cfg          FineTuneConfig
	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeJob    string
	activeDone   chan struct{}
	demanded     bool
}

func NewFineTuneHandler(cfg FineTuneConfig) *FineTuneHandler { return &FineTuneHandler{cfg: cfg} }
func (h *FineTuneHandler) CanHandle(t string) bool           { return t == JobTypeFineTuneStart }

// YieldToDemand is called by the fetch loop before interactive or inference
// work is admitted. It kills the active training container; reservation release
// then restores serving modules before this job reports cancellation.
func (h *FineTuneHandler) YieldToDemand() {
	h.mu.Lock()
	done := h.activeDone
	if h.activeCancel != nil {
		h.demanded = true
		h.activeCancel()
	}
	h.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (h *FineTuneHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	terminalFailure := func(err error, honorCancellation bool) (*JobResult, error) {
		if h.cfg.Control != nil && job != nil {
			if honorCancellation {
				if cancelled, checkErr := h.cfg.Control.Cancelled(context.Background(), job.ID); checkErr == nil && cancelled {
					return h.cancelled(context.Background(), job.ID, stream, "cancelled")
				}
			}
			errorText := err.Error()
			if len(errorText) > 2000 {
				errorText = strings.ToValidUTF8(errorText[:2000], "") + " (truncated)"
			}
			fields := map[string]any{"status": "failed", "error": errorText, "finished_at": time.Now().UTC().Format(time.RFC3339)}
			if persistErr := h.persistTerminal(job.ID, fields, !honorCancellation); persistErr != nil {
				err = errors.Join(err, fmt.Errorf("FINETUNE_START: canonical failure status update failed: %w", persistErr))
			}
		}
		return &JobResult{Status: JobStatusTerminalFailure, Error: err}, nil
	}
	fail := func(err error) (*JobResult, error) { return terminalFailure(err, true) }
	failCritical := func(err error) (*JobResult, error) { return terminalFailure(err, false) }
	if !isPerNodeStream(job.SourceQueue) {
		return fail(errors.New("FINETUNE_START requires a per-node queue"))
	}
	if h.cfg.Control == nil || h.cfg.Reservation == nil {
		return fail(errors.New("FINETUNE_START status/cancel seam unavailable"))
	}
	spec, err := parseFineTune(job, h.cfg.WorkspaceDir, h.cfg.OutputRoot)
	if err != nil {
		return fail(err)
	}
	node, _ := job.Payload["node_id"].(string)
	datasetNode, _ := job.Payload["dataset_node_id"].(string)
	if h.cfg.NodeID == "" || node != h.cfg.NodeID || datasetNode != node {
		return fail(errors.New("FINETUNE_START: dataset and job must be pinned to this node"))
	}
	if err := os.MkdirAll(spec.OutputDir, 0700); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(h.cfg.CacheDir, 0700); err != nil {
		return fail(err)
	}
	if cancelled, err := h.cfg.Control.Cancelled(ctx, job.ID); err != nil {
		return fail(err)
	} else if cancelled {
		return h.cancelled(ctx, job.ID, stream, "cancelled before start")
	}
	if err := h.cfg.Control.Update(ctx, job.ID, map[string]any{"status": "running", "started_at": time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return fail(err)
	}
	trainCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.activeCancel, h.activeJob, h.demanded = cancel, job.ID, false
	h.activeDone = make(chan struct{})
	h.mu.Unlock()
	defer func() {
		cancel()
		h.mu.Lock()
		h.activeCancel = nil
		h.activeJob = ""
		close(h.activeDone)
		h.activeDone = nil
		h.mu.Unlock()
	}()
	// Reserve may partially stop services before failing. Always release by the
	// durable job tag, including on reserve errors and process failures.
	evicted, reserveErr := h.cfg.Reservation.Reserve(trainCtx, job.ID)
	release := func() error {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer releaseCancel()
		return h.cfg.Reservation.Release(releaseCtx, job.ID)
	}
	if reserveErr != nil {
		if err := release(); err != nil {
			return failCritical(errors.Join(reserveErr, fmt.Errorf("FINETUNE_START: restore after reservation failure: %w", err)))
		}
		return fail(reserveErr)
	}
	if trainCtx.Err() != nil {
		if err := release(); err != nil {
			return failCritical(fmt.Errorf("FINETUNE_START: restore after preemption: %w", err))
		}
		return h.cancelled(context.Background(), job.ID, stream, "preempted by demand")
	}
	stopPoll := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopPoll:
				return
			case <-trainCtx.Done():
				return
			case <-ticker.C:
				cancelled, err := h.cfg.Control.Cancelled(trainCtx, job.ID)
				if err != nil || cancelled {
					cancel()
					return
				}
			}
		}
	}()
	progress := func(event map[string]any) error {
		if len(event) != 3 {
			return errors.New("training progress has unexpected fields")
		}
		percent, percentOK := event["progress_percent"].(float64)
		epoch, epochOK := event["current_epoch"].(float64)
		loss, lossOK := event["current_loss"].(float64)
		if !percentOK || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 ||
			!epochOK || math.IsNaN(epoch) || math.IsInf(epoch, 0) || epoch < 0 || epoch != math.Trunc(epoch) || epoch > float64(spec.Parameters.Epochs) ||
			!lossOK || math.IsNaN(loss) || math.IsInf(loss, 0) || loss < 0 {
			return errors.New("training progress has invalid values")
		}
		if err := h.cfg.Control.Update(trainCtx, job.ID, event); err != nil {
			return err
		}
		return h.cfg.Control.Progress(trainCtx, job.ID, event)
	}
	run := h.cfg.Run
	if run == nil {
		run = func(ctx context.Context, s FineTuneSpec, progress func(map[string]any) error) error {
			return runFineTuneContainer(ctx, s, h.cfg.Image, h.cfg.CacheDir, progress)
		}
	}
	runErr := run(trainCtx, spec, progress)
	wasCancelled := trainCtx.Err() != nil
	close(stopPoll)
	cancel()
	var terminationErr *fineTuneTerminationError
	if errors.As(runErr, &terminationErr) {
		// The container may still own GPU memory. Keep the durable reservation
		// tag and serving modules stopped; never claim cancellation succeeded.
		return failCritical(runErr)
	}
	releaseErr := release()
	if releaseErr != nil {
		return failCritical(fmt.Errorf("FINETUNE_START: restore %v: %w", evicted, releaseErr))
	}
	h.mu.Lock()
	demanded := h.demanded
	h.mu.Unlock()
	if wasCancelled {
		reason := "cancelled"
		if demanded {
			reason = "preempted by interactive or inference demand"
		}
		return h.cancelled(context.Background(), job.ID, stream, reason)
	}
	if cancelled, err := h.cfg.Control.Cancelled(context.Background(), job.ID); err == nil && cancelled {
		return h.cancelled(context.Background(), job.ID, stream, "cancelled")
	} else if err != nil {
		return fail(err)
	}
	if runErr != nil {
		return fail(runErr)
	}
	state := map[string]any{"status": "succeeded", "adapter_path": spec.OutputDir, "progress_percent": 100, "finished_at": time.Now().UTC().Format(time.RFC3339)}
	if err := h.cfg.Control.Update(context.Background(), job.ID, state); err != nil {
		return fail(err)
	}
	return &JobResult{Status: JobStatusSuccess, Output: state}, nil
}

func (h *FineTuneHandler) cancelled(ctx context.Context, id string, stream StreamWriter, reason string) (*JobResult, error) {
	fields := map[string]any{"status": "cancelled", "finished_at": time.Now().UTC().Format(time.RFC3339)}
	if err := h.persistTerminal(id, fields, false); err != nil {
		return &JobResult{Status: JobStatusTerminalFailure, Error: fmt.Errorf("FINETUNE_START: cancellation cleanup finished but canonical status update failed: %w", err)}, nil
	}
	if stream != nil {
		_ = stream.WriteCancelled(reason)
	}
	return &JobResult{Status: JobStatusCancelled, Output: map[string]any{"status": "cancelled", "reason": reason}}, nil
}

// Terminal writes are idempotent in both control implementations. A short
// bounded retry handles transient status transport failures without rerunning
// training or claiming cancellation before the canonical hash confirms it.
func (h *FineTuneHandler) persistTerminal(id string, fields map[string]any, critical bool) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if critical {
			err = h.cfg.Control.FailCritical(ctx, id, fields)
		} else {
			err = h.cfg.Control.Update(ctx, id, fields)
		}
		cancel()
		if err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return err
}

func runFineTuneContainer(ctx context.Context, spec FineTuneSpec, image, cacheDir string, progress func(map[string]any) error) error {
	return runFineTuneContainerWithRuntime(ctx, spec, image, cacheDir, catalog.SelectContainerRuntime(), progress)
}

type fineTuneTerminationError struct{ cause error }

func (e *fineTuneTerminationError) Error() string {
	return "FINETUNE_START: training container termination unconfirmed: " + e.cause.Error()
}
func (e *fineTuneTerminationError) Unwrap() error { return e.cause }

func runFineTuneContainerWithRuntime(ctx context.Context, spec FineTuneSpec, image, cacheDir string, runtime catalog.ContainerRuntime, progress func(map[string]any) error) error {
	args, err := fineTuneEngineArgs(spec, image, cacheDir, runtime)
	if err != nil {
		return err
	}
	name := "citadel-finetune-" + strings.ToLower(spec.JobID)
	cmd := runtime.EngineCommandContext(ctx, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	encoded, _ := json.Marshal(spec)
	_, _ = stdin.Write(append(encoded, '\n'))
	_ = stdin.Close()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64*1024), 1024*1024)
	var progressErr error
	for scan.Scan() {
		var event map[string]any
		if json.Unmarshal(scan.Bytes(), &event) == nil && event["type"] == "progress" {
			delete(event, "type")
			if err := progress(event); err != nil {
				progressErr = err
				break
			}
		}
	}
	if progressErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	// Even a successful engine CLI exit is not enough to prove the container
	// stopped. Force removal, then query the runtime's running-container list.
	// A failed rm is harmless only when that independent query proves absence.
	if err := stopFineTuneContainer(runtime, name); err != nil {
		return &fineTuneTerminationError{cause: errors.Join(waitErr, progressErr, err)}
	}
	if progressErr != nil {
		return progressErr
	}
	if err := scan.Err(); err != nil {
		return err
	}
	return waitErr
}

func stopFineTuneContainer(runtime catalog.ContainerRuntime, name string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	removeOutput, removeErr := runtime.EngineCommandContext(cleanupCtx, "rm", "-f", name).CombinedOutput()
	// `ps` lists running containers only. Compare exact names instead of using
	// engine-specific filter regex semantics; Docker and Podman both support
	// this format. Failure to query is not evidence that the child has stopped.
	runningOutput, inspectErr := runtime.EngineCommandContext(cleanupCtx, "ps", "--format", "{{.Names}}").Output()
	if inspectErr != nil {
		return fmt.Errorf("force remove error=%v (%s); verify running containers: %w", removeErr, strings.TrimSpace(string(removeOutput)), inspectErr)
	}
	for _, runningName := range strings.Fields(string(runningOutput)) {
		if runningName == name {
			return fmt.Errorf("container %s is still running after force removal: %v (%s)", name, removeErr, strings.TrimSpace(string(removeOutput)))
		}
	}
	return nil
}

func fineTuneDockerArgs(spec FineTuneSpec, image, cacheDir string) ([]string, error) {
	return fineTuneEngineArgs(spec, image, cacheDir, catalog.ContainerRuntime{})
}

func fineTuneEngineArgs(spec FineTuneSpec, image, cacheDir string, runtime catalog.ContainerRuntime) ([]string, error) {
	if image == "" {
		return nil, errors.New("FINETUNE_START training image is not configured")
	}
	for _, path := range []string{spec.DatasetPath, spec.OutputDir, cacheDir} {
		if strings.ContainsAny(path, ",:\n\r") {
			return nil, errors.New("FINETUNE_START mount path contains unsupported characters")
		}
	}
	name := "citadel-finetune-" + strings.ToLower(spec.JobID)
	gpuArgs, err := runtime.GPUArgs("all")
	if err != nil {
		return nil, err
	}
	args := []string{"run", "--rm", "-i", "--name", name}
	args = append(args, gpuArgs...)
	args = append(args, "--network", "none", "--read-only", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--tmpfs", "/tmp:rw,nosuid,size=2g", "--mount", "type=bind,src="+spec.DatasetPath+",dst=/data/train.jsonl,readonly", "--mount", "type=bind,src="+spec.OutputDir+",dst=/output", "--mount", "type=bind,src="+cacheDir+",dst=/cache,readonly", "-e", "HF_HOME=/cache", "-e", "HF_HUB_OFFLINE=1", "-e", "TRANSFORMERS_OFFLINE=1", image)
	return args, nil
}
