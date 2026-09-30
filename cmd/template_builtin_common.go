package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

type renderCommand struct {
	cmd       *exec.Cmd
	stopScope func() error
}

func (c *renderCommand) stopSystemdScope() error {
	if c == nil || c.stopScope == nil {
		return nil
	}
	return c.stopScope()
}

// minimalRenderChildEnv is the complete environment visible to Chromium and
// ffmpeg. The worker may carry long-lived device, Redis, model-provider, or
// source credentials; none are inherited by render children. The locale and
// identity values are benign compatibility inputs, while Chromium receives an
// attempt-private HOME/XDG/tmp tree.
func minimalRenderChildEnv(home string) []string {
	tempDir := home
	if home == "" {
		home = "/nonexistent"
		tempDir = os.TempDir()
	}
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"HOME=" + home,
		"PATH=" + pathEnv,
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"TMPDIR=" + tempDir,
		"TMP=" + tempDir,
		"TEMP=" + tempDir,
	}
	for _, key := range []string{"USER", "LOGNAME", "LANG", "LC_ALL", "TZ"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// minimalRenderControlEnv is used by systemd-run/systemctl themselves. User
// managers may require XDG_RUNTIME_DIR or DBUS_SESSION_BUS_ADDRESS, but those
// control-plane values are not forwarded through the env -i child boundary.
func minimalRenderControlEnv() []string {
	env := minimalRenderChildEnv("")
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// decodeBuiltinParams gives compiled-in runners an exact-key decoder. The
// approved schema gate is the first line of defence; this second line makes a
// builtin safe even if a future schema feature accidentally accepts an
// unconstrained key. It deliberately does not use encoding/json struct binding,
// whose case-insensitive field matching motivated the #1161 gate.
func decodeBuiltinParams(raw json.RawMessage, allowed ...string) (map[string]any, error) {
	v, err := jobs.DecodeTemplateJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("params must be an object")
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range m {
		if _, ok := allow[key]; !ok {
			return nil, fmt.Errorf("unknown or noncanonical param key %q", key)
		}
	}
	return m, nil
}

// removeStaleBuiltinOutput prevents an earlier failed or retried run from
// satisfying the post-command non-empty-file check. It also removes a stale
// symlink before a fixed output path is handed to an external encoder.
func removeStaleBuiltinOutput(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("remove stale output %q: %w", path, err)
}
