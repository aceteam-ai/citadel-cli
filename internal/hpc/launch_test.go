package hpc

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata golden files")

func testJobSpec() JobSpec {
	return JobSpec{
		Partition:   "gpu",
		Account:     "lab-a",
		GPUs:        2,
		GPUType:     "a100",
		WallTime:    "02:00:00",
		CPUs:        16,
		Memory:      "128G",
		Model:       "Qwen/Qwen2.5-7B-Instruct",
		VenvDir:     "/staging/lab-a/venvs/vllm",
		HFHome:      "/scratch/lab-a/hf",
		DownloadDir: "/scratch/lab-a/weights",
		VLLMArgs:    []string{"--max-model-len=8192"},
		TokenFile:   "/home/user/.citadel/hpc-token",
		OutputDir:   "/home/user/hpc-logs",
		CitadelBin:  "/home/user/bin/citadel",
		AuthService: "https://platform.example",
		StateRoot:   "/scratch/lab-a/citadel state",
	}
}

func TestRenderSbatchGolden(t *testing.T) {
	spec, err := testJobSpec().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	got := RenderSbatch(spec)
	path := filepath.Join("testdata", "launch.sbatch.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-golden to create): %v", err)
	}
	if got != string(want) {
		t.Fatalf("sbatch script differs from golden:\n%s", got)
	}
	for _, must := range []string{"#SBATCH --signal=B:USR1@300", "exec '/home/user/bin/citadel' 'hpc' 'run'", "'/scratch/lab-a/citadel state'"} {
		if !strings.Contains(got, must) {
			t.Errorf("script lacks %q", must)
		}
	}
}

func TestRenderSbatchQuotesAwkwardValues(t *testing.T) {
	spec := testJobSpec()
	spec.TokenFile = "/home/user/it's a token"
	spec.GPUType = ""
	spec, err := spec.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	got := RenderSbatch(spec)
	if !strings.Contains(got, `'/home/user/it'\''s a token'`) || !strings.Contains(got, "#SBATCH --gres=gpu:2\n") {
		t.Fatalf("quoting or gres wrong:\n%s", got)
	}
}

func TestJobSpecNormalizeRefusals(t *testing.T) {
	cases := map[string]func(*JobSpec){
		"no partition":           func(s *JobSpec) { s.Partition = "" },
		"shell in account":       func(s *JobSpec) { s.Account = "a;rm -rf /" },
		"zero gpus":              func(s *JobSpec) { s.GPUs = 0 },
		"bad wall time":          func(s *JobSpec) { s.WallTime = "2h" },
		"relative venv":          func(s *JobSpec) { s.VenvDir = "venv" },
		"newline in path":        func(s *JobSpec) { s.HFHome = "/x\n#SBATCH --x" },
		"space in output dir":    func(s *JobSpec) { s.OutputDir = "/home/user/my logs" },
		"plain http auth":        func(s *JobSpec) { s.AuthService = "http://platform.example" },
		"host override":          func(s *JobSpec) { s.VLLMArgs = []string{"--host=0.0.0.0"} },
		"port override":          func(s *JobSpec) { s.VLLMArgs = []string{"--port", "8000"} },
		"drain lead too short":   func(s *JobSpec) { s.DrainLead = 90 * time.Second },
		"drain lead beyond wall": func(s *JobSpec) { s.WallTime = "4"; s.DrainLead = 5 * time.Minute },
		"bad memory":             func(s *JobSpec) { s.Memory = "lots" },
		"abbreviated host":       func(s *JobSpec) { s.VLLMArgs = []string{"--hos=0.0.0.0"} },
		"abbreviated host again": func(s *JobSpec) { s.VLLMArgs = []string{"--h", "0.0.0.0"} },
		"underscore ssl keyfile": func(s *JobSpec) { s.VLLMArgs = []string{"--ssl_keyfile=/k"} },
		"underscore download":    func(s *JobSpec) { s.VLLMArgs = []string{"--download_dir=/tmp/x"} },
		"config file":            func(s *JobSpec) { s.VLLMArgs = []string{"--config=/x.yaml"} },
		"abbreviated config":     func(s *JobSpec) { s.VLLMArgs = []string{"--conf", "/x.yaml"} },
		"bare separator":         func(s *JobSpec) { s.VLLMArgs = []string{"--"} },
		"negative drain timeout": func(s *JobSpec) { s.DrainTimeout = -time.Second },
		"negative ready timeout": func(s *JobSpec) { s.ReadyTimeout = -time.Second },
		"model reads as a flag":  func(s *JobSpec) { s.Model = "--trust-remote-code" },
	}
	for name, mutate := range cases {
		spec := testJobSpec()
		mutate(&spec)
		if _, err := spec.Normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseRuntime(t *testing.T) {
	if r, err := ParseRuntime(""); err != nil || r != RuntimeVenv {
		t.Fatalf("default runtime: %v %v", r, err)
	}
	for _, r := range []string{"podman", "apptainer"} {
		if _, err := ParseRuntime(r); !errors.Is(err, ErrRuntimeNotImplemented) {
			t.Fatalf("%s: %v", r, err)
		}
	}
	if _, err := ParseRuntime("docker"); err == nil || errors.Is(err, ErrRuntimeNotImplemented) {
		t.Fatalf("docker: %v", err)
	}
}

func TestParseWallTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30": 30 * time.Minute, "10:30": 10*time.Minute + 30*time.Second, "02:00:00": 2 * time.Hour,
		"1-00": 24 * time.Hour, "1-02:30": 26*time.Hour + 30*time.Minute, "2-00:00:10": 48*time.Hour + 10*time.Second,
	} {
		got, err := ParseWallTime(in)
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "2h", "1:2:3:4", "-1"} {
		if _, err := ParseWallTime(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReservedFlagAllowsOrdinaryVLLMFlags(t *testing.T) {
	for _, ok := range []string{"--max-model-len=4096", "--max_model_len=4096", "--gpu-memory-utilization=0.9", "--model-impl=auto", "--dtype", "4096", "--served-model-name=x"} {
		if r := reservedFlag(ok); r != "" {
			t.Errorf("%q wrongly refused as %s", ok, r)
		}
	}
}
