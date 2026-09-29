// Package tmux provides built-in management of named tmux sessions on a Citadel
// node, used to back persistent terminal/console attachments for the "chat to a
// node" path (aceteam EPIC #4144, issue #302).
//
// The package never assumes tmux is installed. Resolve locates a usable tmux
// binary by checking, in order: an explicit override (CITADEL_TMUX_BIN), the
// system PATH, and a Citadel-managed location (see ManagedBinaryPath). If none
// is found it returns ErrTmuxNotFound with actionable guidance rather than
// crashing.
//
// Sessions are created with `tmux new-session -A -s <name>`, which attaches to
// an existing session of that name or creates a new detached one. Because the
// tmux server keeps sessions alive after a client detaches, a WebSocket client
// can disconnect and later re-attach to the same named session — the terminal
// state survives reconnects.
//
// Starting a session is intentionally decoupled from launching `claude`: a
// session is just a shell. Launching an agent inside it is a separate, explicit
// SendKeys step that the caller may perform once claude is installed.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrTmuxNotFound indicates no usable tmux binary could be located on the node.
var ErrTmuxNotFound = errors.New("tmux not found: no tmux binary on PATH and no Citadel-managed binary is installed")

// ErrInvalidSessionName indicates a session name failed validation.
var ErrInvalidSessionName = errors.New("invalid tmux session name")

// envTmuxBin is an optional override for the tmux binary path. When set it takes
// precedence over PATH lookup and the managed location.
const envTmuxBin = "CITADEL_TMUX_BIN"

// managedSessionOption is stored as a tmux user option on every session
// Citadel creates or adopts. The reaper checks this marker before acting, so a
// user's unrelated tmux sessions on the same server are never TTL-managed.
const managedSessionOption = "@citadel_managed"

const managedSessionOptionValue = "citadel-v1"

// logf receives package diagnostics. The CLI wires it to its durable log in
// PersistentPreRun; the no-op default keeps library users quiet.
var logf = func(string, ...any) {}

var logfSet bool

// SetLogf wires tmux lifecycle diagnostics into the caller's logging system.
// It must be called before concurrent tmux operations begin.
func SetLogf(fn func(string, ...any)) {
	if fn != nil {
		logf = fn
		logfSet = true
	}
}

// LogfConfigured reports whether the CLI installed a real diagnostic logger.
// It exists for the root wiring regression test: a forgotten SetLogf otherwise
// compiles while silently discarding every scope/fallback decision.
func LogfConfigured() bool { return logfSet }

// sessionNamePattern restricts session names to a safe character set. tmux
// session names may not contain '.' or ':' (used to address windows/panes), and
// we further forbid whitespace and shell metacharacters so a name can never be
// misinterpreted when constructing argv. Names are limited to a sane length.
var sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidateSessionName reports whether name is a safe, well-formed tmux session
// name. It returns ErrInvalidSessionName (wrapped with context) on failure.
//
// The name is the only user/caller-influenced value that flows into the tmux
// argv, so validation here is the trust boundary that keeps session control
// free of injection or accidental window/pane addressing.
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidSessionName)
	}
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q (allowed: letters, digits, '-', '_'; max 64 chars)", ErrInvalidSessionName, name)
	}
	return nil
}

// ManagedBinaryPath returns the path where a Citadel-managed tmux binary would
// live if bundled/installed by Citadel. The actual provisioning of this binary
// is tracked as follow-up work (see the package doc and issue #302); for now
// Resolve will use the binary if it happens to exist at this path, otherwise it
// falls through to ErrTmuxNotFound.
func ManagedBinaryPath() string {
	name := "tmux"
	if runtime.GOOS == "windows" {
		name = "tmux.exe"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fall back to a relative path; callers that need an absolute path can
		// still test for existence and will simply miss the managed binary.
		return filepath.Join(".citadel", "bin", name)
	}
	return filepath.Join(home, ".citadel", "bin", name)
}

// Resolve locates a usable tmux binary without assuming one is installed.
//
// Resolution order:
//  1. CITADEL_TMUX_BIN, if set and the file exists.
//  2. tmux on the system PATH.
//  3. The Citadel-managed binary at ManagedBinaryPath, if present.
//
// On success it returns the absolute path (or resolvable command) to invoke.
// On failure it returns ErrTmuxNotFound.
func Resolve() (string, error) {
	if override := os.Getenv(envTmuxBin); override != "" {
		if fileExists(override) {
			return override, nil
		}
		return "", fmt.Errorf("%w: %s=%q does not point to an existing file", ErrTmuxNotFound, envTmuxBin, override)
	}

	if path, err := exec.LookPath("tmux"); err == nil {
		return path, nil
	}

	if managed := ManagedBinaryPath(); fileExists(managed) {
		return managed, nil
	}

	return "", ErrTmuxNotFound
}

// IsAvailable reports whether a usable tmux binary can be resolved on this node.
func IsAvailable() bool {
	_, err := Resolve()
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Runner executes a tmux command and returns its combined output. It is the
// single seam through which the package touches the OS, so tests can inject a
// fake without a real tmux installation.
type Runner interface {
	Run(ctx context.Context, bin string, args ...string) (output []byte, err error)
}

// execRunner is the production Runner backed by os/exec.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	return cmd.CombinedOutput()
}

// DefaultRunner returns the production Runner backed by os/exec.
func DefaultRunner() Runner { return execRunner{} }

// Manager manages named tmux sessions through a resolved tmux binary and a
// Runner. Construct it with NewManager.
type Manager struct {
	bin          string
	runner       Runner
	scopeCommand func(sessionName string, command []string) []string
}

// NewManager resolves tmux and returns a Manager bound to it. It returns
// ErrTmuxNotFound if no usable tmux binary is available.
func NewManager() (*Manager, error) {
	bin, err := Resolve()
	if err != nil {
		return nil, err
	}
	return &Manager{bin: bin, runner: DefaultRunner(), scopeCommand: PersistentSessionCommand}, nil
}

// NewManagerWith constructs a Manager from an explicit binary path and Runner.
// It is intended for tests; production code should use NewManager.
func NewManagerWith(bin string, runner Runner) *Manager {
	// Tests and embedders that inject a Runner get the exact command they asked
	// for. The production constructor is what opts tmux creation into a detached
	// service-manager scope.
	return &Manager{bin: bin, runner: runner, scopeCommand: identitySessionCommand}
}

// Binary returns the resolved tmux binary path the Manager invokes.
func (m *Manager) Binary() string { return m.bin }

// HasSessionArgs returns the tmux argv that tests whether a named session
// exists. tmux exits non-zero when the session is absent.
func HasSessionArgs(name string) []string {
	return []string{"has-session", "-t", name}
}

// NewDetachedArgs returns the tmux argv that creates a detached session running
// the given shell. An empty shell lets tmux use its configured default. This is
// the idempotent create primitive used by EnsureSession.
func NewDetachedArgs(name, shell string) []string {
	args := []string{"new-session", "-d", "-s", name}
	if shell != "" {
		args = append(args, shell)
	}
	return args
}

// AttachOrCreateArgs returns the tmux argv that attaches to a named session,
// creating it (running shell) if it does not already exist. This is what the
// terminal PTY runs so the same name survives reconnects: `-A` makes
// new-session attach-if-exists, giving create/attach idempotency in one call.
//
// Launching claude is deliberately NOT part of this command; the session is a
// plain shell until something explicitly sends keys to start an agent.
func AttachOrCreateArgs(name, shell string) []string {
	// Keep the established attach-or-create behavior and append the marker in
	// the same tmux command list. This avoids a detach/reattach transition while
	// still adopting older Citadel sessions on their first post-upgrade attach.
	args := []string{"new-session", "-A", "-s", name}
	if shell != "" {
		args = append(args, shell)
	}
	args = append(args,
		";", "set-option", "-q", "-t", name, managedSessionOption, managedSessionOptionValue,
	)
	return args
}

// ListSessionsArgs returns the tmux argv that lists session names, one per line.
func ListSessionsArgs() []string {
	return []string{"list-sessions", "-F", "#{session_name}"}
}

// sessionStatusFormat is deliberately tab-delimited: validated Citadel names
// cannot contain tabs, and malformed/operator-created rows are skipped by the
// parser rather than broadening the reaper's authority.
const sessionStatusFormat = "#{session_name}\t#{session_activity}\t#{session_attached}\t#{@citadel_managed}"

// SessionStatus is the subset of tmux metadata needed by the idle-session
// reaper.
type SessionStatus struct {
	Name         string
	LastActivity time.Time
	Attached     bool
	Managed      bool
}

// ListSessionStatusArgs returns tmux argv for one metadata row per session.
func ListSessionStatusArgs() []string {
	return []string{"list-sessions", "-F", sessionStatusFormat}
}

// DisplaySessionStatusArgs returns tmux argv for a fresh single-session
// snapshot. ReapIdleSessions uses it immediately before a kill so a session
// that became attached or active after the initial list is spared.
func DisplaySessionStatusArgs(name string) []string {
	return []string{"display-message", "-p", "-t", name, sessionStatusFormat}
}

// MarkSessionArgs marks a session as Citadel-managed using a tmux user option.
func MarkSessionArgs(name string) []string {
	return []string{"set-option", "-q", "-t", name, managedSessionOption, managedSessionOptionValue}
}

// KillSessionArgs returns tmux argv for removing one validated session.
func KillSessionArgs(name string) []string {
	return []string{"kill-session", "-t", name}
}

// HasSession reports whether a session with the given (validated) name exists.
func (m *Manager) HasSession(ctx context.Context, name string) (bool, error) {
	if err := ValidateSessionName(name); err != nil {
		return false, err
	}
	_, err := m.runner.Run(ctx, m.bin, HasSessionArgs(name)...)
	if err == nil {
		return true, nil
	}
	// tmux exits non-zero when the session does not exist; treat a clean
	// non-zero exit as "absent" rather than a hard error.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session failed: %w", err)
}

// EnsureSession creates a detached named session if it does not already exist.
// It is idempotent: calling it for an existing session is a no-op. The session
// runs the given shell (empty uses tmux's default); claude is never launched
// here.
func (m *Manager) EnsureSession(ctx context.Context, name, shell string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}
	exists, err := m.HasSession(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		command := append([]string{m.bin}, NewDetachedArgs(name, shell)...)
		command = m.scopeCommand(name, command)
		if out, err := m.runner.Run(ctx, command[0], command[1:]...); err != nil {
			return fmt.Errorf("tmux new-session failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// Mark both newly-created and already-existing sessions. The latter adopts
	// sessions created by the immediately preceding release once they are next
	// ensured, without ever sweeping arbitrary unmarked tmux state.
	if out, err := m.runner.Run(ctx, m.bin, MarkSessionArgs(name)...); err != nil {
		return fmt.Errorf("tmux mark session failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ListSessions returns the names of all sessions on the node's tmux server.
// When no tmux server is running tmux exits non-zero with "no server running";
// that is reported as an empty list rather than an error.
func (m *Manager) ListSessions(ctx context.Context) ([]string, error) {
	out, err := m.runner.Run(ctx, m.bin, ListSessionsArgs()...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// No server / no sessions: tmux prints to stderr and exits non-zero.
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-sessions failed: %w", err)
	}
	return parseSessionList(out), nil
}

// parseSessionList splits tmux list-sessions output (one name per line) into a
// slice, dropping blank lines.
func parseSessionList(out []byte) []string {
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

// ReapIdleSessions kills Citadel-managed sessions that are detached and whose
// tmux activity timestamp is not newer than cutoff. It never acts on an
// unmarked session. Each candidate is re-read immediately before the kill to
// avoid using a stale attachment/activity snapshot.
func (m *Manager) ReapIdleSessions(ctx context.Context, cutoff time.Time) ([]string, error) {
	statuses, err := m.listSessionStatuses(ctx)
	if err != nil {
		return nil, err
	}

	var reaped []string
	for _, status := range statuses {
		if !reapEligible(status, cutoff) {
			continue
		}
		fresh, ok, err := m.sessionStatus(ctx, status.Name)
		if err != nil {
			return reaped, err
		}
		if !ok || !reapEligible(fresh, cutoff) {
			continue
		}
		if out, err := m.runner.Run(ctx, m.bin, KillSessionArgs(status.Name)...); err != nil {
			return reaped, fmt.Errorf("tmux kill-session %q failed: %w: %s", status.Name, err, strings.TrimSpace(string(out)))
		}
		reaped = append(reaped, status.Name)
	}
	return reaped, nil
}

func (m *Manager) listSessionStatuses(ctx context.Context) ([]SessionStatus, error) {
	out, err := m.runner.Run(ctx, m.bin, ListSessionStatusArgs()...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-sessions for reaper failed: %w", err)
	}
	return parseSessionStatuses(out), nil
}

func (m *Manager) sessionStatus(ctx context.Context, name string) (SessionStatus, bool, error) {
	if err := ValidateSessionName(name); err != nil {
		return SessionStatus{}, false, err
	}
	out, err := m.runner.Run(ctx, m.bin, DisplaySessionStatusArgs(name)...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return SessionStatus{}, false, nil
		}
		return SessionStatus{}, false, fmt.Errorf("tmux inspect session %q failed: %w", name, err)
	}
	statuses := parseSessionStatuses(out)
	if len(statuses) != 1 || statuses[0].Name != name {
		return SessionStatus{}, false, nil
	}
	return statuses[0], true, nil
}

func reapEligible(status SessionStatus, cutoff time.Time) bool {
	return status.Managed && !status.Attached && !status.LastActivity.IsZero() && !status.LastActivity.After(cutoff)
}

func parseSessionStatuses(out []byte) []SessionStatus {
	var statuses []SessionStatus
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || ValidateSessionName(fields[0]) != nil {
			continue
		}
		activity, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || activity <= 0 {
			continue
		}
		attached, err := strconv.Atoi(fields[2])
		if err != nil || attached < 0 {
			continue
		}
		statuses = append(statuses, SessionStatus{
			Name:         fields[0],
			LastActivity: time.Unix(activity, 0),
			Attached:     attached > 0,
			Managed:      fields[3] == managedSessionOptionValue,
		})
	}
	return statuses
}
