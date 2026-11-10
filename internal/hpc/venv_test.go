//go:build !windows

package hpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeVenvExec records argv and simulates the venv on disk.
type fakeVenvExec struct {
	dir       string
	installed string
	calls     [][]string
	failOn    string
}

func (f *fakeVenvExec) run(_ context.Context, argv []string) ([]byte, error) {
	f.calls = append(f.calls, argv)
	joined := strings.Join(argv, " ")
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		return []byte("boom"), errors.New("exit status 1")
	}
	switch {
	case strings.Contains(joined, "import vllm"):
		if f.installed == "" {
			return []byte("ModuleNotFoundError"), errors.New("exit status 1")
		}
		return []byte(f.installed + "\n"), nil
	case strings.HasSuffix(joined, " "+f.dir) && (strings.Contains(joined, " venv")):
		_ = os.MkdirAll(filepath.Join(f.dir, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(f.dir, "bin", "python"), nil, 0o755)
		_ = os.WriteFile(filepath.Join(f.dir, "pyvenv.cfg"), nil, 0o644)
	case strings.Contains(joined, "vllm=="):
		f.installed = strings.TrimPrefix(argv[len(argv)-1], "vllm==")
	}
	return nil, nil
}

func noUV(string) (string, error) { return "", errors.New("not found") }

func TestVenvSetupPipPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vllm")
	f := &fakeVenvExec{dir: dir}
	got, err := VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Python: "python3.12", Exec: f.run, LookPath: noUV}.Run(context.Background())
	if err != nil || got != "0.11.0" {
		t.Fatalf("got %q, %v", got, err)
	}
	py := filepath.Join(dir, "bin", "python")
	want := [][]string{
		{"python3.12", "-m", "venv", dir},
		{py, "-m", "pip", "install", "--upgrade", "pip"},
		{py, "-m", "pip", "install", "vllm==0.11.0"},
		{py, "-c", "import vllm; print(vllm.__version__)"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("argv = %v\nwant %v", f.calls, want)
	}
}

func TestVenvSetupUVPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vllm")
	f := &fakeVenvExec{dir: dir}
	lookUV := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	if _, err := (VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Exec: f.run, LookPath: lookUV}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(dir, "bin", "python")
	if !reflect.DeepEqual(f.calls[:2], [][]string{
		{"/usr/bin/uv", "venv", "--allow-existing", "--python", "python3", dir},
		{"/usr/bin/uv", "pip", "install", "--python", py, "vllm==0.11.0"},
	}) {
		t.Fatalf("argv = %v", f.calls)
	}
}

func TestVenvSetupExistingAndCheck(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bin", "python"), nil, 0o755)
	f := &fakeVenvExec{dir: dir, installed: "0.11.0"}
	if _, err := (VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Exec: f.run, CheckOnly: true}).Run(context.Background()); err != nil {
		t.Fatalf("matching venv: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("a valid venv must not be rebuilt: %v", f.calls)
	}
	f.installed = "0.10.0"
	if _, err := (VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Exec: f.run}).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "0.10.0") {
		t.Fatalf("version mismatch: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "absent")
	if _, err := (VenvSetup{Dir: empty, Exec: f.run, CheckOnly: true}).Run(context.Background()); err == nil {
		t.Fatal("check of a missing venv passed")
	}
}

func TestVenvSetupRefusals(t *testing.T) {
	foreign := t.TempDir()
	_ = os.WriteFile(filepath.Join(foreign, "notes.txt"), []byte("x"), 0o644)
	f := &fakeVenvExec{dir: foreign}
	if _, err := (VenvSetup{Dir: foreign, Exec: f.run, LookPath: noUV}).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "not a virtualenv") {
		t.Fatalf("foreign dir: %v", err)
	}
	if _, err := (VenvSetup{Dir: "rel", Exec: f.run}).Run(context.Background()); err == nil {
		t.Fatal("relative dir accepted")
	}
	if _, err := (VenvSetup{Dir: t.TempDir(), VLLMVersion: "latest", Exec: f.run}).Run(context.Background()); err == nil {
		t.Fatal("unpinned version accepted")
	}
	dir := filepath.Join(t.TempDir(), "v")
	f = &fakeVenvExec{dir: dir, failOn: "vllm=="}
	if _, err := (VenvSetup{Dir: dir, Exec: f.run, LookPath: noUV}).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("install failure: %v", err)
	}
}

func TestVenvSetupEnvKeepsCachesBesideVenvAndNeverDownloadsPython(t *testing.T) {
	env := VenvSetup{Dir: "/scratch/lab/venv"}.Env()
	if env["UV_CACHE_DIR"] != "/scratch/lab/venv.cache/uv" || env["PIP_CACHE_DIR"] != "/scratch/lab/venv.cache/pip" || env["UV_PYTHON_DOWNLOADS"] != "never" {
		t.Fatalf("env = %v", env)
	}
}
