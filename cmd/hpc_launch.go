package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/spf13/cobra"
)

// The launcher (aceteam#12718). `citadel hpc launch` and `citadel hpc setup`
// run on a login node with no profile and no fabric contact. `citadel hpc
// run` is the job body: it runs inside an allocation under the hpc profile,
// either from the generated batch script or by hand in an interactive one.

// isHPCLoginCommand reports the hpc commands that run outside an allocation.
func isHPCLoginCommand(path string) bool {
	return path == "citadel hpc launch" || path == "citadel hpc setup"
}

var (
	hpcLaunchSpec      hpc.JobSpec
	hpcLaunchRuntime   string
	hpcLaunchPrint     bool
	hpcLaunchScriptDir string

	hpcRunSpec    hpc.JobSpec
	hpcRunRuntime string

	hpcSetupVenv    string
	hpcSetupVersion string
	hpcSetupPython  string
	hpcSetupCheck   bool
)

// hpcLaunchDeps are the login-node edges of a launch.
type hpcLaunchDeps struct {
	UID         int
	Exec        func(ctx context.Context, argv []string) ([]byte, error)
	WriteScript func(dir, name, content string) (string, error)
	Now         func() time.Time
}

// launchHPCJob validates the spec, renders the batch script and submits it.
// It returns the script and, when submitted, the Slurm job id.
func launchHPCJob(ctx context.Context, spec hpc.JobSpec, scriptDir string, submit bool, d hpcLaunchDeps) (script, jobID string, err error) {
	spec, err = spec.Normalize()
	if err != nil {
		return "", "", err
	}
	if spec.TokenFile != "" {
		if err := hpc.CheckTokenFile(spec.TokenFile, d.UID); err != nil {
			return "", "", err
		}
	}
	if _, err := os.Stat(hpc.VenvPython(spec.VenvDir)); err != nil {
		return "", "", fmt.Errorf("venv %s has no python; build it first with 'citadel hpc setup --venv %s'", spec.VenvDir, spec.VenvDir)
	}
	script = hpc.RenderSbatch(spec)
	if !submit {
		return script, "", nil
	}
	if scriptDir == "" {
		scriptDir = spec.OutputDir
	}
	name := fmt.Sprintf("citadel-hpc-%d.sbatch", d.Now().Unix())
	path, err := d.WriteScript(scriptDir, name, script)
	if err != nil {
		return script, "", err
	}
	out, err := d.Exec(ctx, []string{"sbatch", "--parsable", path})
	if err != nil {
		return script, "", fmt.Errorf("sbatch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// --parsable prints "<jobid>[;<cluster>]".
	jobID = strings.SplitN(strings.TrimSpace(string(out)), ";", 2)[0]
	if _, convErr := strconv.ParseUint(jobID, 10, 64); convErr != nil {
		return script, "", fmt.Errorf("sbatch returned an unexpected job id %q", jobID)
	}
	return script, jobID, nil
}

func writeHPCScript(dir, name, content string) (string, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

func execCombined(ctx context.Context, argv []string) ([]byte, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
}

// resolvedSelf is this binary's absolute path, for the batch script and the
// worker child.
func resolvedSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}

// mergeEnv returns base with every key in extra set (replacing earlier values).
func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if _, replaced := extra[k]; !replaced {
			out = append(out, kv)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// hpcWorkerCommand is the worker child: `citadel work --profile hpc` with the
// engine's per-job port in the environment, so the inference handler and the
// heartbeat probe use the external-vLLM endpoint (services.VLLMHostPort).
func hpcWorkerCommand(self string, spec hpc.JobSpec, port int, base []string) ([]string, []string) {
	argv := []string{self, "work", "--profile", hpc.Profile}
	if spec.TokenFile != "" {
		argv = append(argv, "--hpc-token-file", spec.TokenFile)
	} else {
		argv = append(argv, "--hpc-org", spec.OrgID, "--hpc-approval-timeout", spec.ApprovalTimeout.String())
	}
	argv = append(argv,
		"--hpc-drain-timeout", spec.DrainTimeout.String(),
		"--auth-service", spec.AuthService,
	)
	if spec.StateRoot != "" {
		argv = append(argv, "--hpc-state-root", spec.StateRoot)
	}
	if spec.CACert != "" {
		argv = append(argv, "--ca-cert", spec.CACert)
	}
	noProxy := hpc.LoopbackNoProxy(envValue(base, "NO_PROXY"))
	env := mergeEnv(base, map[string]string{
		envProfile:               hpc.Profile,
		"CITADEL_VLLM_HOST":      "127.0.0.1",
		"CITADEL_VLLM_HOST_PORT": strconv.Itoa(port),
		"NO_PROXY":               noProxy,
		"no_proxy":               noProxy,
		"CITADEL_NO_AUTO_UPDATE": "1",
		hpc.EnvStateRoot:         spec.StateRoot,
	})
	return argv, env
}

// hpcEngineEnv is the model server's environment.
func hpcEngineEnv(base []string, extra map[string]string) []string {
	noProxy := hpc.LoopbackNoProxy(envValue(base, "NO_PROXY"))
	merged := map[string]string{"NO_PROXY": noProxy, "no_proxy": noProxy}
	for k, v := range extra {
		merged[k] = v
	}
	return mergeEnv(base, merged)
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], key+"=") {
			return env[i][len(key)+1:]
		}
	}
	return ""
}

// hpcJobBodySupervisor builds the production supervisor for `hpc run`.
func hpcJobBodySupervisor(spec hpc.JobSpec, layout hpc.Layout, self string, sigs <-chan os.Signal) (hpc.Supervisor, func(), error) {
	logPath := filepath.Join(layout.Root, "vllm.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return hpc.Supervisor{}, nil, err
	}
	engine := hpc.EngineSpec{
		VenvDir:        spec.VenvDir,
		Model:          spec.Model,
		HFHome:         spec.HFHome,
		DownloadDir:    spec.DownloadDir,
		TensorParallel: hpc.TensorParallelFor(os.Getenv(hpc.EnvVisibleGPUs), os.Getenv("SLURM_GPUS_ON_NODE")),
		Args:           spec.VLLMArgs,
		CacheRoot:      filepath.Join(layout.Root, "cache"),
	}
	if err := engine.EnsureCacheDirs(); err != nil {
		logFile.Close()
		return hpc.Supervisor{}, nil, err
	}
	logf := func(format string, args ...any) { Log(format, args...); fmt.Printf("   - "+format+"\n", args...) }
	sup := hpc.Supervisor{
		PickPort: hpc.PickLoopbackPort,
		StartEngine: func(port int) (hpc.Process, error) {
			argv, extra := engine.Command(port)
			return hpc.StartProcess(argv, hpcEngineEnv(os.Environ(), extra), logFile, true)
		},
		WaitReady: func(ctx context.Context, port int, done <-chan error) error {
			return hpc.WaitModelsReadyOwned(ctx, "http://127.0.0.1:"+strconv.Itoa(port), 2*time.Second, done, hpc.CheckListenerOwner(port, os.Geteuid()))
		},
		StartWorker: func(port int) (hpc.Process, error) {
			argv, env := hpcWorkerCommand(self, spec, port, os.Environ())
			return hpc.StartProcess(argv, env, os.Stdout, false)
		},
		StopEngine:   func(p hpc.Process) { hpc.StopGroup(p, 15*time.Second) },
		Signals:      sigs,
		IsDrain:      isHPCDrainSignal,
		ReadyTimeout: spec.ReadyTimeout,
		DrainWait:    spec.DrainTimeout + hpcDeregisterTimeout + 15*time.Second,
		TermWait:     hpcDeregisterTimeout + 5*time.Second,
		Log:          logf,
	}
	return sup, func() { logFile.Close() }, nil
}

func isHPCDrainSignal(sig os.Signal) bool {
	for _, d := range hpcDrainSignals {
		if sig == d {
			return true
		}
	}
	return false
}

// normalizeHPCRunSpec validates the in-job flags. Slurm placement fields are
// not used inside the job, so fixed placeholders satisfy the shared checks.
func normalizeHPCRunSpec(spec hpc.JobSpec) (hpc.JobSpec, error) {
	spec.Partition, spec.Account, spec.GPUs, spec.WallTime = "job", "job", 1, "7-00:00:00"
	if spec.OutputDir == "" {
		spec.OutputDir = "/"
	}
	if spec.CitadelBin == "" {
		spec.CitadelBin = "/"
	}
	return spec.Normalize()
}

var hpcLaunchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Generate and submit a batch job that serves a model on this cluster",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		spec := hpcLaunchSpec
		rt, err := hpc.ParseRuntime(hpcLaunchRuntime)
		if err != nil {
			fail(err)
		}
		spec.Runtime = rt
		// The token is consumed by whichever platform the job contacts, so the
		// platform must be named explicitly rather than taken from the default.
		if err := requireExplicitAuthService(cmd); err != nil {
			fail(err)
		}
		spec.AuthService = authServiceURL
		spec.CACert = caCertPath
		spec.StateRoot = hpcStateRootFlag
		if spec.CitadelBin == "" {
			if spec.CitadelBin, err = resolvedSelf(); err != nil {
				fail(err)
			}
		}
		if spec.OutputDir == "" {
			if spec.OutputDir, err = os.Getwd(); err != nil {
				fail(err)
			}
		}
		script, jobID, err := launchHPCJob(contextOrBackground(cmd.Context()), spec, hpcLaunchScriptDir, !hpcLaunchPrint, hpcLaunchDeps{
			UID: os.Geteuid(), Exec: execCombined, WriteScript: writeHPCScript, Now: time.Now,
		})
		if err != nil {
			fail(err)
		}
		if hpcLaunchPrint {
			fmt.Print(script)
			return
		}
		fmt.Printf("Submitted batch job %s\n", jobID)
	},
}

var hpcRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Job body: serve a model and run the worker inside this allocation",
	Long: `Runs inside a Slurm allocation (from the generated batch script, or by hand
in an interactive allocation). Starts vLLM on a per-job loopback port, waits
for it, then enrolls and runs the worker under the hpc profile. SIGUSR1
drains, deregisters, stops vLLM and exits 0; a worker that errors or has to be
killed exits non-zero.

HF_HOME is set to job scratch, so a token saved by 'hf login' in your home is
not seen: export HF_TOKEN before submitting if the model is gated. Runtime
caches (vLLM, Triton, XDG) also live on job scratch.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		spec := hpcRunSpec
		rt, err := hpc.ParseRuntime(hpcRunRuntime)
		if err != nil {
			fail(err)
		}
		spec.Runtime = rt
		if err := requireExplicitAuthService(cmd); err != nil {
			fail(err)
		}
		spec.AuthService = authServiceURL
		spec.CACert = caCertPath
		spec.StateRoot = hpcStateRootFlag
		if spec, err = normalizeHPCRunSpec(spec); err != nil {
			fail(err)
		}
		if _, err := os.Stat(hpc.VenvPython(spec.VenvDir)); err != nil {
			fail(fmt.Errorf("venv %s has no python", spec.VenvDir))
		}
		self, err := resolvedSelf()
		if err != nil {
			fail(err)
		}
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, append([]os.Signal{syscall.SIGINT, syscall.SIGTERM}, hpcDrainSignals...)...)
		defer signal.Stop(sigs)
		sup, closeLog, err := hpcJobBodySupervisor(spec, currentHPCLayout(), self, sigs)
		if err != nil {
			fail(err)
		}
		code, err := sup.Run(contextOrBackground(cmd.Context()))
		closeLog()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(code)
	},
}

var hpcSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Build or check the pinned vLLM virtualenv used by hpc jobs",
	Long: `Builds a virtualenv with the requested vLLM version, or checks one. The pin is
partial: it fixes the vLLM release only, and its dependencies (torch and the
rest) resolve to whatever is current when the venv is built. Rebuild rather
than reuse a venv across sites. uv never downloads an interpreter, and package
caches are kept beside the venv.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		setup := hpc.VenvSetup{
			Dir: hpcSetupVenv, VLLMVersion: hpcSetupVersion, Python: hpcSetupPython, CheckOnly: hpcSetupCheck,
			LookPath: exec.LookPath,
		}
		env := setup.Env()
		setup.Exec = func(ctx context.Context, argv []string) ([]byte, error) {
			c := exec.CommandContext(ctx, argv[0], argv[1:]...)
			c.Env = mergeEnv(os.Environ(), env)
			return c.CombinedOutput()
		}
		version, err := setup.Run(contextOrBackground(cmd.Context()))
		if err != nil {
			fail(err)
		}
		fmt.Printf("vLLM %s ready in %s\n", version, hpcSetupVenv)
	},
}

// requireExplicitAuthService refuses to fall back to the production default:
// the single-use token is spent at whichever platform the job contacts, so
// the platform must be named by --auth-service or CITADEL_AUTH_HOST.
func requireExplicitAuthService(cmd *cobra.Command) error {
	if !flagChanged(cmd, "auth-service") && os.Getenv("CITADEL_AUTH_HOST") == "" {
		return errors.New("pass --auth-service (the platform that minted the enrollment token or will approve the request)")
	}
	return nil
}

func fail(err error) {
	if errors.Is(err, hpc.ErrRuntimeNotImplemented) {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

// bindHPCModelFlags registers the flags shared by launch and run.
func bindHPCModelFlags(c *cobra.Command, s *hpc.JobSpec, runtime *string) {
	f := c.Flags()
	f.StringVar(runtime, "runtime", string(hpc.RuntimeVenv), "Model server runtime: venv (podman and apptainer are not implemented yet)")
	f.StringVar(&s.VenvDir, "venv", "", "Absolute path of the pinned vLLM virtualenv (see 'citadel hpc setup')")
	f.StringVar(&s.Model, "model", "", "Model to serve (Hugging Face id)")
	f.StringVar(&s.HFHome, "hf-home", "", "Hugging Face cache directory on job scratch")
	f.StringVar(&s.DownloadDir, "download-dir", "", "Weights directory on job scratch")
	f.StringArrayVar(&s.VLLMArgs, "vllm-arg", nil, "Extra vLLM argument (repeatable; --host and --port are fixed)")
	f.StringVar(&s.TokenFile, "token-file", "", "Owner-only file holding the single-use enrollment token (wins over --org when both are given)")
	f.StringVar(&s.OrgID, "org", "", "Organization id: with no --token-file, the job waits for an admin to approve its enrollment")
	f.DurationVar(&s.ApprovalTimeout, "hpc-approval-timeout", hpc.DefaultApprovalTimeout, "How long the job waits for an enrollment approval (counts against the wall time)")
	f.DurationVar(&s.DrainTimeout, "drain-timeout", hpc.DefaultDrainTimeout, "How long in-flight jobs may finish after the drain signal")
	f.DurationVar(&s.ReadyTimeout, "ready-timeout", 30*time.Minute, "How long to wait for the model to download and load")
}

func init() {
	bindHPCModelFlags(hpcLaunchCmd, &hpcLaunchSpec, &hpcLaunchRuntime)
	lf := hpcLaunchCmd.Flags()
	lf.StringVar(&hpcLaunchSpec.JobName, "job-name", "citadel-hpc", "Slurm job name")
	lf.StringVar(&hpcLaunchSpec.Partition, "partition", "", "Slurm partition")
	lf.StringVar(&hpcLaunchSpec.Account, "account", "", "Slurm account")
	lf.IntVar(&hpcLaunchSpec.GPUs, "gpus", 1, "GPUs to request")
	lf.StringVar(&hpcLaunchSpec.GPUType, "gpu-type", "", "GPU gres type (optional, for example a100)")
	lf.StringVar(&hpcLaunchSpec.WallTime, "time", "", "Wall time limit (Slurm format, for example 02:00:00)")
	lf.IntVar(&hpcLaunchSpec.CPUs, "cpus", 0, "CPUs per task (optional)")
	lf.StringVar(&hpcLaunchSpec.Memory, "mem", "", "Memory (optional, for example 64G)")
	lf.BoolVar(&hpcLaunchSpec.Exclusive, "exclusive", false, "Request the whole node (no other users' jobs beside the model server)")
	lf.StringVar(&hpcLaunchSpec.OutputDir, "output-dir", "", "Directory for the job's log (default: current directory)")
	lf.StringVar(&hpcLaunchSpec.CitadelBin, "citadel-bin", "", "citadel binary path on the compute nodes (default: this binary)")
	lf.DurationVar(&hpcLaunchSpec.DrainLead, "drain-lead", hpc.DefaultDrainLead, "Send the drain signal this long before the wall time ends")
	lf.BoolVar(&hpcLaunchPrint, "print", false, "Print the batch script instead of submitting it")
	lf.StringVar(&hpcLaunchScriptDir, "script-dir", "", "Where to write the batch script (default: --output-dir)")

	bindHPCModelFlags(hpcRunCmd, &hpcRunSpec, &hpcRunRuntime)

	sf := hpcSetupCmd.Flags()
	sf.StringVar(&hpcSetupVenv, "venv", "", "Absolute path of the virtualenv to build or check")
	sf.StringVar(&hpcSetupVersion, "vllm-version", hpc.DefaultVLLMVersion, "vLLM version to pin")
	sf.StringVar(&hpcSetupPython, "python", "python3", "Python interpreter used to create the venv")
	sf.BoolVar(&hpcSetupCheck, "check", false, "Only check the venv; never install")

	hpcCmd.AddCommand(hpcLaunchCmd, hpcRunCmd, hpcSetupCmd)
}
