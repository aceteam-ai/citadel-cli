//go:build !windows

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/spf13/cobra"
)

func testLaunchSpec(t *testing.T) hpc.JobSpec {
	t.Helper()
	venv := t.TempDir()
	if err := os.MkdirAll(filepath.Join(venv, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "bin", "python"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("hpcet_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return hpc.JobSpec{
		Partition: "gpu", Account: "lab-a", GPUs: 1, WallTime: "01:00:00",
		Runtime: hpc.RuntimeVenv, VenvDir: venv, Model: "org/model",
		HFHome: "/scratch/hf", DownloadDir: "/scratch/w", TokenFile: token,
		OutputDir: t.TempDir(), CitadelBin: "/opt/citadel/citadel", AuthService: "https://platform.example",
	}
}

func TestLaunchHPCJobSubmitsWithSbatch(t *testing.T) {
	spec := testLaunchSpec(t)
	var argv []string
	script, jobID, err := launchHPCJob(context.Background(), spec, "", true, hpcLaunchDeps{
		UID: os.Getuid(),
		Exec: func(_ context.Context, a []string) ([]byte, error) {
			argv = a
			return []byte("31337;testcluster\n"), nil
		},
		WriteScript: writeHPCScript,
		Now:         func() time.Time { return time.Unix(1700000000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "31337" {
		t.Fatalf("job id %q", jobID)
	}
	path := filepath.Join(spec.OutputDir, "citadel-hpc-1700000000.sbatch")
	if !reflect.DeepEqual(argv, []string{"sbatch", "--parsable", path}) {
		t.Fatalf("sbatch argv %v", argv)
	}
	written, err := os.ReadFile(path)
	if err != nil || string(written) != script || !strings.Contains(script, "--signal=B:USR1@300") {
		t.Fatalf("script not written as rendered: %v", err)
	}
	if _, err := os.Stat(spec.TokenFile); err != nil {
		t.Fatal("launch must not consume the token; the job reads it")
	}
}

func TestLaunchHPCJobRefusesEarly(t *testing.T) {
	never := hpcLaunchDeps{UID: os.Getuid(), Now: time.Now,
		Exec: func(context.Context, []string) ([]byte, error) {
			t.Fatal("sbatch called")
			return nil, nil
		},
		WriteScript: func(string, string, string) (string, error) { t.Fatal("script written"); return "", nil },
	}
	spec := testLaunchSpec(t)
	if err := os.Chmod(spec.TokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := launchHPCJob(context.Background(), spec, "", true, never); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("loose token: %v", err)
	}
	spec = testLaunchSpec(t)
	spec.VenvDir = filepath.Join(t.TempDir(), "missing")
	if _, _, err := launchHPCJob(context.Background(), spec, "", true, never); err == nil || !strings.Contains(err.Error(), "citadel hpc setup") {
		t.Fatalf("missing venv: %v", err)
	}
	spec = testLaunchSpec(t)
	spec.Runtime = hpc.RuntimeApptainer
	if _, _, err := launchHPCJob(context.Background(), spec, "", true, never); !errors.Is(err, hpc.ErrRuntimeNotImplemented) {
		t.Fatalf("apptainer: %v", err)
	}
	// --print renders without writing or submitting.
	if script, _, err := launchHPCJob(context.Background(), testLaunchSpec(t), "", false, never); err != nil || !strings.HasPrefix(script, "#!/bin/bash") {
		t.Fatalf("print: %v", err)
	}
	bad := hpcLaunchDeps{UID: os.Getuid(), Now: time.Now, WriteScript: writeHPCScript,
		Exec: func(context.Context, []string) ([]byte, error) { return []byte("Submitted batch job 1"), nil }}
	if _, _, err := launchHPCJob(context.Background(), testLaunchSpec(t), "", true, bad); err == nil {
		t.Fatal("a non-parsable sbatch reply was accepted")
	}
}

// TestRunArgsParseBackIntoRunFlags proves the batch script's argument list is
// exactly what `citadel hpc run` accepts.
func TestRunArgsParseBackIntoRunFlags(t *testing.T) {
	spec := testLaunchSpec(t)
	spec.VLLMArgs = []string{"--max-model-len=8192", "--enforce-eager"}
	spec.StateRoot = "/scratch/state root"
	spec, err := spec.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	var got hpc.JobSpec
	var runtime, auth, stateRoot, caCert string
	c := &cobra.Command{Use: "run"}
	bindHPCModelFlags(c, &got, &runtime)
	c.Flags().StringVar(&auth, "auth-service", "", "")
	c.Flags().StringVar(&stateRoot, "hpc-state-root", "", "")
	c.Flags().StringVar(&caCert, "ca-cert", "", "")
	args := spec.RunArgs()
	if args[0] != "hpc" || args[1] != "run" {
		t.Fatalf("args %v", args)
	}
	if err := c.ParseFlags(args[2:]); err != nil {
		t.Fatal(err)
	}
	if runtime != "venv" || got.VenvDir != spec.VenvDir || got.Model != spec.Model || got.TokenFile != spec.TokenFile ||
		got.DrainTimeout != spec.DrainTimeout || !reflect.DeepEqual(got.VLLMArgs, spec.VLLMArgs) ||
		auth != spec.AuthService || stateRoot != spec.StateRoot {
		t.Fatalf("round trip lost values: %+v runtime=%s auth=%s root=%s", got, runtime, auth, stateRoot)
	}
}

func TestHPCWorkerCommand(t *testing.T) {
	spec := hpc.JobSpec{TokenFile: "/t", DrainTimeout: 2 * time.Minute, AuthService: "https://platform.example", StateRoot: "/scratch/s"}
	base := []string{"PATH=/usr/bin", "NO_PROXY=corp.example", "CITADEL_VLLM_HOST_PORT=8201", "HTTPS_PROXY=http://proxy.example:3128"}
	argv, env := hpcWorkerCommand("/opt/citadel", spec, 40123, base)
	wantArgv := []string{"/opt/citadel", "work", "--profile", "hpc", "--hpc-token-file", "/t", "--hpc-drain-timeout", "2m0s",
		"--auth-service", "https://platform.example", "--hpc-state-root", "/scratch/s"}
	if !reflect.DeepEqual(argv, wantArgv) {
		t.Fatalf("argv %v", argv)
	}
	for key, want := range map[string]string{
		"CITADEL_VLLM_HOST_PORT": "40123", "CITADEL_VLLM_HOST": "127.0.0.1", "CITADEL_PROFILE": "hpc",
		"HTTPS_PROXY": "http://proxy.example:3128", "NO_PROXY": "corp.example,127.0.0.1,localhost,::1",
	} {
		if got := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "CITADEL_VLLM_HOST_PORT=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("CITADEL_VLLM_HOST_PORT appears %d times", count)
	}
}

func TestHPCLoginCommandsRunWithoutProfile(t *testing.T) {
	for _, p := range []string{"citadel hpc launch", "citadel hpc setup"} {
		if !isHPCLoginCommand(p) || hpcProfileAllowsCommand(p) {
			t.Errorf("%s: login command must not run under the profile", p)
		}
	}
	for _, p := range []string{"citadel hpc run", "citadel hpc enroll", "citadel hpc deregister"} {
		if isHPCLoginCommand(p) || !hpcProfileAllowsCommand(p) {
			t.Errorf("%s: job command must run under the profile", p)
		}
	}
	for _, name := range []string{"launch", "run", "setup", "enroll", "deregister"} {
		if c, _, err := rootCmd.Find([]string{"hpc", name}); err != nil || c.Name() != name {
			t.Errorf("citadel hpc %s is not registered", name)
		}
	}
}

func TestNormalizeHPCRunSpec(t *testing.T) {
	spec := testLaunchSpec(t)
	spec.Partition, spec.Account, spec.WallTime, spec.OutputDir, spec.CitadelBin = "", "", "", "", ""
	if _, err := normalizeHPCRunSpec(spec); err != nil {
		t.Fatalf("job-side spec needs no placement fields: %v", err)
	}
	spec.VLLMArgs = []string{"--host", "0.0.0.0"}
	if _, err := normalizeHPCRunSpec(spec); err == nil {
		t.Fatal("a --host override was accepted inside the job")
	}
}

// TestHPCWorkerChildArgvIsAccepted parses the exact child command line the
// job body runs through the cobra tree and the profile's flag check.
func TestHPCWorkerChildArgvIsAccepted(t *testing.T) {
	spec := hpc.JobSpec{TokenFile: "/t", DrainTimeout: 2 * time.Minute, AuthService: "https://platform.example",
		StateRoot: "/scratch/s", CACert: "/etc/ssl/site-ca.pem"}
	argv, _ := hpcWorkerCommand("/opt/citadel", spec, 40123, nil)
	c, rest, err := rootCmd.Find(argv[1:])
	if err != nil || c.Name() != "work" {
		t.Fatalf("find: %v %v", c, err)
	}
	resetParsedFlags(t, c.Flags(), rootCmd.PersistentFlags())
	if err := c.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	if got := hpcRefusedWorkFlags(c.Flags(), c.InheritedFlags()); len(got) != 0 {
		t.Fatalf("the job body's own worker command is refused: %v", got)
	}
	if hpcTokenFileFlag != "/t" || hpcDrainTimeoutFlag != 2*time.Minute {
		t.Fatalf("flags not parsed: %q %v", hpcTokenFileFlag, hpcDrainTimeoutFlag)
	}
}
