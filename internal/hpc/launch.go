package hpc

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runtime names the way the job body runs the model server (aceteam#12718).
type Runtime string

const (
	// RuntimeVenv runs vLLM from a pinned Python virtualenv. The v1 default
	// and the only runtime implemented.
	RuntimeVenv Runtime = "venv"
	// RuntimePodman and RuntimeApptainer are reserved for later runtimes.
	RuntimePodman    Runtime = "podman"
	RuntimeApptainer Runtime = "apptainer"
)

// ErrRuntimeNotImplemented marks a known runtime that v1 does not run yet.
var ErrRuntimeNotImplemented = errors.New("runtime is not implemented yet; use --runtime venv")

// ParseRuntime validates a --runtime value.
func ParseRuntime(s string) (Runtime, error) {
	switch r := Runtime(strings.ToLower(strings.TrimSpace(s))); r {
	case "", RuntimeVenv:
		return RuntimeVenv, nil
	case RuntimePodman, RuntimeApptainer:
		return "", fmt.Errorf("--runtime %s: %w", r, ErrRuntimeNotImplemented)
	default:
		return "", fmt.Errorf("unknown --runtime %q (supported: venv)", s)
	}
}

// Defaults for the generated job.
const (
	DefaultDrainLead    = 300 * time.Second
	DefaultDrainTimeout = 120 * time.Second
	// drainMargin is the time after the drain timeout reserved for
	// deregistration and stopping the model server before the wall end.
	drainMargin = 60 * time.Second
)

// JobSpec is everything the generated sbatch script and the in-job body need.
type JobSpec struct {
	JobName   string
	Partition string
	Account   string
	GPUs      int
	GPUType   string // optional gres type, for example a100
	WallTime  string // Slurm --time, for example 02:00:00 or 1-00:00:00
	CPUs      int    // optional --cpus-per-task
	Memory    string // optional --mem, for example 64G
	Exclusive bool

	Runtime     Runtime
	VenvDir     string
	Model       string
	HFHome      string // Hugging Face cache on job scratch
	DownloadDir string // vLLM --download-dir (weights) on job scratch
	VLLMArgs    []string

	TokenFile string
	// OrgID selects approve-on-arrival enrollment when TokenFile is empty.
	OrgID           string
	ApprovalTimeout time.Duration
	StateRoot       string // optional job state base (else TMPDIR)
	OutputDir       string // where Slurm writes the job's stdout/stderr
	CitadelBin      string
	AuthService     string
	CACert          string // optional private CA bundle path
	DrainLead       time.Duration
	DrainTimeout    time.Duration
	ReadyTimeout    time.Duration
}

var (
	slurmValuePattern = regexp.MustCompile(`^[A-Za-z0-9._:+-]{1,128}$`)
	wallTimePattern   = regexp.MustCompile(`^(?:(\d{1,3})-)?(\d{1,3})(?::(\d{2}))?(?::(\d{2}))?$`)
	memoryPattern     = regexp.MustCompile(`^[0-9]{1,7}[KMGT]?$`)
	// A model id may not start with '-' so it can never read as a flag.
	modelPattern = regexp.MustCompile(`^[A-Za-z0-9._/:][A-Za-z0-9._/:-]{0,255}$`)
)

// reservedVLLMArgs are set by the job body and may not be overridden: the
// bind address and port keep the model server on a per-job loopback port.
//
// vLLM's parser accepts unambiguous prefixes ("--hos"), maps "_" to "-", and
// merges a --config YAML, so reservedFlag compares the normalized name, refuses
// any prefix of a reserved flag, and --config is refused outright.
var reservedVLLMArgs = []string{"--host", "--port", "--model", "--download-dir", "--uds", "--ssl-keyfile", "--ssl-certfile", "--config"}

// reservedFlag returns the reserved flag arg would set, or "". arg is one
// --vllm-arg value; only values that look like long flags are judged.
func reservedFlag(arg string) string {
	if !strings.HasPrefix(arg, "--") {
		return ""
	}
	name := arg
	if i := strings.IndexByte(arg, '='); i >= 0 {
		name = arg[:i]
	}
	name = "--" + strings.ReplaceAll(name[2:], "_", "-")
	for _, r := range reservedVLLMArgs {
		if strings.HasPrefix(r, name) {
			return r
		}
	}
	return ""
}

// ParseWallTime converts a Slurm --time value to a duration. It accepts
// minutes, MM:SS, HH:MM:SS, D-HH, D-HH:MM and D-HH:MM:SS.
func ParseWallTime(s string) (time.Duration, error) {
	m := wallTimePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("wall time %q is not a Slurm time (for example 02:00:00)", s)
	}
	n := func(i int) int {
		if m[i] == "" {
			return 0
		}
		v, _ := strconv.Atoi(m[i])
		return v
	}
	var d time.Duration
	switch {
	case m[1] != "": // D-HH[:MM[:SS]]
		d = time.Duration(n(1))*24*time.Hour + time.Duration(n(2))*time.Hour + time.Duration(n(3))*time.Minute + time.Duration(n(4))*time.Second
	case m[4] != "": // HH:MM:SS
		d = time.Duration(n(2))*time.Hour + time.Duration(n(3))*time.Minute + time.Duration(n(4))*time.Second
	case m[3] != "": // MM:SS
		d = time.Duration(n(2))*time.Minute + time.Duration(n(3))*time.Second
	default: // minutes
		d = time.Duration(n(2)) * time.Minute
	}
	if d <= 0 {
		return 0, fmt.Errorf("wall time %q must be positive", s)
	}
	return d, nil
}

// Normalize fills defaults and validates the spec. Every value written into
// the script is checked here, so rendering never needs to escape a newline.
func (s JobSpec) Normalize() (JobSpec, error) {
	if s.JobName == "" {
		s.JobName = "citadel-hpc"
	}
	if s.Runtime == "" {
		s.Runtime = RuntimeVenv
	}
	if _, err := ParseRuntime(string(s.Runtime)); err != nil {
		return JobSpec{}, err
	}
	if s.DrainLead == 0 {
		s.DrainLead = DefaultDrainLead
	}
	if s.DrainTimeout == 0 {
		s.DrainTimeout = DefaultDrainTimeout
	}
	if s.ReadyTimeout == 0 {
		s.ReadyTimeout = 30 * time.Minute
	}
	var problems []string
	need := func(name, v string, p *regexp.Regexp) {
		if v == "" {
			problems = append(problems, name+" is required")
		} else if !p.MatchString(v) {
			problems = append(problems, name+" has unsupported characters")
		}
	}
	need("--job-name", s.JobName, slurmValuePattern)
	need("--partition", s.Partition, slurmValuePattern)
	need("--account", s.Account, slurmValuePattern)
	need("--model", s.Model, modelPattern)
	if s.GPUType != "" && !slurmValuePattern.MatchString(s.GPUType) {
		problems = append(problems, "--gpu-type has unsupported characters")
	}
	if s.GPUs < 1 || s.GPUs > 64 {
		problems = append(problems, "--gpus must be between 1 and 64")
	}
	if s.CPUs < 0 || s.CPUs > 1024 {
		problems = append(problems, "--cpus must be between 0 and 1024")
	}
	if s.Memory != "" && !memoryPattern.MatchString(s.Memory) {
		problems = append(problems, "--mem must look like 64G")
	}
	wall, err := ParseWallTime(s.WallTime)
	if err != nil {
		problems = append(problems, err.Error())
	}
	for name, p := range map[string]*string{
		"--venv": &s.VenvDir, "--hf-home": &s.HFHome, "--download-dir": &s.DownloadDir,
		"--output-dir": &s.OutputDir, "--citadel-bin": &s.CitadelBin,
	} {
		if err := checkScriptPath(name, *p, true); err != nil {
			problems = append(problems, err.Error())
		} else {
			*p = filepath.Clean(*p)
		}
	}
	// The token path wins when both are given: the org is not passed on.
	if s.TokenFile != "" {
		s.OrgID, s.ApprovalTimeout = "", 0
		if err := checkScriptPath("--token-file", s.TokenFile, true); err != nil {
			problems = append(problems, err.Error())
		} else {
			s.TokenFile = filepath.Clean(s.TokenFile)
		}
	} else if s.OrgID == "" {
		problems = append(problems, "--token-file or --org is required")
	} else {
		if err := ValidateOrgID(s.OrgID); err != nil {
			problems = append(problems, err.Error())
		}
		s.OrgID = strings.ToLower(s.OrgID)
		if s.ApprovalTimeout == 0 {
			s.ApprovalTimeout = DefaultApprovalTimeout
		}
		if s.ApprovalTimeout < 0 || s.ApprovalTimeout > MaxApprovalTimeout {
			problems = append(problems, "--hpc-approval-timeout must be between 0 and "+MaxApprovalTimeout.String())
		}
	}
	for name, p := range map[string]*string{"--hpc-state-root": &s.StateRoot, "--ca-cert": &s.CACert} {
		if *p == "" {
			continue
		}
		if err := checkScriptPath(name, *p, false); err != nil {
			problems = append(problems, err.Error())
		} else {
			*p = filepath.Clean(*p)
		}
	}
	if strings.ContainsAny(s.OutputDir, " \t") {
		problems = append(problems, "--output-dir must not contain whitespace (Slurm does not quote #SBATCH values)")
	}
	if err := checkPlatformURL(s.AuthService); err != nil {
		problems = append(problems, "--auth-service "+err.Error())
	}
	for _, a := range s.VLLMArgs {
		if strings.ContainsAny(a, "\n\r\x00") {
			problems = append(problems, "--vllm-arg must not contain control characters")
		}
		if r := reservedFlag(a); r != "" {
			problems = append(problems, "--vllm-arg may not set "+r+" (or an abbreviation or underscore spelling of it)")
		}
	}
	if s.DrainTimeout < 0 || s.ReadyTimeout < 0 || s.DrainLead < 0 {
		problems = append(problems, "--drain-timeout, --ready-timeout and --drain-lead must be positive")
	}
	if s.DrainTimeout+drainMargin > s.DrainLead {
		problems = append(problems, fmt.Sprintf("--drain-lead (%s) must exceed --drain-timeout (%s) by at least %s", s.DrainLead, s.DrainTimeout, drainMargin))
	}
	if err == nil && s.DrainLead >= wall {
		problems = append(problems, "--drain-lead must be shorter than the wall time")
	}
	// The approval wait runs inside the allocation after the model server is
	// up, so it spends wall time before any serving starts.
	if err == nil && s.OrgID != "" && s.ApprovalTimeout+s.DrainLead >= wall {
		problems = append(problems, "--time must exceed --hpc-approval-timeout plus --drain-lead (the approval wait runs inside the allocation)")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return JobSpec{}, errors.New("hpc launch: " + strings.Join(problems, "; "))
	}
	return s, nil
}

func checkScriptPath(name, p string, required bool) error {
	if p == "" {
		if required {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s must be an absolute path (the job runs in another directory)", name)
	}
	if strings.ContainsAny(p, "\n\r\x00") {
		return fmt.Errorf("%s must not contain control characters", name)
	}
	return nil
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RunArgs is the `citadel hpc run` argument list for this spec (without the
// binary), shared by the batch script and an interactive allocation.
func (s JobSpec) RunArgs() []string {
	args := []string{"hpc", "run",
		"--runtime", string(s.Runtime),
		"--venv", s.VenvDir,
		"--model", s.Model,
		"--hf-home", s.HFHome,
		"--download-dir", s.DownloadDir,
	}
	if s.TokenFile != "" {
		args = append(args, "--token-file", s.TokenFile)
	} else {
		args = append(args, "--org", s.OrgID, "--hpc-approval-timeout", s.ApprovalTimeout.String())
	}
	args = append(args,
		"--drain-timeout", s.DrainTimeout.String(),
		"--ready-timeout", s.ReadyTimeout.String(),
		"--auth-service", s.AuthService,
	)
	if s.StateRoot != "" {
		args = append(args, "--hpc-state-root", s.StateRoot)
	}
	if s.CACert != "" {
		args = append(args, "--ca-cert", s.CACert)
	}
	for _, a := range s.VLLMArgs {
		args = append(args, "--vllm-arg="+a)
	}
	return args
}

// RenderSbatch returns the batch script for a normalized spec. The batch
// shell execs citadel, so `--signal=B:USR1@N` reaches the job body directly.
func RenderSbatch(s JobSpec) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("#!/bin/bash")
	line("# Generated by citadel hpc launch (aceteam#12718). Edits are not preserved.")
	line("#SBATCH --job-name=%s", s.JobName)
	line("#SBATCH --partition=%s", s.Partition)
	line("#SBATCH --account=%s", s.Account)
	line("#SBATCH --nodes=1")
	line("#SBATCH --ntasks=1")
	if s.GPUType != "" {
		line("#SBATCH --gres=gpu:%s:%d", s.GPUType, s.GPUs)
	} else {
		line("#SBATCH --gres=gpu:%d", s.GPUs)
	}
	if s.CPUs > 0 {
		line("#SBATCH --cpus-per-task=%d", s.CPUs)
	}
	if s.Memory != "" {
		line("#SBATCH --mem=%s", s.Memory)
	}
	if s.Exclusive {
		line("#SBATCH --exclusive")
	}
	line("#SBATCH --time=%s", s.WallTime)
	line("#SBATCH --signal=B:USR1@%d", int(s.DrainLead/time.Second))
	line("#SBATCH --output=%s", filepath.Join(s.OutputDir, "citadel-hpc-%j.out"))
	line("")
	line("set -euo pipefail")
	line("export CITADEL_PROFILE=hpc")
	var cmd []string
	cmd = append(cmd, shellQuote(s.CitadelBin))
	for _, a := range s.RunArgs() {
		cmd = append(cmd, shellQuote(a))
	}
	line("exec %s", strings.Join(cmd, " "))
	return b.String()
}
