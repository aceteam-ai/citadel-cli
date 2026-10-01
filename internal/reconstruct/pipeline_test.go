package reconstruct

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type recordedInvocation struct {
	Name string
	Args []string
	Dir  string
}

type fakeExecutor struct {
	mu    sync.Mutex
	calls []recordedInvocation
	run   func(context.Context, Invocation, int) error
}

func (f *fakeExecutor) Run(ctx context.Context, invocation Invocation) error {
	f.mu.Lock()
	index := len(f.calls)
	f.calls = append(f.calls, recordedInvocation{
		Name: invocation.Name,
		Args: append([]string(nil), invocation.Args...),
		Dir:  invocation.Dir,
	})
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, invocation, index)
	}
	return nil
}

func (f *fakeExecutor) Calls() []recordedInvocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedInvocation(nil), f.calls...)
}

func TestRunVideoPipelinePublishesOnlyReferenceAndHash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	video := filepath.Join(root, "orbit clip.mp4")
	if err := os.WriteFile(video, []byte("fake video"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "result")
	plyBytes := []byte("ply\nformat binary_little_endian 1.0\n")
	fake := &fakeExecutor{}
	fake.run = func(_ context.Context, invocation Invocation, _ int) error {
		switch {
		case invocation.Name == "/tools/ffmpeg":
			return nil
		case invocation.Name == "/tools/spirula" && len(invocation.Args) >= 2 && invocation.Args[0] == "sfm":
			_, _ = io.WriteString(invocation.Stdout,
				"Mapping: done   Registered: 80/100 images   Points: 45000   Cameras: 1\n"+
					"Reprojection error: mean 0.49 px, median 0.41 px, over 300 observations\n")
			return nil
		case invocation.Name == "/tools/spirula" && len(invocation.Args) >= 2 && invocation.Args[0] == "train":
			data := argumentValue(t, invocation.Args, "--data")
			linkedFrames, err := filepath.EvalSymlinks(filepath.Join(data, "images"))
			if err != nil || linkedFrames == "" {
				return fmt.Errorf("training dataset images link is invalid: path=%q err=%v", linkedFrames, err)
			}
			prefix := argumentValue(t, invocation.Args, "--output-dir-prefix")
			iterations := argumentValue(t, invocation.Args, "--num-iterations")
			artifact := filepath.Join(prefix, "run", "step-000007000.ckpt", "splat.ply")
			if iterations != "7000" {
				t.Fatalf("iterations = %q, want 7000", iterations)
			}
			if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
				return err
			}
			return os.WriteFile(artifact, plyBytes, 0o600)
		default:
			return fmt.Errorf("unexpected invocation: %s %v", invocation.Name, invocation.Args)
		}
	}

	result, err := (Runner{
		Executor: fake, FFmpegPath: "/tools/ffmpeg", SpirulaPath: "/tools/spirula",
	}).Run(context.Background(), Request{
		InputPath: video, OutputDir: output, FrameFPS: 15,
		SFMQuality: "high", TrainQuality: "low", MaxSplats: 300_000,
		Iterations: 7_000, TrainResolutionDivisor: 2,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Quality.AcceptedForTraining || result.Quality.RegisteredPercent != 80 || result.Quality.MeanReprojectionPX != 0.49 {
		t.Fatalf("quality = %+v", result.Quality)
	}
	if result.Artifact == nil {
		t.Fatal("artifact is nil")
	}
	wantHash := fmt.Sprintf("sha256:%x", sha256.Sum256(plyBytes))
	if result.Artifact.Path != filepath.Join(output, "splat.ply") || result.Artifact.SHA256 != wantHash {
		t.Fatalf("artifact = %+v, want path %q hash %q", result.Artifact, filepath.Join(output, "splat.ply"), wantHash)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "splat.ply" {
		t.Fatalf("published entries = %v, want only splat.ply", entries)
	}

	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want ffmpeg + sfm + train", len(calls))
	}
	wantFFmpegPrefix := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", video, "-vf", "fps=15", "-q:v", "2"}
	if !reflect.DeepEqual(calls[0].Args[:len(wantFFmpegPrefix)], wantFFmpegPrefix) {
		t.Errorf("ffmpeg args = %v, want prefix %v", calls[0].Args, wantFFmpegPrefix)
	}
	assertSubsequence(t, calls[1].Args, []string{"sfm", "auto"})
	assertSubsequence(t, calls[1].Args, []string{"--sequence", ".", "--device", "0", "--quality", "high", "--data-type", "video"})
	assertSubsequence(t, calls[2].Args, []string{"train", "3dgs"})
	assertSubsequence(t, calls[2].Args, []string{"--device", "0", "--quality", "low", "--cap-max", "300000"})
	assertSubsequence(t, calls[2].Args, []string{"--disable-viewer", "1", "--keep-viewer-alive", "0"})
}

func TestRunFrameFolderSkipsFFmpeg(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	frames := filepath.Join(root, "frames")
	if err := os.Mkdir(frames, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "result")
	fake := successfulFake(t, []byte("ply"))

	result, err := (Runner{Executor: fake}).Run(context.Background(), Request{
		InputPath: frames, OutputDir: output, Iterations: 3,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Artifact == nil {
		t.Fatal("artifact is nil")
	}
	calls := fake.Calls()
	if len(calls) != 2 || calls[0].Name != "spirula" || calls[1].Name != "spirula" {
		t.Fatalf("calls = %+v, want only sfm and train", calls)
	}
	if calls[0].Args[2] != frames {
		t.Fatalf("sfm frame folder = %q, want %q", calls[0].Args[2], frames)
	}
}

func TestRunAcceptsCompletedNonMetricSFMVerdict(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	frames := filepath.Join(root, "frames")
	if err := os.Mkdir(frames, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "result")
	fake := successfulFake(t, []byte("ply"))
	original := fake.run
	fake.run = func(ctx context.Context, invocation Invocation, index int) error {
		if invocation.Args[0] == "sfm" {
			if err := original(ctx, invocation, index); err != nil {
				return err
			}
			return fakeExitError(4)
		}
		return original(ctx, invocation, index)
	}

	result, err := (Runner{Executor: fake}).Run(context.Background(), Request{
		InputPath: frames, OutputDir: output, Iterations: 3,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Artifact == nil || len(fake.Calls()) != 2 {
		t.Fatalf("result = %+v calls = %+v", result, fake.Calls())
	}
}

func TestRunQualityGateStopsBeforeTraining(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	frames := filepath.Join(root, "frames")
	if err := os.Mkdir(frames, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "result")
	fake := &fakeExecutor{run: func(_ context.Context, invocation Invocation, _ int) error {
		_, _ = io.WriteString(invocation.Stdout,
			"Mapping: done   Registered: 40/100 images   Points: 500   Cameras: 1\n"+
				"Reprojection error: mean 0.40 px, median 0.30 px, over 100 observations\n")
		return nil
	}}

	result, err := (Runner{Executor: fake}).Run(context.Background(), Request{
		InputPath: frames, OutputDir: output,
	})
	if !errors.Is(err, ErrQualityGate) {
		t.Fatalf("Run() error = %v, want ErrQualityGate", err)
	}
	if result.Quality.RegisteredPercent != 40 || result.Quality.AcceptedForTraining {
		t.Fatalf("quality = %+v", result.Quality)
	}
	if len(fake.Calls()) != 1 {
		t.Fatalf("calls = %d, training must not start", len(fake.Calls()))
	}
	assertNoPublishedOrPartial(t, output)
}

func TestRunCancellationCleansPartialTrainingArtifact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	frames := filepath.Join(root, "frames")
	if err := os.Mkdir(frames, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "result")
	trainStarted := make(chan struct{})
	fake := &fakeExecutor{}
	fake.run = func(ctx context.Context, invocation Invocation, _ int) error {
		if invocation.Args[0] == "sfm" {
			_, _ = io.WriteString(invocation.Stdout,
				"Mapping: done   Registered: 100/100 images   Points: 500   Cameras: 1\n"+
					"Reprojection error: mean 0.50 px, median 0.40 px, over 100 observations\n")
			return nil
		}
		prefix := argumentValue(t, invocation.Args, "--output-dir-prefix")
		partial := filepath.Join(prefix, "run", "step-000030000.ckpt", "splat.ply")
		if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(partial, []byte("partial"), 0o600); err != nil {
			return err
		}
		close(trainStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (Runner{Executor: fake}).Run(ctx, Request{InputPath: frames, OutputDir: output})
		done <- err
	}()
	<-trainStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	assertNoPublishedOrPartial(t, output)
}

func TestParseQualitySignalResultFallback(t *testing.T) {
	t.Parallel()
	signal, err := ParseQualitySignal("RESULT: PARTIAL -- 73% of the images registered, 1.25 px mean reprojection. Check capture.\n")
	if err != nil {
		t.Fatal(err)
	}
	if signal.RegisteredPercent != 73 || signal.MeanReprojectionPX != 1.25 {
		t.Fatalf("signal = %+v", signal)
	}
}

func successfulFake(t *testing.T, ply []byte) *fakeExecutor {
	t.Helper()
	fake := &fakeExecutor{}
	fake.run = func(_ context.Context, invocation Invocation, _ int) error {
		if invocation.Args[0] == "sfm" {
			_, _ = io.WriteString(invocation.Stdout,
				"Mapping: done   Registered: 9/10 images   Points: 500   Cameras: 1\n"+
					"Reprojection error: mean 0.50 px, median 0.40 px, over 100 observations\n")
			return nil
		}
		prefix := argumentValue(t, invocation.Args, "--output-dir-prefix")
		iterations, err := strconvAtoi(argumentValue(t, invocation.Args, "--num-iterations"))
		if err != nil {
			return err
		}
		artifact := filepath.Join(prefix, "run", fmt.Sprintf("step-%09d.ckpt", iterations), "splat.ply")
		if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
			return err
		}
		return os.WriteFile(artifact, ply, 0o600)
	}
	return fake
}

func argumentValue(t *testing.T, args []string, name string) string {
	t.Helper()
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("argument %q not found in %v", name, args)
	return ""
}

func assertSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	for i := 0; i+len(want) <= len(got); i++ {
		if reflect.DeepEqual(got[i:i+len(want)], want) {
			return
		}
	}
	t.Errorf("%v does not contain contiguous subsequence %v", got, want)
}

func assertNoPublishedOrPartial(t *testing.T, output string) {
	t.Helper()
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output path exists after failed run: err=%v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(output), "."+filepath.Base(output)+".*-"+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("private partial paths remain: %s", strings.Join(matches, ", "))
	}
}

func strconvAtoi(value string) (int, error) {
	var n int
	_, err := fmt.Sscanf(value, "%d", &n)
	return n, err
}

type fakeExitError int

func (e fakeExitError) Error() string { return fmt.Sprintf("exit status %d", e) }
func (e fakeExitError) ExitCode() int { return int(e) }
