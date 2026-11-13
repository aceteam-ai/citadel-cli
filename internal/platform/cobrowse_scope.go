// internal/platform/cobrowse_scope.go
//
// Detaching a co-browse session from the worker's service control group
// (citadel-cli#1546, item 2). The Linux runner starts each session's Xvfb and
// browser in their own uniquely named transient systemd scope, so a full service
// restart (control-group kill) leaves them running and the next worker can
// re-adopt them. This mirrors tmux.PersistentSessionCommand (#1166): a user
// manager for a user unit, the system manager only for a root system unit, and a
// fail-open direct launch everywhere else.
package platform

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	scopeInvocationEnv = "INVOCATION_ID"
	// EndedReasonServiceRestart is stamped on sessions left running when the
	// managed worker stops, so status can say why one did not come back.
	EndedReasonServiceRestart = "service_restart"
)

// Seams for tests; production values talk to the real system.
var (
	scopeSupported   = runtime.GOOS == "linux"
	scopeGetenv      = os.Getenv
	scopeLookupRun   = exec.LookPath
	scopeReadCgroup  = func() ([]byte, error) { return os.ReadFile("/proc/self/cgroup") }
	scopeEUID        = os.Geteuid
	scopeProcCgroup  = defaultProcCgroup
	scopeStopper     = defaultScopeStop
	scopeSeq         atomic.Uint64
	scopeFailOpenLog sync.Once
	scopeNow         = time.Now
	// scopeBroken is set by the first failed scoped launch in this process; from
	// then on launches go direct, so a broken systemd-run costs one retry, not one
	// per session.
	scopeBroken atomic.Bool
)

// DetachGraceEnv bounds how long a scoped session may outlive its owner (for
// example after a permanent systemctl stop) before it is refused and swept.
const (
	DetachGraceEnv     = "CITADEL_COBROWSE_DETACH_GRACE_SECONDS"
	defaultDetachGrace = 900 * time.Second
)

// detachGrace resolves the owner-gone window; invalid or non-positive values
// fall back to the default.
func detachGrace() time.Duration {
	if v := scopeGetenv(DetachGraceEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultDetachGrace
}

// withinDetachGrace reports whether a record's owner died recently enough for
// its sessions to be kept. The record's last write is the owner-death evidence;
// an unreadable stamp fails closed.
func withinDetachGrace(rec sessionRecord) bool {
	t, err := time.Parse(time.RFC3339, rec.UpdatedAt)
	if err != nil {
		return false
	}
	return scopeNow().Sub(t) <= detachGrace()
}

// validScopeName accepts only the names this package generates for session id.
func validScopeName(id, role, unit string) bool {
	if !sessionIDPattern.MatchString(id) {
		return false
	}
	return regexp.MustCompile(`^citadel-cobrowse-` + regexp.QuoteMeta(id) + `-` + role + `-[0-9]+-[0-9]+\.scope$`).MatchString(unit)
}

var anyScopeName = regexp.MustCompile(`^citadel-cobrowse-cb-[0-9a-f]+-(browser|xvfb)-[0-9]+-[0-9]+\.scope$`)

func defaultProcCgroup(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	return string(b), err
}

// scopePlan is a decision to wrap launches in transient scopes.
type scopePlan struct {
	systemdRun string
	user       bool
}

// planCobrowseScope decides whether a launch can be scoped. It returns a nil
// plan and a reason when it cannot; the launch then stays a direct child
// (today's behavior: the session dies with a service restart).
func planCobrowseScope() (*scopePlan, string) {
	if scopeBroken.Load() {
		return nil, "an earlier scoped launch failed in this process"
	}
	if !scopeSupported {
		return nil, "unsupported platform"
	}
	if scopeGetenv(scopeInvocationEnv) == "" {
		return nil, "process is not running in a systemd service (INVOCATION_ID is unset)"
	}
	run, err := scopeLookupRun("systemd-run")
	if err != nil {
		return nil, "systemd-run is unavailable"
	}
	cg, err := scopeReadCgroup()
	if err != nil {
		return nil, fmt.Sprintf("cannot read /proc/self/cgroup: %v", err)
	}
	user := cgroupIsUserManager(string(cg))
	if !user && scopeEUID() != 0 {
		return nil, "non-root process in a system service cannot create a sibling system scope"
	}
	return &scopePlan{systemdRun: run, user: user}, ""
}

func cgroupIsUserManager(cgroup string) bool {
	for _, line := range strings.Split(cgroup, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, c := range strings.Split(parts[2], "/") {
			if strings.HasPrefix(c, "user@") && strings.HasSuffix(c, ".service") {
				return true
			}
		}
	}
	return false
}

// unit returns a fresh scope name for one process of a session. The PID and a
// process-local sequence keep names unique across restarts and relaunches.
func (p *scopePlan) unit(sessionID, role string) string {
	return fmt.Sprintf("citadel-cobrowse-%s-%s-%d-%d.scope", sessionID, role, os.Getpid(), scopeSeq.Add(1))
}

// wrap returns the argv that runs command inside the named scope. systemd-run
// --scope execs the command in place, so the command keeps the PID of the
// process we start.
func (p *scopePlan) wrap(unit string, command []string) []string {
	args := []string{p.systemdRun}
	if p.user {
		args = append(args, "--user")
	}
	args = append(args, "--scope", "--quiet", "--collect", "--unit="+unit, "--")
	return append(args, command...)
}

// logScopeFailOpen logs (once per process) why sessions are not detached.
func logScopeFailOpen(reason string) {
	scopeFailOpenLog.Do(func() {
		log.Printf("[cobrowse] sessions are not detached from the service (a service restart will end them): %s", reason)
	})
}

// scopeStopArgs ends options before the unit so a name can never be a flag.
func scopeStopArgs(unit string) []string { return []string{"stop", "--", unit} }

func defaultScopeStop(user bool, unit string) error {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return err
	}
	args := []string{}
	if user {
		args = append(args, "--user")
	}
	args = append(args, scopeStopArgs(unit)...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, args...).Run()
}

// inScope reports whether pid's cgroup is the named scope.
func inScope(pid int, unit string) bool {
	if unit == "" || pid <= 0 {
		return false
	}
	cg, err := scopeProcCgroup(pid)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(cg, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		comps := strings.Split(strings.TrimSpace(parts[2]), "/")
		if comps[len(comps)-1] == unit { // the unit must be the leaf, not an ancestor
			return true
		}
	}
	return false
}

// procLeafCgroup returns the parent path and leaf component of pid's cgroup.
func procLeafCgroup(pid int) (parent, leaf string, ok bool) {
	if pid <= 0 {
		return "", "", false
	}
	cg, err := scopeProcCgroup(pid)
	if err != nil {
		return "", "", false
	}
	for _, line := range strings.Split(cg, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		p := strings.TrimSpace(parts[2])
		i := strings.LastIndex(p, "/")
		if i < 0 || i == len(p)-1 {
			continue
		}
		return p[:i], p[i+1:], true
	}
	return "", "", false
}

// browserAppScope returns the scope a browser moved itself into (Chrome creates
// app-com.google.Chrome-<pid>.scope, Chromium app-org.chromium.Chromium-<pid>.scope
// about a second after launch). It is returned only when the name carries
// exactly this pid and the cgroup sits under the same systemd manager that owns
// the recorded scopes: the user manager's app.slice for a user scope, the system
// manager's app.slice otherwise. Anything else yields "".
func browserAppScope(user bool, uid, pid int) string {
	parent, leaf, ok := procLeafCgroup(pid)
	if !ok || !regexp.MustCompile(`^app-[A-Za-z0-9][A-Za-z0-9._-]*-`+strconv.Itoa(pid)+`\.scope$`).MatchString(leaf) {
		return ""
	}
	want := "/app.slice"
	if user {
		want = fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice", uid, uid)
	}
	if parent != want {
		return ""
	}
	return leaf
}

// browserInOwnScope reports whether the browser pid lives in its recorded
// citadel scope or in an app scope it created for itself under the same manager.
func browserInOwnScope(user bool, uid, pid int, recorded string) bool {
	return inScope(pid, recorded) || browserAppScope(user, uid, pid) != ""
}

// stopBrowserAppScope stops the browser's self-created app scope, only while
// the recorded process identity still matches and the name passes the checks.
func stopBrowserAppScope(user bool, uid, pid int, start uint64) {
	if pid <= 0 || start == 0 {
		return
	}
	unit := browserAppScope(user, uid, pid)
	if unit == "" {
		return
	}
	if st, alive, err := procIdentity(pid); err != nil || !alive || st != start {
		return
	}
	_ = scopeStopper(user, unit)
}

// stopScope stops a transient scope, but only while the process it was created
// for is still the recorded one (matching start time) and still lives in that
// scope. Anything else is left alone: a scope name is never trusted by itself.
func stopScope(user bool, unit string, pid int, start uint64) error {
	if !anyScopeName.MatchString(unit) || pid <= 0 || start == 0 {
		return nil
	}
	st, alive, err := procIdentity(pid)
	if err != nil || !alive || st != start || !inScope(pid, unit) {
		return nil
	}
	return scopeStopper(user, unit)
}

// EnableDetachedCobrowse marks this process as the managed worker, the only
// kind that may re-adopt sessions whose owner has died, and the only one whose
// shutdown leaves detached sessions running. It is a no-op outside a service
// manager (INVOCATION_ID unset), so an interactive run keeps stopping its
// sessions on exit.
func EnableDetachedCobrowse() {
	if scopeSupported && scopeGetenv(scopeInvocationEnv) != "" {
		detachedWorker.Store(true)
	}
}

var detachedWorker atomic.Bool

// StartManagedCobrowse is the worker's co-browse startup: mark this process as
// the managed worker FIRST, then recover, because recovery decides adoption of a
// dead owner's scoped sessions on that mark. Keep both calls here, in order.
func StartManagedCobrowse() { startManagedCobrowse(GetCobrowseSessionManager()) }

func startManagedCobrowse(m *CobrowseSessionManager) {
	EnableDetachedCobrowse()
	m.Recover()
}

// ShutdownSessions is the worker's shutdown path. In the managed worker, a
// non-persistent session running in its own scope is left running (stamped with
// service_restart in case it does not survive) for the next worker to re-adopt;
// everything else is stopped as StopAll does.
func (m *CobrowseSessionManager) ShutdownSessions() error {
	if !detachedWorker.Load() || !m.useResolver || !adoptionSupported {
		return m.StopAll()
	}
	m.mu.Lock()
	all := m.sessions
	m.sessions = make(map[string]*cobrowseSession)
	m.mu.Unlock()
	var lastErr error
	for _, s := range all {
		s.mu.Lock()
		keep := s.proc != nil && s.proc.browserScope != "" && !s.persistent && s.encProfile == nil
		s.mu.Unlock()
		if keep {
			_ = m.MarkSessionEnding(s, EndedReasonServiceRestart,
				"the session outlives a service stop for up to "+detachGrace().String()+" before it is swept")
			continue
		}
		if err := s.teardown(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}
