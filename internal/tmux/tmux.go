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
// Citadel creates sessions detached, marks only the session it just created,
// and then attaches with a separate command. A pre-existing unmarked session
// with the requested name is a collision, never something Citadel adopts.
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

// ErrSessionNameCollision means a requested name already belongs to an
// unmarked tmux session. Citadel fails closed rather than attaching to or
// adopting terminal state it did not create.
var ErrSessionNameCollision = errors.New("tmux session name is already owned outside Citadel")

// envTmuxBin is an optional override for the tmux binary path. When set it takes
// precedence over PATH lookup and the managed location.
const envTmuxBin = "CITADEL_TMUX_BIN"

// managedSessionOption is stored as a tmux user option on every session
// Citadel creates. The reaper checks this marker before acting, so a
// user's unrelated tmux sessions on the same server are never TTL-managed.
const managedSessionOption = "@citadel_managed"

const managedSessionOptionValue = "citadel-v1"

const managedSessionLeaseOption = "@citadel_lease_expires"

// DefaultSessionLeaseTTL is shared by the terminal service and TMUX_SESSION
// jobs so both creation paths receive the same bounded default lease.
const DefaultSessionLeaseTTL = 7 * 24 * time.Hour

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

// AttachArgs returns argv that attaches to an already-proven Citadel session.
// Creation and ownership verification happen in Manager.PrepareSession first;
// keeping attach separate prevents `new-session -A` from adopting an operator
// session that merely collides by name.
func AttachArgs(name string) []string {
	return []string{"attach-session", "-t", name}
}

// ListSessionsArgs returns the tmux argv that lists session names, one per line.
func ListSessionsArgs() []string {
	return []string{"list-sessions", "-F", "#{session_name}"}
}

// sessionStatusFormat is deliberately tab-delimited: validated Citadel names
// cannot contain tabs, and malformed/operator-created rows are skipped by the
// parser rather than broadening the reaper's authority.
const sessionStatusFormat = "#{session_name}\t#{session_id}\t#{pid}\t#{session_attached}\t#{@citadel_managed}\t#{@citadel_lease_expires}"

// SessionStatus is the subset of tmux metadata needed by the lease reaper.
type SessionStatus struct {
	Name         string
	ID           string
	ServerPID    int64
	Attached     bool
	Managed      bool
	LeaseExpires int64
}

// ListSessionStatusArgs returns tmux argv for one metadata row per session.
func ListSessionStatusArgs() []string {
	return []string{"list-sessions", "-F", sessionStatusFormat}
}

// DisplaySessionStatusArgs returns tmux argv for a fresh single-session
// snapshot.
func DisplaySessionStatusArgs(name string) []string {
	return []string{"display-message", "-p", "-t", name, sessionStatusFormat}
}

// NewManagedDetachedArgs creates, marks, and leases a session in one tmux
// command queue. tmux stops the queue when new-session fails (for example on a
// name collision), so the set-option commands can never adopt the colliding
// session. Keeping ownership establishment in the creator's queue also removes
// the create-return/mark-invocation window where a name could be replaced.
func NewManagedDetachedArgs(name, shell string, leaseExpires int64) []string {
	args := NewDetachedArgs(name, shell)
	return append(args,
		";",
		"set-option", "-q", "-t", name, managedSessionOption, managedSessionOptionValue,
		";", "set-option", "-q", "-t", name, managedSessionLeaseOption, strconv.FormatInt(leaseExpires, 10),
	)
}

// KillSessionArgs returns tmux argv for removing one validated session.
func KillSessionArgs(name string) []string {
	return []string{"kill-session", "-t", name}
}

// ReapExpiredLeaseArgs performs the final identity, ownership, attachment, and
// lease check inside one tmux command queue item. Session ID alone is not
// sufficient because a new tmux server starts numbering at $0 again; the
// server PID distinguishes that replacement incarnation.
func ReapExpiredLeaseArgs(status SessionStatus) []string {
	condition := fmt.Sprintf(
		"#{&&:%s,#{&&:#{==:#{session_attached},0},#{==:#{%s},%d}}}",
		ownedSessionCondition(status),
		managedSessionLeaseOption, status.LeaseExpires,
	)
	return []string{"if-shell", "-F", "-t", status.Name, condition, "kill-session -t " + status.ID, ""}
}

func ownedSessionCondition(status SessionStatus) string {
	return fmt.Sprintf(
		"#{&&:#{==:#{session_id},%s},#{&&:#{==:#{pid},%d},#{==:#{%s},%s}}}",
		status.ID, status.ServerPID, managedSessionOption, managedSessionOptionValue,
	)
}

// RenewOwnedSessionLeaseArgs binds renewal to both the immutable session ID
// and the tmux server PID. A killed/recreated name therefore fails the
// condition instead of receiving Citadel metadata.
func RenewOwnedSessionLeaseArgs(status SessionStatus, leaseExpires int64) []string {
	command := fmt.Sprintf("set-option -q -t %s %s %d", status.ID, managedSessionLeaseOption, leaseExpires)
	return []string{"if-shell", "-F", "-t", status.Name, ownedSessionCondition(status), command, ""}
}

// AttachOwnedSessionArgs renews and attaches only if name still resolves to
// the exact marked session inspected by Citadel. The whole check/action is one
// tmux command queue, closing the inspect-to-attach replacement window.
func AttachOwnedSessionArgs(status SessionStatus, leaseExpires int64) []string {
	command := fmt.Sprintf("set-option -q -t %s %s %d ; attach-session -t %s",
		status.ID, managedSessionLeaseOption, leaseExpires, status.ID)
	return []string{"if-shell", "-F", "-t", status.Name, ownedSessionCondition(status), command,
		"display-message -p 'Citadel tmux session ownership changed; refusing attach'"}
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

// EnsureSession creates a detached, leased Citadel session, or renews the
// default lease of an existing Citadel-owned session. An unmarked pre-existing
// name is rejected as ErrSessionNameCollision and is never modified.
func (m *Manager) EnsureSession(ctx context.Context, name, shell string) error {
	return m.EnsureSessionLease(ctx, name, shell, time.Now().Add(DefaultSessionLeaseTTL))
}

// EnsureSessionLease is EnsureSession with an explicit absolute lease deadline.
func (m *Manager) EnsureSessionLease(ctx context.Context, name, shell string, leaseUntil time.Time) error {
	_, err := m.ensureSessionLease(ctx, name, shell, leaseUntil)
	return err
}

func (m *Manager) ensureSessionLease(ctx context.Context, name, shell string, leaseUntil time.Time) (SessionStatus, error) {
	if err := ValidateSessionName(name); err != nil {
		return SessionStatus{}, err
	}
	status, exists, err := m.sessionStatus(ctx, name)
	if err != nil {
		return SessionStatus{}, err
	}
	if exists {
		if !status.Managed {
			return SessionStatus{}, fmt.Errorf("%w: %q", ErrSessionNameCollision, name)
		}
		return m.setSessionLease(ctx, status, leaseUntil)
	}

	// Ownership is established in the same tmux command queue as creation.
	// tmux aborts the remaining queue on a duplicate-session error, so a
	// pre-existing or racing operator session cannot receive our marker.
	command := append([]string{m.bin}, NewManagedDetachedArgs(name, shell, leaseUnix(leaseUntil))...)
	command = m.scopeCommand(name, command)
	if out, createErr := m.runner.Run(ctx, command[0], command[1:]...); createErr != nil {
		// Another creator may have won the absent->create race. It is safe to
		// continue only if that winner has already marked the session as
		// Citadel-owned; an unmarked winner remains an operator collision.
		status, nowExists, inspectErr := m.sessionStatus(ctx, name)
		if inspectErr == nil && nowExists {
			if !status.Managed {
				return SessionStatus{}, fmt.Errorf("%w: %q", ErrSessionNameCollision, name)
			}
			return m.setSessionLease(ctx, status, leaseUntil)
		}
		return SessionStatus{}, fmt.Errorf("tmux new-session failed: %w: %s", createErr, strings.TrimSpace(string(out)))
	}

	status, exists, err = m.sessionStatus(ctx, name)
	if err != nil {
		return SessionStatus{}, err
	}
	if !exists || !status.Managed || status.LeaseExpires != leaseUnix(leaseUntil) {
		return SessionStatus{}, fmt.Errorf("tmux created session %q but could not verify its ownership lease", name)
	}
	return status, nil
}

// PrepareSession ensures ownership/lease then returns the scoped attach command
// for a terminal PTY.
func (m *Manager) PrepareSession(ctx context.Context, name, shell string, leaseUntil time.Time) ([]string, error) {
	status, err := m.ensureSessionLease(ctx, name, shell, leaseUntil)
	if err != nil {
		return nil, err
	}
	command := append([]string{m.bin}, AttachOwnedSessionArgs(status, leaseUnix(leaseUntil))...)
	return m.scopeCommand(name, command), nil
}

// RenewSessionLease extends only an already-marked Citadel session.
func (m *Manager) RenewSessionLease(ctx context.Context, name string, leaseUntil time.Time) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}
	status, exists, err := m.sessionStatus(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !status.Managed {
		return fmt.Errorf("%w: %q", ErrSessionNameCollision, name)
	}
	_, err = m.setSessionLease(ctx, status, leaseUntil)
	return err
}

func (m *Manager) setSessionLease(ctx context.Context, status SessionStatus, leaseUntil time.Time) (SessionStatus, error) {
	lease := leaseUnix(leaseUntil)
	out, err := m.runner.Run(ctx, m.bin, RenewOwnedSessionLeaseArgs(status, lease)...)
	if err != nil {
		return SessionStatus{}, fmt.Errorf("tmux renew session lease failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fresh, exists, err := m.sessionStatus(ctx, status.Name)
	if err != nil {
		return SessionStatus{}, err
	}
	if !exists || !sameSessionIdentity(status, fresh) || !fresh.Managed || fresh.LeaseExpires != lease {
		return SessionStatus{}, fmt.Errorf("%w: %q changed while renewing its lease", ErrSessionNameCollision, status.Name)
	}
	return fresh, nil
}

func sameSessionIdentity(a, b SessionStatus) bool {
	return a.ID == b.ID && a.ServerPID == b.ServerPID
}

func leaseUnix(leaseUntil time.Time) int64 {
	if leaseUntil.IsZero() {
		return 0
	}
	return leaseUntil.Unix()
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

// ReapExpiredSessions removes detached Citadel sessions whose explicit
// retention lease has expired. It does not use session_activity: tmux does not
// update that field for detached pane output or send-keys, so it cannot safely
// identify an idle task.
func (m *Manager) ReapExpiredSessions(ctx context.Context, now time.Time) ([]string, error) {
	statuses, err := m.listSessionStatuses(ctx)
	if err != nil {
		return nil, err
	}

	var reaped []string
	for _, status := range statuses {
		if !reapEligible(status, now) {
			continue
		}
		if out, err := m.runner.Run(ctx, m.bin, ReapExpiredLeaseArgs(status)...); err != nil {
			return reaped, fmt.Errorf("tmux conditional reap %q failed: %w: %s", status.Name, err, strings.TrimSpace(string(out)))
		}
		exists, err := m.HasSession(ctx, status.Name)
		if err != nil {
			return reaped, err
		}
		if exists {
			continue
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

func reapEligible(status SessionStatus, now time.Time) bool {
	return status.Managed && !status.Attached && status.LeaseExpires > 0 && status.LeaseExpires <= now.Unix()
}

func parseSessionStatuses(out []byte) []SessionStatus {
	var statuses []SessionStatus
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 6 || ValidateSessionName(fields[0]) != nil || !validSessionID(fields[1]) {
			continue
		}
		serverPID, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || serverPID <= 0 {
			continue
		}
		attached, err := strconv.Atoi(fields[3])
		if err != nil || attached < 0 {
			continue
		}
		lease, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			lease = 0
		}
		statuses = append(statuses, SessionStatus{
			Name:         fields[0],
			ID:           fields[1],
			ServerPID:    serverPID,
			Attached:     attached > 0,
			Managed:      fields[4] == managedSessionOptionValue,
			LeaseExpires: lease,
		})
	}
	return statuses
}

func validSessionID(id string) bool {
	if len(id) < 2 || id[0] != '$' {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 10, 64)
	return err == nil
}
