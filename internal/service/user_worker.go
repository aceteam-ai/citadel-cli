package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Per-user worker (citadel-cli#1867 C3): a standard Windows user cannot install
// the LocalSystem service (windows.go needs Administrator), so the worker is kept
// running by an HKCU Run value that starts `citadel work --background` at logon,
// and by a small in-binary supervisor (supervisor.go) that restarts it.
//
// This file holds the platform-neutral parts: the Run value format, its parser,
// and the file names the supervisor and `citadel worker stop` agree on. The
// registry access is in user_windows.go; other OSes get user_other.go.

// UserAutostartValueName is the HKCU ...\CurrentVersion\Run value Citadel owns.
const UserAutostartValueName = "CitadelWorker"

// UserRunKeyPath is the per-user Run key, relative to HKEY_CURRENT_USER.
const UserRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// BackgroundWorkArgs is what the Run value starts after the executable.
var BackgroundWorkArgs = []string{"work", "--background"}

// Supervisor coordination files, kept in the node config dir beside the worker
// lock so every invocation that resolves the same node finds the same files.
const (
	SupervisorLockFileName = "citadel-supervisor.lock"
	// SupervisorStopFilePrefix plus a supervisor PID is a stop request for that
	// supervisor only. Naming the target means a request can never be lost to,
	// or misapplied by, a supervisor that started in between.
	SupervisorStopFilePrefix = "citadel-supervisor.stop."
)

// SupervisorLockPath is the per-user supervisor's single-instance lock.
func SupervisorLockPath(nodeConfigDir string) string {
	return filepath.Join(nodeConfigDir, SupervisorLockFileName)
}

// SupervisorStopPath is the stop request for the supervisor with PID pid, which
// `citadel worker stop` writes and that supervisor polls for.
func SupervisorStopPath(nodeConfigDir string, pid int) string {
	return filepath.Join(nodeConfigDir, SupervisorStopFilePrefix+strconv.Itoa(pid))
}

// RemoveStaleStopRequests deletes stop requests addressed to any PID other than
// self. The caller must hold the supervisor lock: then no other PID is a live
// supervisor, so every such request is stale.
func RemoveStaleStopRequests(nodeConfigDir string, self int) {
	matches, _ := filepath.Glob(filepath.Join(nodeConfigDir, SupervisorStopFilePrefix+"*"))
	keep := SupervisorStopPath(nodeConfigDir, self)
	for _, m := range matches {
		if m != keep {
			_ = os.Remove(m)
		}
	}
}

// ErrUserWorkerDirUntrusted: the supervisor lock and stop requests must live in
// a directory inside this user's own profile.
var ErrUserWorkerDirUntrusted = errors.New("the node config dir is not inside your user profile, so the background worker will not use it")

// CheckUserWorkerDir accepts dir only when it lies strictly inside one of homes
// (the user's profile dirs), after resolving symlinks on the existing part of
// each path. A machine-wide or shared node dir is refused, so another account
// cannot plant or remove the supervisor's lock and stop requests.
func CheckUserWorkerDir(dir string, homes []string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return ErrUserWorkerDirUntrusted
	}
	target := resolveExisting(dir)
	for _, h := range homes {
		if h == "" || !filepath.IsAbs(h) {
			continue
		}
		rel, err := filepath.Rel(resolveExisting(h), target)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return nil
		}
	}
	return ErrUserWorkerDirUntrusted
}

// UserProfileDirs are the current user's own profile roots.
func UserProfileDirs() []string {
	var dirs []string
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, h)
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		dirs = append(dirs, v)
	}
	return dirs
}

// resolveExisting resolves symlinks on the longest existing prefix of p.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// ErrUserAutostartUnsupported is returned off Windows, where the worker runs
// under systemd or launchd (`citadel service install`) instead.
var ErrUserAutostartUnsupported = errors.New("per-user autostart is only used on Windows; on Linux and macOS run 'citadel service install'")

// UserAutostartStatus is the state of the Run value.
type UserAutostartStatus struct {
	// Set is true when the value exists.
	Set bool
	// Value is the raw command line stored in the value.
	Value string
	// Exe is the executable parsed out of Value ("" when it cannot be parsed).
	Exe string
	// PointsAtThis is true when Exe names the executable passed to the status
	// call and the arguments are exactly BackgroundWorkArgs.
	PointsAtThis bool
	// DisabledByUser is true when the user switched the entry off in Settings or
	// Task Manager (Windows StartupApproved); the value then does not run.
	DisabledByUser bool
}

// BuildRunValue renders the Run value for exe: the quoted absolute path followed
// by `work --background`. It accepts only a local drive path (C:\...). It
// refuses a relative path (the Run key is evaluated from an unrelated working
// directory), a UNC or device path (a network share could change under the
// user at every sign-in), and a path containing a double quote, which Windows
// command-line quoting cannot represent.
func BuildRunValue(exe string) (string, error) {
	exe = strings.TrimSpace(exe)
	if exe == "" {
		return "", errors.New("autostart: executable path is empty")
	}
	if strings.ContainsAny(exe, "\"\r\n\x00") {
		return "", fmt.Errorf("autostart: executable path %q contains a quote or control character", exe)
	}
	if !isLocalDrivePath(exe) {
		return "", fmt.Errorf("autostart: executable path %q is not an absolute local drive path", exe)
	}
	return `"` + exe + `" ` + strings.Join(BackgroundWorkArgs, " "), nil
}

// ParseRunValue splits a Run value into its executable and arguments. It
// accepts the quoted form BuildRunValue writes and an unquoted path with no
// spaces; it reports false for anything else.
func ParseRunValue(value string) (exe string, args []string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil, false
	}
	var rest string
	if strings.HasPrefix(value, `"`) {
		end := strings.Index(value[1:], `"`)
		if end < 0 {
			return "", nil, false
		}
		exe = value[1 : end+1]
		rest = value[end+2:]
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			return "", nil, false
		}
	} else {
		fields := strings.Fields(value)
		exe = fields[0]
		rest = strings.TrimPrefix(value, exe)
	}
	if exe == "" {
		return "", nil, false
	}
	return exe, strings.Fields(rest), true
}

// autostartStatusFor interprets a raw Run value (set=false means absent) for the
// executable thisExe.
func autostartStatusFor(set bool, value, thisExe string) UserAutostartStatus {
	st := UserAutostartStatus{Set: set, Value: value}
	if !set {
		return st
	}
	exe, args, ok := ParseRunValue(value)
	if !ok {
		return st
	}
	st.Exe = exe
	st.PointsAtThis = sameExePath(exe, thisExe) && equalArgs(args, BackgroundWorkArgs)
	return st
}

// isLocalDrivePath accepts only "X:\..." (or "X:/..."), checked by hand so the
// builder is testable off Windows. UNC, device (\\?\, \\.\) and
// drive-relative paths are refused.
func isLocalDrivePath(p string) bool {
	return len(p) >= 4 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// sameExePath compares two executable paths the way Windows does: separators
// and case do not matter.
func sameExePath(a, b string) bool {
	norm := func(p string) string {
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "/", `\`))
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
