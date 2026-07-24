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

// Shell refusal reason codes. A SHELL_COMMAND refusal returns a *ShellRefusal
// whose Error() is a JSON object {"reason":"<code>","message":"<human>"} so the
// platform frontend (aceteam#6559/#6597/#6598) can prompt the operator precisely
// (enable shell, set a passcode, or fix the presented passcode) instead of
// surfacing a raw error string. The codes are part of the handler's contract and
// are asserted in tests, so keep them stable.
const (
	// ReasonShellDisabled means the shell surface is turned off entirely
	// (h.Disabled==true). Shell is default-deny (opt-in) as of aceteam #6149
	// Phase 0: a node accepts remote commands only after an operator explicitly
	// enables the `shell` permission.
	ReasonShellDisabled = "shell_disabled"
	// ReasonPasscodeNotSet means shell is ENABLED but no node passcode is
	// configured, so there is nothing to check the presented passcode against.
	// Fails CLOSED: the operator must set a passcode before shell can run.
	ReasonPasscodeNotSet = "passcode_not_set"
	// ReasonPasscodeInvalid means a passcode IS configured but the presented
	// payload passcode is absent or wrong.
	ReasonPasscodeInvalid = "passcode_invalid"
)

// Human-readable messages carried in a ShellRefusal.Message. Kept as constants
// so the handler and its tests agree on the wording without duplicating the
// literals.
const (
	msgShellDisabled   = "shell command execution is not enabled on this node; enable the `shell` permission (set `shell: true` in permissions.yaml or toggle Shell in the AceTeam control center, then restart the worker)"
	msgPasscodeNotSet  = "shell command execution requires a node passcode, but none is set; set a node passcode (APPLY_DEVICE_CONFIG `nodePasscode` or the AceTeam control center) before dispatching shell commands"
	msgPasscodeInvalid = "shell command execution requires the node passcode; present the correct passcode in the SHELL_COMMAND payload `passcode` field"
)

// ShellRefusal is the typed error returned when a SHELL_COMMAND is refused. Its
// Error() renders a machine-readable JSON object
// {"reason":"<code>","message":"<human>"} (marshaled via encoding/json, never
// hand-concatenated, so a message containing quotes stays valid JSON). Reason is
// one of the Reason* codes above. Callers should inspect the typed error via
// errors.As and switch on Reason rather than string-matching the message.
type ShellRefusal struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Error renders the refusal as a JSON object. Reason and Message are plain
// strings so json.Marshal cannot fail here; the concatenated fallback exists
// only to satisfy the error contract defensively and is never expected to run.
func (e *ShellRefusal) Error() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"reason":"` + e.Reason + `","message":"shell command refused"}`
	}
	return string(b)
}

// ShellPasscodePayloadKey is the SHELL_COMMAND payload field carrying the
// per-node passcode. The platform (aceteam#6559) must set this on every shell
// dispatch once the node has Shell enabled, or the command is refused.
const ShellPasscodePayloadKey = "passcode"

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
	// Disabled, when true, makes Execute refuse every command with a
	// ReasonShellDisabled refusal instead of running it. Wired from the persisted
	// `shell` node permission, which is default-deny (opt-in): unless a node
	// explicitly enables Shell, callers set Disabled=true (aceteam #6149).
	Disabled bool
	// HasPasscode reports whether a node passcode is configured at all, WITHOUT
	// leaking the hash. It lets Execute distinguish "no passcode set"
	// (ReasonPasscodeNotSet: the operator must set one) from "wrong passcode
	// presented" (ReasonPasscodeInvalid), which VerifyPasscode alone cannot do
	// (both cases make VerifyPasscode return false). Fail-CLOSED contract: a nil
	// HasPasscode, or HasPasscode()==false, is treated as "no passcode set" and
	// refuses every command. The construction sites wire it from
	// config.LoadPermissions(...).HasPasscode.
	HasPasscode func() bool
	// VerifyPasscode gates an ENABLED shell handler on the per-node passcode
	// (aceteam#6524), mirroring how the gateway gates console/desktop/files: it
	// checks the passcode presented in the SHELL_COMMAND payload (see
	// ShellPasscodePayloadKey) against the node's bcrypt passcode. Fail-CLOSED
	// contract: whenever the handler is enabled (Disabled==false) this MUST be
	// wired; a nil verifier refuses every command, so a forgotten gate never
	// silently opens root shell to anyone who can dispatch a job. The construction
	// sites (worker.CreateLegacyHandlersWithOpts and the legacy cmd/job_handlers
	// map) wire it from config.LoadPermissions(...).VerifyPasscode.
	VerifyPasscode func(pin string) bool
}

// NewShellCommandHandler constructs a handler bound to a workspace directory.
// A zero-value &ShellCommandHandler{} remains valid (no working directory set).
func NewShellCommandHandler(workspace string) *ShellCommandHandler {
	return &ShellCommandHandler{WorkspaceDir: workspace}
}

func (h *ShellCommandHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	// Refusal decision order (aceteam#6524, #6559): shell_disabled beats
	// passcode_not_set beats passcode_invalid. Each returns a typed *ShellRefusal
	// carrying a machine-readable reason code so the platform can prompt the
	// operator precisely. Fail CLOSED throughout: a nil HasPasscode or a nil
	// VerifyPasscode still refuses, so a forgotten gate never silently opens root
	// shell to anyone who can dispatch a job.
	if h.Disabled {
		refusal := &ShellRefusal{Reason: ReasonShellDisabled, Message: msgShellDisabled}
		ctx.Log("warn", "     - [Job %s] Refusing shell command: %s", job.ID, refusal.Message)
		return nil, refusal
	}

	// No passcode configured: nothing to check against, so the operator must set
	// one before shell can run. A nil HasPasscode is treated as "not set".
	if h.HasPasscode == nil || !h.HasPasscode() {
		refusal := &ShellRefusal{Reason: ReasonPasscodeNotSet, Message: msgPasscodeNotSet}
		ctx.Log("warn", "     - [Job %s] Refusing shell command: %s", job.ID, refusal.Message)
		return nil, refusal
	}

	// A passcode IS configured: the job must present the correct one. The passcode
	// is read from a dedicated payload field and is never forwarded into the
	// command environment. A nil verifier (gate not wired) or a wrong/absent
	// payload passcode refuses before any command runs.
	if h.VerifyPasscode == nil || !h.VerifyPasscode(job.Payload[ShellPasscodePayloadKey]) {
		refusal := &ShellRefusal{Reason: ReasonPasscodeInvalid, Message: msgPasscodeInvalid}
		ctx.Log("warn", "     - [Job %s] Refusing shell command: %s", job.ID, refusal.Message)
		return nil, refusal
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
