package hpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	// Notify, when set, reports a change made to an existing venv.
	Notify func(msg string)
}

// VenvPython is the venv interpreter.
func VenvPython(dir string) string { return filepath.Join(dir, "bin", "python") }

// probeSentinel starts the one probe line that carries the versions. Python
// warnings and package logging (for example the TRANSFORMERS_CACHE
// deprecation notice) share the combined output, so the parser looks for this
// line instead of trusting the whole stream.
const probeSentinel = "citadel-probe"

// versionProbe prints the installed vLLM and transformers versions on one
// sentinel line.
func versionProbe(dir string) []string {
	return []string{VenvPython(dir), "-c", "import vllm, transformers; print('" + probeSentinel + "', vllm.__version__, transformers.__version__)"}
}

// parseProbe returns the versions from the sentinel line of the probe output.
func parseProbe(out []byte) (string, string, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == probeSentinel {
			return f[1], f[2], true
		}
	}
	return "", "", false
}

// companionPins returns the extra requirements a vLLM release needs installed
// beside it. vLLM through 0.11.x declares transformers without an upper bound
// but cannot load tokenizers under transformers 5 (Qwen2Tokenizer has no
// all_special_tokens_extended), so a fresh resolve breaks the engine at serve
// time. Newer releases get no extra pin.
func companionPins(vllmVersion string) []string {
	parts := strings.SplitN(vllmVersion, ".", 3)
	if len(parts) < 2 || parts[0] != "0" {
		return nil
	}
	minor, err := strconv.Atoi(leadingDigits(parts[1]))
	if err != nil || minor > 11 {
		return nil
	}
	return []string{"transformers<5"}
}

// leadingDigits returns the numeric prefix of a version component, so a
// pre-release such as "11rc1" reads as 11.
func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}

// transformersCompatible reports whether an installed transformers version
// satisfies companionPins for the given vLLM release.
func transformersCompatible(vllmVersion, transformersVersion string) bool {
	if len(companionPins(vllmVersion)) == 0 {
		return true
	}
	major, err := strconv.Atoi(strings.SplitN(transformersVersion, ".", 2)[0])
	return err == nil && major < 5
}

// installArgs is the package list for one resolver run.
func (v VenvSetup) installArgs() []string {
	return append([]string{"vllm==" + v.VLLMVersion}, companionPins(v.VLLMVersion)...)
}

// pipInstall returns the install command for pkgs with uv or pip.
func (v VenvSetup) pipInstall(uvPath string, pkgs []string) []string {
	py := VenvPython(v.Dir)
	if uvPath != "" {
		return append([]string{uvPath, "pip", "install", "--python", py}, pkgs...)
	}
	return append([]string{py, "-m", "pip", "install"}, pkgs...)
}

// CreatePlan returns the argv sequence that builds the venv: uv when present,
// else python -m venv plus pip.
func (v VenvSetup) CreatePlan(uvPath string) [][]string {
	py := VenvPython(v.Dir)
	if uvPath != "" {
		return [][]string{
			{uvPath, "venv", "--allow-existing", "--python", v.Python, v.Dir},
			v.pipInstall(uvPath, v.installArgs()),
		}
	}
	return [][]string{
		{v.Python, "-m", "venv", v.Dir},
		{py, "-m", "pip", "install", "--upgrade", "pip"},
		v.pipInstall("", v.installArgs()),
	}
}

// Env is the extra environment for every setup command. Caches live beside
// the venv (on the same scratch filesystem), not in a small home quota, and
// uv never downloads an interpreter: the pin covers vLLM and its known-bad
// companions only, so the Python it runs on is the cluster's own.
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
	if got, tf, err := v.installedVersion(ctx); err == nil {
		if got == v.VLLMVersion {
			if transformersCompatible(got, tf) {
				return got, nil
			}
			if v.CheckOnly {
				return "", fmt.Errorf("venv %s has transformers %s, which vLLM %s cannot load; run citadel hpc setup without --check to install %s", v.Dir, tf, got, strings.Join(companionPins(got), " "))
			}
			return v.repairCompanions(ctx)
		}
		if v.CheckOnly {
			return "", fmt.Errorf("venv %s has vLLM %s, want %s", v.Dir, got, v.VLLMVersion)
		}
		return "", fmt.Errorf("venv %s has vLLM %s, want %s; remove it or choose another --venv", v.Dir, got, v.VLLMVersion)
	}
	if v.CheckOnly {
		return "", fmt.Errorf("venv %s does not have a working vLLM %s with transformers", v.Dir, v.VLLMVersion)
	}
	if err := refuseForeignDir(v.Dir); err != nil {
		return "", err
	}
	for _, argv := range v.CreatePlan(v.uvPath()) {
		if out, err := v.Exec(ctx, argv); err != nil {
			return "", fmt.Errorf("%s: %w: %s", strings.Join(argv[:2], " "), err, tail(out))
		}
	}
	return v.verifyInstalled(ctx)
}

// repairCompanions installs the companion pins into a venv that already has
// the right vLLM, resolving them together with that vLLM, then verifies it.
func (v VenvSetup) repairCompanions(ctx context.Context) (string, error) {
	if v.Notify != nil {
		v.Notify(fmt.Sprintf("installing %s into %s", strings.Join(companionPins(v.VLLMVersion), " "), v.Dir))
	}
	argv := v.pipInstall(v.uvPath(), v.installArgs())
	if out, err := v.Exec(ctx, argv); err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(argv[:2], " "), err, tail(out))
	}
	return v.verifyInstalled(ctx)
}

func (v VenvSetup) uvPath() string {
	if v.LookPath != nil {
		if p, err := v.LookPath("uv"); err == nil {
			return p
		}
	}
	return ""
}

func (v VenvSetup) verifyInstalled(ctx context.Context) (string, error) {
	got, tf, err := v.installedVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("venv built but vLLM or transformers does not import: %w", err)
	}
	if got != v.VLLMVersion {
		return "", fmt.Errorf("venv built with vLLM %s, want %s", got, v.VLLMVersion)
	}
	if !transformersCompatible(got, tf) {
		return "", fmt.Errorf("venv built with transformers %s, which vLLM %s cannot load", tf, got)
	}
	return got, nil
}

// installedVersion returns the installed vLLM and transformers versions.
func (v VenvSetup) installedVersion(ctx context.Context) (string, string, error) {
	if _, err := os.Stat(VenvPython(v.Dir)); err != nil {
		return "", "", err
	}
	out, err := v.Exec(ctx, versionProbe(v.Dir))
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", err, tail(out))
	}
	vllm, tf, ok := parseProbe(out)
	if !ok {
		return "", "", fmt.Errorf("unexpected version probe output: %s", tail(out))
	}
	return vllm, tf, nil
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
