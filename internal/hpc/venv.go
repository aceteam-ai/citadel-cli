package hpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultVLLMVersion is the vLLM release the v1 venv pins. Override with
// --vllm-version; confirm the pin on the target site before a demo.
const DefaultVLLMVersion = "0.11.0"

var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}([a-z]+[0-9]*)?$`)

// VenvSetup describes one `citadel hpc setup` run.
type VenvSetup struct {
	Dir         string
	VLLMVersion string
	Python      string // interpreter used to create the venv
	CheckOnly   bool
	// Exec runs argv and returns combined output. Injected so setup is fully
	// testable offline.
	Exec func(ctx context.Context, argv []string) ([]byte, error)
	// LookPath reports whether a tool (uv) is on PATH.
	LookPath func(name string) (string, error)
}

// VenvPython is the venv interpreter.
func VenvPython(dir string) string { return filepath.Join(dir, "bin", "python") }

// versionProbe prints the installed vLLM version.
func versionProbe(dir string) []string {
	return []string{VenvPython(dir), "-c", "import vllm; print(vllm.__version__)"}
}

// CreatePlan returns the argv sequence that builds the venv: uv when present,
// else python -m venv plus pip.
func (v VenvSetup) CreatePlan(uvPath string) [][]string {
	py := VenvPython(v.Dir)
	pin := "vllm==" + v.VLLMVersion
	if uvPath != "" {
		return [][]string{
			{uvPath, "venv", "--allow-existing", "--python", v.Python, v.Dir},
			{uvPath, "pip", "install", "--python", py, pin},
		}
	}
	return [][]string{
		{v.Python, "-m", "venv", v.Dir},
		{py, "-m", "pip", "install", "--upgrade", "pip"},
		{py, "-m", "pip", "install", pin},
	}
}

// Env is the extra environment for every setup command. Caches live beside
// the venv (on the same scratch filesystem), not in a small home quota, and
// uv never downloads an interpreter: the pin covers vLLM only, so the Python
// it runs on is the cluster's own.
func (v VenvSetup) Env() map[string]string {
	cache := v.Dir + ".cache"
	return map[string]string{
		"UV_CACHE_DIR":        filepath.Join(cache, "uv"),
		"PIP_CACHE_DIR":       filepath.Join(cache, "pip"),
		"UV_PYTHON_DOWNLOADS": "never",
	}
}

// Run validates an existing venv, or builds it unless CheckOnly. It returns
// the installed version.
func (v VenvSetup) Run(ctx context.Context) (string, error) {
	if v.VLLMVersion == "" {
		v.VLLMVersion = DefaultVLLMVersion
	}
	if v.Python == "" {
		v.Python = "python3"
	}
	if !versionPattern.MatchString(v.VLLMVersion) {
		return "", fmt.Errorf("--vllm-version %q is not a release version", v.VLLMVersion)
	}
	if !filepath.IsAbs(v.Dir) {
		return "", errors.New("--venv must be an absolute path")
	}
	if v.Exec == nil {
		return "", errors.New("venv setup has no exec function")
	}
	if got, err := v.installedVersion(ctx); err == nil {
		if got == v.VLLMVersion {
			return got, nil
		}
		if v.CheckOnly {
			return "", fmt.Errorf("venv %s has vLLM %s, want %s", v.Dir, got, v.VLLMVersion)
		}
		return "", fmt.Errorf("venv %s has vLLM %s, want %s; remove it or choose another --venv", v.Dir, got, v.VLLMVersion)
	}
	if v.CheckOnly {
		return "", fmt.Errorf("venv %s does not have vLLM %s", v.Dir, v.VLLMVersion)
	}
	if err := refuseForeignDir(v.Dir); err != nil {
		return "", err
	}
	uv := ""
	if v.LookPath != nil {
		if p, err := v.LookPath("uv"); err == nil {
			uv = p
		}
	}
	for _, argv := range v.CreatePlan(uv) {
		if out, err := v.Exec(ctx, argv); err != nil {
			return "", fmt.Errorf("%s: %w: %s", strings.Join(argv[:2], " "), err, tail(out))
		}
	}
	got, err := v.installedVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("venv built but vLLM does not import: %w", err)
	}
	if got != v.VLLMVersion {
		return "", fmt.Errorf("venv built with vLLM %s, want %s", got, v.VLLMVersion)
	}
	return got, nil
}

func (v VenvSetup) installedVersion(ctx context.Context) (string, error) {
	if _, err := os.Stat(VenvPython(v.Dir)); err != nil {
		return "", err
	}
	out, err := v.Exec(ctx, versionProbe(v.Dir))
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, tail(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// refuseForeignDir refuses to build into a non-empty directory that is not
// already a venv, so setup never writes over unrelated files.
func refuseForeignDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "pyvenv.cfg")); err == nil {
		return nil
	}
	return fmt.Errorf("%s is not empty and is not a virtualenv", dir)
}

func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}
