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

// fakeVenvExec records argv and simulates the venv on disk. An unbounded
// resolve installs transformers 5, as PyPI does today.
type fakeVenvExec struct {
	dir          string
	installed    string
	transformers string
	calls        [][]string
	failOn       string
	noise        string
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
		return []byte(f.noise + "citadel-probe " + f.installed + " " + f.transformers + "\n"), nil
	case strings.HasSuffix(joined, " "+f.dir) && (strings.Contains(joined, " venv")):
		_ = os.MkdirAll(filepath.Join(f.dir, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(f.dir, "bin", "python"), nil, 0o755)
		_ = os.WriteFile(filepath.Join(f.dir, "pyvenv.cfg"), nil, 0o644)
	case strings.Contains(joined, " install "):
		f.transformers = "5.23.0"
		for _, a := range argv {
			if strings.HasPrefix(a, "vllm==") {
				f.installed = strings.TrimPrefix(a, "vllm==")
			}
			if a == "transformers<5" {
				f.transformers = "4.57.6"
			}
		}
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
		{py, "-m", "pip", "install", "vllm==0.11.0", "transformers<5"},
		{py, "-c", "import vllm, transformers; print('citadel-probe', vllm.__version__, transformers.__version__)"},
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
		{"/usr/bin/uv", "pip", "install", "--python", py, "vllm==0.11.0", "transformers<5"},
	}) {
		t.Fatalf("argv = %v", f.calls)
	}
}

func TestVenvSetupExistingAndCheck(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bin", "python"), nil, 0o755)
	f := &fakeVenvExec{dir: dir, installed: "0.11.0", transformers: "4.57.6"}
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

// A venv built before the companion pin has vLLM 0.11.0 beside transformers 5,
// which imports but cannot load a tokenizer at serve time.
func TestVenvSetupTransformers5(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bin", "python"), nil, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pyvenv.cfg"), nil, 0o644)
	f := &fakeVenvExec{dir: dir, installed: "0.11.0", transformers: "5.23.0"}
	_, err := (VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Exec: f.run, CheckOnly: true}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "transformers 5.23.0") || !strings.Contains(err.Error(), "transformers<5") {
		t.Fatalf("check must refuse transformers 5: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("check must not install: %v", f.calls)
	}
	f.calls = nil
	got, err := (VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Exec: f.run, LookPath: noUV}).Run(context.Background())
	if err != nil || got != "0.11.0" || f.transformers != "4.57.6" {
		t.Fatalf("repair: got %q, %v, transformers %s", got, err, f.transformers)
	}
	py := filepath.Join(dir, "bin", "python")
	if !reflect.DeepEqual(f.calls[1], []string{py, "-m", "pip", "install", "vllm==0.11.0", "transformers<5"}) {
		t.Fatalf("repair argv = %v", f.calls)
	}
}

// A matching Python prefix is not proof that Citadel may install into it.
// Both installer paths must refuse a foreign directory before any mutation.
func TestVenvSetupRepairRefusesForeignPrefix(t *testing.T) {
	for _, installer := range []string{"pip", "uv"} {
		t.Run(installer, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(VenvPython(dir), nil, 0o755); err != nil {
				t.Fatal(err)
			}
			f := &fakeVenvExec{dir: dir, installed: "0.11.0", transformers: "5.23.0"}
			look := noUV
			if installer == "uv" {
				look = func(string) (string, error) { return "/fake/uv", nil }
			}
			notified := false
			v := VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Exec: f.run, LookPath: look,
				Notify: func(string) { notified = true }}
			if _, err := v.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "not a virtualenv") {
				t.Fatalf("foreign repair must be refused, got %v", err)
			}
			if len(f.calls) != 1 || !strings.Contains(strings.Join(f.calls[0], " "), "import vllm") {
				t.Fatalf("only the read-only version probe may run: %v", f.calls)
			}
			if notified || f.transformers != "5.23.0" {
				t.Fatal("foreign prefix was treated as an installation target")
			}
			if _, err := os.Stat(filepath.Join(dir, "pyvenv.cfg")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("foreign prefix was changed: %v", err)
			}
		})
	}
}

func TestVenvSetupCheckForeignPrefixNeverInstalls(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(VenvPython(dir), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeVenvExec{dir: dir, installed: "0.11.0", transformers: "5.23.0"}
	_, err := (VenvSetup{Dir: dir, Exec: f.run, CheckOnly: true}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "transformers 5.23.0") {
		t.Fatalf("check must retain its version diagnosis: %v", err)
	}
	if len(f.calls) != 1 || f.transformers != "5.23.0" {
		t.Fatalf("check mutated the foreign prefix: %v", f.calls)
	}
}

func TestCompanionPins(t *testing.T) {
	for v, want := range map[string]bool{"0.11.0": true, "0.10.2": true, "0.11.1rc1": true, "0.11rc1": true, "0.12.0": false, "1.0.0": false} {
		if got := len(companionPins(v)) > 0; got != want {
			t.Errorf("companionPins(%s) pinned=%v, want %v", v, got, want)
		}
	}
	if !transformersCompatible("0.11.0", "4.57.6") || transformersCompatible("0.11.0", "5.0.0") || !transformersCompatible("0.12.0", "5.23.0") {
		t.Fatal("transformersCompatible")
	}
}

// Warnings on the combined output, such as the TRANSFORMERS_CACHE deprecation
// notice many HPC sites trigger, must not hide a valid venv.
func TestVenvSetupProbeIgnoresWarnings(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bin", "python"), nil, 0o755)
	f := &fakeVenvExec{dir: dir, installed: "0.11.0", transformers: "4.57.6",
		noise: "/x/transformers/utils/hub.py:110: FutureWarning: Using `TRANSFORMERS_CACHE` is deprecated\n  warnings.warn(\n"}
	got, err := (VenvSetup{Dir: dir, VLLMVersion: "0.11.0", Exec: f.run, CheckOnly: true}).Run(context.Background())
	if err != nil || got != "0.11.0" || len(f.calls) != 1 {
		t.Fatalf("got %q, %v, calls %v", got, err, f.calls)
	}
}
