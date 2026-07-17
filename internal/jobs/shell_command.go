// internal/jobs/shell_command.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// ShellDisabledError is the exact message returned when SHELL_COMMAND execution
// has been disabled on this node via the persisted `shell: false` permission
// (the `--no-shell`-style opt-out). The wording is part of the handler's
// contract and is asserted in tests, so keep it stable.
const ShellDisabledError = "shell command execution is disabled on this node"

// standardPATHDirs are directories ensured on PATH when the inherited process
// environment is restricted (e.g. citadel running via systemd or nohup with a
// minimal PATH). They are merged into the command environment so /bin/sh can
// resolve common executables.
var standardPATHDirs = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
}

// envAllowExact is the allowlist of environment variable names that are safe to
// forward from citadel's own process environment into a SHELL_COMMAND child.
// Anything not on this list (or the envAllowPrefixes below) is dropped, so
// inherited secrets never leak into dispatched shell jobs.
var envAllowExact = map[string]struct{}{
	"PATH":  {},
	"HOME":  {},
	"LANG":  {},
	"TERM":  {},
	"TZ":    {},
	"USER":  {},
	"SHELL": {},
}

// envAllowPrefixes are name prefixes that are also forwarded (locale settings
// such as LC_ALL, LC_CTYPE, ...). A denylist match still wins over these.
var envAllowPrefixes = []string{
	"LC_",
}

// envDenySubstrings deny any variable whose name contains one of these tokens,
// even if it would otherwise be allowed. This constrains the LC_* prefix (e.g.
// a contrived LC_SECRET_KEY) and guards against a careless future allowlist
// addition. Matching is case-insensitive.
var envDenySubstrings = []string{
	"TOKEN",
	"SECRET",
	"KEY",
	"PASSWORD",
}

// envDenyPrefixes deny any variable whose name starts with one of these
// prefixes. Covers cloud/CI credential families and citadel's own config/token
// vars. Matching is case-insensitive.
var envDenyPrefixes = []string{
	"AWS_",
	"DOCKER_",
	"GITHUB_",
	"CITADEL_",
}

// envDenyExact deny these specific variable names outright.
var envDenyExact = map[string]struct{}{
	"SSH_AUTH_SOCK": {},
}

// isDeniedEnvName reports whether an environment variable name matches any
// denylist rule. Deny always wins over the allowlist.
func isDeniedEnvName(name string) bool {
	upper := strings.ToUpper(name)
	if _, ok := envDenyExact[upper]; ok {
		return true
	}
	for _, p := range envDenyPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, s := range envDenySubstrings {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
}

// isAllowedInheritedEnvName reports whether an inherited variable name is on the
// allowlist. The denylist is applied separately (and wins).
func isAllowedInheritedEnvName(name string) bool {
	if _, ok := envAllowExact[name]; ok {
		return true
	}
	for _, p := range envAllowPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// scrubEnv builds the environment for a SHELL_COMMAND child from a base
// environment (typically os.Environ()) plus any job-provided vars.
//
// Inherited vars are kept only when they match the allowlist AND do not match
// the denylist (deny wins). Job-provided vars are trusted and bypass the lists —
// the dispatcher is authenticated, and the threat model here is inherited
// ambient secrets, not vars the caller deliberately set. PATH is always
// augmented with standardPATHDirs so /bin/sh can resolve common executables even
// under a restricted inherited PATH (#154).
func scrubEnv(base []string, jobEnv map[string]string) []string {
	kept := make(map[string]string)
	var order []string
	add := func(name, value string) {
		if _, seen := kept[name]; !seen {
			order = append(order, name)
		}
		kept[name] = value
	}

	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name := kv[:eq]
		if !isAllowedInheritedEnvName(name) || isDeniedEnvName(name) {
			continue
		}
		add(name, kv[eq+1:])
	}

	// Job-provided vars are explicit and override inherited values.
	for name, value := range jobEnv {
		if name == "" {
			continue
		}
		add(name, value)
	}

	// Ensure PATH includes the standard directories (restricted-env fallback).
	sep := string(os.PathListSeparator)
	if current, ok := kept["PATH"]; ok {
		existing := make(map[string]struct{})
		for _, d := range filepath.SplitList(current) {
			existing[d] = struct{}{}
		}
		var additions []string
		for _, d := range standardPATHDirs {
			if _, ok := existing[d]; !ok {
				additions = append(additions, d)
			}
		}
		if len(additions) > 0 {
			kept["PATH"] = current + sep + strings.Join(additions, sep)
		}
	} else {
		add("PATH", strings.Join(standardPATHDirs, sep))
	}

	env := make([]string, 0, len(order))
	for _, name := range order {
		env = append(env, name+"="+kept[name])
	}
	return env
}

// parseJobEnv extracts explicit environment overrides from the job payload's
// optional "env" field (a JSON object of string->string). Absent or malformed
// values yield no overrides rather than an error, so a bad env map never blocks
// an otherwise-valid command.
func parseJobEnv(job *nexus.Job) map[string]string {
	raw, ok := job.Payload["env"]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// ShellCommandHandler executes a command string through /bin/sh -c, so pipes,
// redirects, && / ||, quoting, and command substitution behave as expected.
type ShellCommandHandler struct {
	// WorkspaceDir, when non-empty, is set as the command's working directory so
	// relative paths resolve consistently with the file-operation handlers.
	WorkspaceDir string
	// Disabled, when true, makes Execute refuse every command with
	// ShellDisabledError instead of running it. Wired from the persisted
	// `shell` node permission (default: enabled).
	Disabled bool
}

// NewShellCommandHandler constructs a handler bound to a workspace directory.
// A zero-value &ShellCommandHandler{} remains valid (no working directory set).
func NewShellCommandHandler(workspace string) *ShellCommandHandler {
	return &ShellCommandHandler{WorkspaceDir: workspace}
}

func (h *ShellCommandHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if h.Disabled {
		ctx.Log("warn", "     - [Job %s] Refusing shell command: %s", job.ID, ShellDisabledError)
		return nil, fmt.Errorf("%s", ShellDisabledError)
	}

	cmdString, ok := job.Payload["command"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'command' field")
	}
	if strings.TrimSpace(cmdString) == "" {
		return nil, fmt.Errorf("empty command")
	}
	ctx.Log("info", "     - [Job %s] Running shell command: '%s'", job.ID, cmdString)

	// Bind the command to the job context so a per-job deadline / cancellation
	// terminates the child process instead of leaking it (aceteam#6000).
	// CommandContext sends Kill when ctx is done; WaitDelay bounds how long
	// CombinedOutput waits on inherited stdio pipes afterwards, so a backgrounded
	// grandchild holding the pipe can't keep the handler blocked indefinitely.
	cmd := exec.CommandContext(ctx.Context(), "/bin/sh", "-c", cmdString)
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = scrubEnv(os.Environ(), parseJobEnv(job))
	if h.WorkspaceDir != "" {
		cmd.Dir = h.WorkspaceDir
	}
	return cmd.CombinedOutput()
}
