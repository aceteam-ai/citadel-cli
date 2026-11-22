package service

import (
	"errors"
	"fmt"
	"path/filepath"
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
	SupervisorStopFileName = "citadel-supervisor.stop"
)

// SupervisorLockPath is the per-user supervisor's single-instance lock.
func SupervisorLockPath(nodeConfigDir string) string {
	return filepath.Join(nodeConfigDir, SupervisorLockFileName)
}

// SupervisorStopPath is the stop request `citadel worker stop` writes and the
// supervisor polls for.
func SupervisorStopPath(nodeConfigDir string) string {
	return filepath.Join(nodeConfigDir, SupervisorStopFileName)
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
// by `work --background`. It refuses a relative path (the Run key is evaluated
// from an unrelated working directory) and a path containing a double quote,
// which Windows command-line quoting cannot represent.
func BuildRunValue(exe string) (string, error) {
	exe = strings.TrimSpace(exe)
	if exe == "" {
		return "", errors.New("autostart: executable path is empty")
	}
	if strings.ContainsAny(exe, "\"\r\n\x00") {
		return "", fmt.Errorf("autostart: executable path %q contains a quote or control character", exe)
	}
	if !isAbsExePath(exe) {
		return "", fmt.Errorf("autostart: executable path %q is not absolute", exe)
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

// isAbsExePath accepts a Windows drive or UNC path on any host, so the builder
// is testable off Windows, plus a host-absolute path.
func isAbsExePath(p string) bool {
	if len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	return filepath.IsAbs(p)
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
