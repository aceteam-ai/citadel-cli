// Package reconstruct runs the node-local video or frame-folder to 3DGS
// reconstruction pipeline. It deliberately owns only the engine pipeline; the
// signed platform lifecycle which invokes it is a separate contract.
package reconstruct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	defaultFrameFPS             = 12
	defaultSFMQuality           = "high"
	defaultTrainQuality         = "high"
	defaultMaxSplats            = 1_000_000
	defaultIterations           = 30_000
	defaultResolutionDivisor    = 1
	defaultMinRegisteredPercent = 50
	defaultMaxMeanReprojection  = 2
)

// Invocation is one argv-only child process call. Shell interpretation is
// never part of the pipeline.
type Invocation struct {
	Name   string
	Args   []string
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

// Executor is the fake-exec seam used by hermetic tests and by callers which
// need to put the subprocess inside stronger node-specific isolation.
type Executor interface {
	Run(ctx context.Context, invocation Invocation) error
}

// Request describes one run. Zero-valued tuning fields receive the production
// defaults documented on their fields.
type Request struct {
	// InputPath is either one video file or a directory of image frames.
	InputPath string
	// OutputDir must not exist. It becomes addressable only after a complete
	// PLY has been hashed and atomically promoted.
	OutputDir string

	// FrameFPS applies only to video input. The default is 12.
	FrameFPS float64
	// SFMQuality is low, medium, high (default), or extreme.
	SFMQuality string
	// TrainQuality is low, medium, high (default), or ultra.
	TrainQuality string
	// MaxSplats defaults to 1,000,000.
	MaxSplats int
	// Iterations defaults to 30,000.
	Iterations int
	// TrainResolutionDivisor defaults to 1.
	TrainResolutionDivisor int

	// MinRegisteredPercent defaults to 50. A lower signal stops before train.
	MinRegisteredPercent float64
	// MaxMeanReprojectionPX defaults to 2. A higher signal stops before train.
	MaxMeanReprojectionPX float64
}

// QualitySignal is the cheap SfM go/no-go signal surfaced before training.
type QualitySignal struct {
	RegisteredImages    int     `json:"registered_images"`
	TotalImages         int     `json:"total_images"`
	RegisteredPercent   float64 `json:"registered_percent"`
	MeanReprojectionPX  float64 `json:"mean_reprojection_px"`
	AcceptedForTraining bool    `json:"accepted_for_training"`
}

// Artifact is intentionally reference-only. The PLY bytes are never inlined.
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Result reports the quality gate and, only after success, the promoted PLY.
type Result struct {
	Quality  QualitySignal `json:"quality"`
	Artifact *Artifact     `json:"artifact,omitempty"`
}

// ErrQualityGate means SfM completed but its signal did not justify spending
// GPU time on training.
var ErrQualityGate = errors.New("reconstruct: SfM quality gate rejected capture")

// Runner executes reconstruction without imposing a deadline. These scans are
// an unbounded-length workload; the platform lifecycle owns cancellation, not
// the RUN_JOB_TEMPLATE four-hour watchdog lane.
type Runner struct {
	Executor    Executor
	FFmpegPath  string
	SpirulaPath string
	Log         io.Writer
}

// Run executes ffmpeg (for video), SfM, the quality gate, and headless 3DGS
// training. Cancellation is delegated to Executor and all hidden work dirs are
// removed before Run returns.
func (r Runner) Run(ctx context.Context, request Request) (Result, error) {
	var result Result
	if r.Executor == nil {
		return result, fmt.Errorf("reconstruct: executor is required")
	}
	req, input, output, inputIsDir, err := normalizeRequest(request)
	if err != nil {
		return result, err
	}
	ffmpeg := r.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	spirula := r.SpirulaPath
	if spirula == "" {
		spirula = "spirula"
	}
	log := r.Log
	if log == nil {
		log = io.Discard
	}

	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return result, fmt.Errorf("reconstruct: create output parent: %w", err)
	}
	base := filepath.Base(output)
	workDir, err := os.MkdirTemp(parent, "."+base+".work-")
	if err != nil {
		return result, fmt.Errorf("reconstruct: create private work directory: %w", err)
	}
	if err := os.Chmod(workDir, 0o700); err != nil {
		_ = os.RemoveAll(workDir)
		return result, fmt.Errorf("reconstruct: protect private work directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	framesDir := input
	if !inputIsDir {
		framesDir = filepath.Join(workDir, "frames")
		if err := os.Mkdir(framesDir, 0o700); err != nil {
			return result, fmt.Errorf("reconstruct: create frame directory: %w", err)
		}
		framePattern := filepath.Join(framesDir, "frame_%06d.jpg")
		args := []string{
			"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-i", input,
			"-vf", "fps=" + strconv.FormatFloat(req.FrameFPS, 'f', -1, 64),
			"-q:v", "2", framePattern,
		}
		if err := r.run(ctx, log, Invocation{Name: ffmpeg, Args: args, Dir: workDir}); err != nil {
			return result, fmt.Errorf("reconstruct: extract frames: %w", err)
		}
	}

	sfmDir := filepath.Join(workDir, "sfm")
	var sfmOutput synchronizedBuffer
	sfmLog := io.MultiWriter(log, &sfmOutput)
	sfmArgs := []string{
		"sfm", "auto", framesDir,
		"-o", sfmDir,
		"--sequence", ".",
		"--device", "0",
		"--quality", req.SFMQuality,
		"--data-type", "video",
	}
	sfmErr := r.runWithWriters(ctx, Invocation{
		Name: spirula, Args: sfmArgs, Dir: workDir,
		Stdout: sfmLog, Stderr: sfmLog,
	})
	// Spirula uses exit 3 for a completed partial reconstruction and exit 4
	// for a completed, sound reconstruction without a metric gauge. Both still
	// carry the quality signal and a usable sparse model, so apply our explicit
	// gate instead of confusing those verdicts with a crashed subprocess.
	if sfmErr != nil && !completedSFMVerdict(sfmErr) {
		return result, fmt.Errorf("reconstruct: SfM: %w", sfmErr)
	}
	quality, err := ParseQualitySignal(sfmOutput.String())
	if err != nil {
		return result, fmt.Errorf("reconstruct: SfM completed without a parseable quality signal: %w", err)
	}
	quality.AcceptedForTraining = quality.RegisteredPercent >= req.MinRegisteredPercent &&
		quality.MeanReprojectionPX <= req.MaxMeanReprojectionPX
	result.Quality = quality
	if !quality.AcceptedForTraining {
		return result, fmt.Errorf(
			"%w: %.1f%% registered (minimum %.1f%%), mean reprojection %.3f px (maximum %.3f px)",
			ErrQualityGate, quality.RegisteredPercent, req.MinRegisteredPercent,
			quality.MeanReprojectionPX, req.MaxMeanReprojectionPX)
	}
	// SfM writes COLMAP features/matches/sparse data into its workspace but
	// intentionally leaves the source images in place. Complete the standard
	// COLMAP dataset layout for the trainer with a private symlink rather than
	// copying a multi-hundred-frame 4K capture.
	imagesLink := filepath.Join(sfmDir, "images")
	if err := os.MkdirAll(sfmDir, 0o700); err != nil {
		return result, fmt.Errorf("reconstruct: create SfM dataset directory: %w", err)
	}
	if _, err := os.Lstat(imagesLink); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(framesDir, imagesLink); err != nil {
			return result, fmt.Errorf("reconstruct: link frames into SfM dataset: %w", err)
		}
	} else if err != nil {
		return result, fmt.Errorf("reconstruct: inspect SfM image directory: %w", err)
	} else if info, err := os.Stat(imagesLink); err != nil || !info.IsDir() {
		return result, fmt.Errorf("reconstruct: SfM image path is not a directory")
	}

	trainRoot := filepath.Join(workDir, "training")
	trainArgs := []string{
		"train", "3dgs",
		"--data", sfmDir,
		"--device", "0",
		"--quality", req.TrainQuality,
		"--cap-max", strconv.Itoa(req.MaxSplats),
		"--num-iterations", strconv.Itoa(req.Iterations),
		"--train-resolution-divisor", strconv.Itoa(req.TrainResolutionDivisor),
		"--output-dir-prefix", trainRoot,
		"--output-dir-name", "run",
		"--steps-per-save", strconv.Itoa(req.Iterations),
		"--save-only-latest-checkpoint", "1",
		"--disable-viewer", "1",
		"--keep-viewer-alive", "0",
	}
	if err := r.run(ctx, log, Invocation{Name: spirula, Args: trainArgs, Dir: workDir}); err != nil {
		return result, fmt.Errorf("reconstruct: train 3DGS: %w", err)
	}

	checkpoint := fmt.Sprintf("step-%09d.ckpt", req.Iterations)
	ply := filepath.Join(trainRoot, "run", checkpoint, "splat.ply")
	info, err := os.Lstat(ply)
	if err != nil {
		return result, fmt.Errorf("reconstruct: final artifact missing: %w", err)
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("reconstruct: final artifact is not a regular file")
	}
	digest, err := hashFile(ctx, ply)
	if err != nil {
		return result, fmt.Errorf("reconstruct: hash final artifact: %w", err)
	}

	publishDir, err := os.MkdirTemp(parent, "."+base+".publish-")
	if err != nil {
		return result, fmt.Errorf("reconstruct: create private publish directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(publishDir)
		}
	}()
	if err := os.Chmod(publishDir, 0o700); err != nil {
		return result, fmt.Errorf("reconstruct: protect publish directory: %w", err)
	}
	stagedPLY := filepath.Join(publishDir, "splat.ply")
	if err := os.Rename(ply, stagedPLY); err != nil {
		return result, fmt.Errorf("reconstruct: stage final artifact: %w", err)
	}
	if err := os.Rename(publishDir, output); err != nil {
		return result, fmt.Errorf("reconstruct: atomically publish final artifact: %w", err)
	}
	published = true
	result.Artifact = &Artifact{
		Path:   filepath.Join(output, "splat.ply"),
		SHA256: "sha256:" + digest,
	}
	return result, nil
}

func (r Runner) run(ctx context.Context, log io.Writer, invocation Invocation) error {
	invocation.Stdout = log
	invocation.Stderr = log
	return r.runWithWriters(ctx, invocation)
}

func (r Runner) runWithWriters(ctx context.Context, invocation Invocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Executor.Run(ctx, invocation)
}

func normalizeRequest(req Request) (Request, string, string, bool, error) {
	if strings.TrimSpace(req.InputPath) == "" || strings.TrimSpace(req.OutputDir) == "" {
		return req, "", "", false, fmt.Errorf("reconstruct: input and output paths are required")
	}
	input, err := filepath.Abs(req.InputPath)
	if err != nil {
		return req, "", "", false, fmt.Errorf("reconstruct: resolve input path: %w", err)
	}
	input, err = filepath.EvalSymlinks(input)
	if err != nil {
		return req, "", "", false, fmt.Errorf("reconstruct: resolve input symlinks: %w", err)
	}
	info, err := os.Stat(input)
	if err != nil {
		return req, "", "", false, fmt.Errorf("reconstruct: inspect input: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return req, "", "", false, fmt.Errorf("reconstruct: input must be a regular video file or frame directory")
	}
	output, err := filepath.Abs(req.OutputDir)
	if err != nil {
		return req, "", "", false, fmt.Errorf("reconstruct: resolve output path: %w", err)
	}
	if _, err := os.Lstat(output); err == nil {
		return req, "", "", false, fmt.Errorf("reconstruct: output path already exists: %s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return req, "", "", false, fmt.Errorf("reconstruct: inspect output path: %w", err)
	}
	if filepath.Clean(input) == filepath.Clean(output) {
		return req, "", "", false, fmt.Errorf("reconstruct: input and output paths must differ")
	}
	if info.IsDir() && (pathWithin(output, input) || pathWithin(input, output)) {
		return req, "", "", false, fmt.Errorf("reconstruct: frame input and output directories must not overlap")
	}

	if req.FrameFPS == 0 {
		req.FrameFPS = defaultFrameFPS
	}
	if req.FrameFPS <= 0 || req.FrameFPS > 120 {
		return req, "", "", false, fmt.Errorf("reconstruct: frame FPS must be in (0, 120]")
	}
	if req.SFMQuality == "" {
		req.SFMQuality = defaultSFMQuality
	}
	if !oneOf(req.SFMQuality, "low", "medium", "high", "extreme") {
		return req, "", "", false, fmt.Errorf("reconstruct: invalid SfM quality %q", req.SFMQuality)
	}
	if req.TrainQuality == "" {
		req.TrainQuality = defaultTrainQuality
	}
	if !oneOf(req.TrainQuality, "low", "medium", "high", "ultra") {
		return req, "", "", false, fmt.Errorf("reconstruct: invalid train quality %q", req.TrainQuality)
	}
	if req.MaxSplats == 0 {
		req.MaxSplats = defaultMaxSplats
	}
	if req.MaxSplats < 1 {
		return req, "", "", false, fmt.Errorf("reconstruct: max splats must be positive")
	}
	if req.Iterations == 0 {
		req.Iterations = defaultIterations
	}
	if req.Iterations < 1 {
		return req, "", "", false, fmt.Errorf("reconstruct: iterations must be positive")
	}
	if req.TrainResolutionDivisor == 0 {
		req.TrainResolutionDivisor = defaultResolutionDivisor
	}
	if req.TrainResolutionDivisor < 1 {
		return req, "", "", false, fmt.Errorf("reconstruct: train resolution divisor must be positive")
	}
	if req.MinRegisteredPercent == 0 {
		req.MinRegisteredPercent = defaultMinRegisteredPercent
	}
	if req.MinRegisteredPercent < 0 || req.MinRegisteredPercent > 100 {
		return req, "", "", false, fmt.Errorf("reconstruct: minimum registered percent must be in [0, 100]")
	}
	if req.MaxMeanReprojectionPX == 0 {
		req.MaxMeanReprojectionPX = defaultMaxMeanReprojection
	}
	if req.MaxMeanReprojectionPX < 0 {
		return req, "", "", false, fmt.Errorf("reconstruct: maximum mean reprojection must be nonnegative")
	}
	return req, input, output, info.IsDir(), nil
}

func pathWithin(path, directory string) bool {
	rel, err := filepath.Rel(directory, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type exitCoder interface {
	ExitCode() int
}

func completedSFMVerdict(err error) bool {
	var exited exitCoder
	if !errors.As(err, &exited) {
		return false
	}
	return exited.ExitCode() == 3 || exited.ExitCode() == 4
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

var (
	registeredRE   = regexp.MustCompile(`(?i)Registered:\s*([0-9]+)\s*/\s*([0-9]+)\s+images`)
	reprojectionRE = regexp.MustCompile(`(?i)Reprojection error:\s*mean\s*([0-9]+(?:\.[0-9]+)?)\s*px`)
	resultRE       = regexp.MustCompile(`(?i)RESULT:\s*(?:OK|PARTIAL)\s*--\s*([0-9]+(?:\.[0-9]+)?)%.*?([0-9]+(?:\.[0-9]+)?)\s*px mean reprojection`)
)

// ParseQualitySignal parses the stable English summary emitted by the pinned
// Spirula release. It prefers exact image counts and falls back to the RESULT
// line's percentage when necessary.
func ParseQualitySignal(output string) (QualitySignal, error) {
	var signal QualitySignal
	registered := registeredRE.FindAllStringSubmatch(output, -1)
	if len(registered) > 0 {
		last := registered[len(registered)-1]
		signal.RegisteredImages, _ = strconv.Atoi(last[1])
		signal.TotalImages, _ = strconv.Atoi(last[2])
		if signal.TotalImages > 0 {
			signal.RegisteredPercent = 100 * float64(signal.RegisteredImages) / float64(signal.TotalImages)
		}
	}
	reprojection := reprojectionRE.FindAllStringSubmatch(output, -1)
	if len(reprojection) > 0 {
		signal.MeanReprojectionPX, _ = strconv.ParseFloat(reprojection[len(reprojection)-1][1], 64)
	}
	result := resultRE.FindAllStringSubmatch(output, -1)
	if len(result) > 0 {
		last := result[len(result)-1]
		if signal.TotalImages == 0 {
			signal.RegisteredPercent, _ = strconv.ParseFloat(last[1], 64)
		}
		if len(reprojection) == 0 {
			signal.MeanReprojectionPX, _ = strconv.ParseFloat(last[2], 64)
		}
	}
	if signal.TotalImages == 0 && len(result) == 0 {
		return QualitySignal{}, fmt.Errorf("registered-image summary not found")
	}
	if len(reprojection) == 0 && len(result) == 0 {
		return QualitySignal{}, fmt.Errorf("mean-reprojection summary not found")
	}
	return signal, nil
}

func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err := h.Write(buf[:n]); err != nil {
				return "", err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// synchronizedBuffer tolerates stdout and stderr copy goroutines writing at
// the same time in os/exec.
type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
