package hpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Process is a started child the supervisor can signal and wait for.
type Process interface {
	// Signal delivers sig (to the whole process group for the engine).
	Signal(sig os.Signal) error
	// Done receives the exit error once, then is closed.
	Done() <-chan error
}

// EngineSpec is the vLLM server the job body runs from the venv runtime.
type EngineSpec struct {
	VenvDir        string
	Model          string
	HFHome         string
	DownloadDir    string
	TensorParallel int
	Args           []string
	// CacheRoot is a directory on job scratch for runtime caches (vLLM,
	// Triton, generic XDG). Empty leaves the caches where they default, which
	// on many sites is a small home quota.
	CacheRoot string
}

// Command returns the argv and the extra environment for the engine bound to
// 127.0.0.1:port. The fixed flags are appended LAST: vLLM's parser keeps the
// final value of a repeated flag, so nothing a user passes can override them
// (spec validation also refuses reserved flags and their abbreviations).
func (e EngineSpec) Command(port int) ([]string, map[string]string) {
	tp := e.TensorParallel
	if tp < 1 {
		tp = 1
	}
	argv := []string{
		VenvPython(e.VenvDir), "-m", "vllm.entrypoints.openai.api_server",
		"--tensor-parallel-size", strconv.Itoa(tp),
	}
	argv = append(argv, e.Args...)
	argv = append(argv,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--model", e.Model,
		"--download-dir", e.DownloadDir,
	)
	env := map[string]string{
		"HF_HOME":             e.HFHome,
		"VLLM_NO_USAGE_STATS": "1",
		"DO_NOT_TRACK":        "1",
	}
	if e.CacheRoot != "" {
		env["VLLM_CACHE_ROOT"] = filepath.Join(e.CacheRoot, "vllm")
		env["TRITON_CACHE_DIR"] = filepath.Join(e.CacheRoot, "triton")
		env["TORCHINDUCTOR_CACHE_DIR"] = filepath.Join(e.CacheRoot, "inductor")
		env["XDG_CACHE_HOME"] = filepath.Join(e.CacheRoot, "xdg")
	}
	return argv, env
}

// TensorParallelFor is the tensor-parallel size for an allocation: the
// number of devices in CUDA_VISIBLE_DEVICES (indexes, UUIDs or MIG ids), else
// SLURM_GPUS_ON_NODE, else 1.
func TensorParallelFor(cudaVisible, slurmGPUsOnNode string) int {
	if n := len(VisibleGPUs(cudaVisible)); n > 0 {
		return n
	}
	v := strings.TrimSpace(slurmGPUsOnNode)
	if i := strings.LastIndexByte(v, ':'); i >= 0 {
		v = v[i+1:]
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 1
}

// EnsureCacheDirs creates the runtime cache directories (owner-only).
func (e EngineSpec) EnsureCacheDirs() error {
	if e.CacheRoot == "" {
		return nil
	}
	for _, d := range []string{"vllm", "triton", "inductor", "xdg"} {
		if err := os.MkdirAll(filepath.Join(e.CacheRoot, d), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// LoopbackNoProxy appends the loopback names to a NO_PROXY value so a local
// model server is never reached through HTTPS_PROXY.
func LoopbackNoProxy(current string) string {
	have := map[string]bool{}
	var parts []string
	for _, p := range strings.Split(current, ",") {
		if p = strings.TrimSpace(p); p != "" && !have[p] {
			have[p] = true
			parts = append(parts, p)
		}
	}
	for _, p := range []string{"127.0.0.1", "localhost", "::1"} {
		if !have[p] {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ",")
}

// PickLoopbackPort returns a currently free TCP port on 127.0.0.1. The
// listener is closed before returning; the engine binds the port next.
func PickLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// WaitModelsReady polls baseURL/v1/models until it answers 200 with an
// OpenAI-shaped list, ctx ends, or engineDone fires.
func WaitModelsReady(ctx context.Context, baseURL string, interval time.Duration, engineDone <-chan error) error {
	return WaitModelsReadyOwned(ctx, baseURL, interval, engineDone, nil)
}

// ListenerCheck inspects who owns the listening socket. listening reports
// whether anything listens yet; a non-nil error is fatal (a foreign owner).
type ListenerCheck func() (listening bool, err error)

// WaitModelsReadyOwned is WaitModelsReady with an ownership check run before
// every probe: the port is free while the engine starts and visible to other
// users, so a server that is not this user's must never be accepted (or sent
// the enrollment token later). A nil check skips it.
func WaitModelsReadyOwned(ctx context.Context, baseURL string, interval time.Duration, engineDone <-chan error, check ListenerCheck) error {
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		listening := true
		if check != nil {
			var err error
			if listening, err = check(); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/models", nil)
		if err != nil {
			return err
		}
		if !listening {
			// Nothing is bound yet; do not probe (nothing to ask).
		} else if resp, err := client.Do(req); err == nil {
			var body struct {
				Data []json.RawMessage `json:"data"`
			}
			ok := resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil && body.Data != nil
			resp.Body.Close()
			if ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("model server not ready: %w", ctx.Err())
		case err := <-engineDone:
			return fmt.Errorf("model server exited before it was ready: %v", err)
		case <-time.After(interval):
		}
	}
}

// Supervisor runs the job body: model server first, then the worker, and
// owns both lifetimes. Every edge is injected so the sequencing is tested
// with fake processes.
type Supervisor struct {
	PickPort    func() (int, error)
	StartEngine func(port int) (Process, error)
	WaitReady   func(ctx context.Context, port int, engineDone <-chan error) error
	StartWorker func(port int) (Process, error)
	// StopEngine terminates the engine's process group and reaps it.
	StopEngine func(p Process)
	Signals    <-chan os.Signal
	IsDrain    func(os.Signal) bool
	// ReadyTimeout bounds model download and load.
	ReadyTimeout time.Duration
	// DrainWait bounds how long the worker may take after a drain signal
	// (its drain timeout plus deregistration).
	DrainWait time.Duration
	// TermWait bounds how long the worker may take after SIGTERM.
	TermWait time.Duration
	Log      func(format string, args ...any)
}

func (s Supervisor) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Run executes the job body and returns the process exit code. A drain
// before the time limit is a clean exit (0).
func (s Supervisor) Run(ctx context.Context) (int, error) {
	port, err := s.PickPort()
	if err != nil {
		return 1, fmt.Errorf("pick a model server port: %w", err)
	}
	engine, err := s.StartEngine(port)
	if err != nil {
		return 1, fmt.Errorf("start model server: %w", err)
	}
	stopped := false
	stopEngine := func() {
		if !stopped {
			stopped = true
			s.StopEngine(engine)
		}
	}
	defer stopEngine()
	s.logf("model server starting on 127.0.0.1:%d", port)

	// Engine exit is observed by exactly one reader at a time: readiness,
	// then the main loop.
	engineDone := make(chan error, 1)
	go func() { engineDone <- <-engine.Done() }()

	readyCtx, cancelReady := context.WithTimeout(ctx, s.ReadyTimeout)
	readyErr := make(chan error, 1)
	go func() { readyErr <- s.WaitReady(readyCtx, port, engineDone) }()
	select {
	case err := <-readyErr:
		cancelReady()
		if err != nil {
			return 1, err
		}
	case sig := <-s.Signals:
		cancelReady()
		<-readyErr
		s.logf("received %v before the model server was ready; stopping (nothing was enrolled)", sig)
		return 0, nil
	}
	s.logf("model server ready on 127.0.0.1:%d", port)

	worker, err := s.StartWorker(port)
	if err != nil {
		return 1, fmt.Errorf("start worker: %w", err)
	}

	var deadline <-chan time.Time
	for {
		select {
		case err := <-worker.Done():
			stopEngine()
			if err != nil {
				return 1, fmt.Errorf("worker exited: %w", err)
			}
			return 0, nil
		case err := <-engineDone:
			s.logf("model server exited (%v); stopping the worker", err)
			_ = worker.Signal(syscall.SIGTERM)
			s.waitOrKill(worker, s.TermWait)
			return 1, errors.New("model server exited while serving")
		case sig := <-s.Signals:
			// The first signal sets the deadline; a later one is forwarded (the
			// worker deregisters at once on SIGTERM) but never shortens it,
			// so a SIGTERM during a drain does not get the worker killed
			// before it has finished and deregistered.
			if s.IsDrain != nil && s.IsDrain(sig) {
				s.logf("drain signal: asking the worker to finish and deregister")
				if deadline == nil {
					deadline = time.After(s.DrainWait)
				}
			} else {
				s.logf("received %v: asking the worker to deregister and exit", sig)
				if deadline == nil {
					deadline = time.After(s.TermWait)
				}
			}
			_ = worker.Signal(sig)
		case <-deadline:
			s.logf("worker did not exit in time; killing it")
			_ = worker.Signal(syscall.SIGKILL)
			<-worker.Done()
			return 1, errors.New("the worker did not exit in time and was killed; it may not have deregistered")
		}
	}
}

func (s Supervisor) waitOrKill(p Process, wait time.Duration) {
	select {
	case <-p.Done():
	case <-time.After(wait):
		_ = p.Signal(syscall.SIGKILL)
		<-p.Done()
	}
}
