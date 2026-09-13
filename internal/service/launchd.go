//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// launchdLabel is defined in launchd_plist.go (non-tagged) so the pure helpers
// there can share it.

type launchdManager struct{}

func newPlatformManager() Manager {
	return &launchdManager{}
}

// plistPath returns the path for the launchd plist.
// User mode:  ~/Library/LaunchAgents/ai.aceteam.citadel.plist
// System mode: /Library/LaunchDaemons/ai.aceteam.citadel.plist
func plistPath(userMode bool) (string, error) {
	if userMode {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
	}
	return filepath.Join("/Library/LaunchDaemons", launchdLabel+".plist"), nil
}

// logDir returns the directory for launchd service logs.
func logDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "citadel"), nil
}

// GeneratePlist produces a launchd plist XML string from the given config.
// Exported so tests can verify the output. It resolves the real home/log dirs
// and delegates the pure rendering (including the RunAtLoad/KeepAlive
// reboot-survival keys) to renderLaunchdPlist.
func GeneratePlist(cfg ServiceConfig) (string, error) {
	if cfg.Description == "" {
		cfg.Description = DefaultDescription
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}

	ld, err := logDir()
	if err != nil {
		return "", err
	}

	return renderLaunchdPlist(launchdPlistInput{
		Label:     launchdLabel,
		ExecPath:  cfg.ExecPath,
		Args:      cfg.Args,
		HomeDir:   home,
		LogDir:    ld,
		RunAtLoad: true,
		KeepAlive: true,
		PathEnv:   launchdServicePATH,
	}), nil
}

func (m *launchdManager) Install(cfg ServiceConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	// System mode requires root.
	if !cfg.UserMode && os.Geteuid() != 0 {
		return fmt.Errorf("installing a system daemon requires root privileges.\nRun: sudo citadel service install --system")
	}

	plistContent, err := GeneratePlist(cfg)
	if err != nil {
		return fmt.Errorf("failed to generate plist: %w", err)
	}

	pp, err := plistPath(cfg.UserMode)
	if err != nil {
		return err
	}

	// Ensure parent directory + log directory exist.
	if err := os.MkdirAll(filepath.Dir(pp), 0755); err != nil {
		return fmt.Errorf("failed to create plist directory: %w", err)
	}
	ld, err := logDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ld, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Idempotency: if the plist on disk is already byte-identical and the
	// service is running, re-running install (e.g. a second `citadel init`)
	// must not bootout/bootstrap a healthy worker and drop its in-flight jobs.
	if existing, rerr := os.ReadFile(pp); rerr == nil && string(existing) == plistContent {
		if st, _ := m.Status(); st != nil && st.Running {
			fmt.Printf("Citadel service already installed and running (%s).\n", pp)
			return nil
		}
	}

	if err := os.WriteFile(pp, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}
	fmt.Printf("Created plist: %s\n", pp)

	// (Re)load the service via bootout+bootstrap (falling back to legacy
	// load/unload where bootstrap into a gui/<uid> domain is unavailable).
	if err := m.reload(pp, cfg.UserMode); err != nil {
		return fmt.Errorf("launchctl (re)load failed: %w", err)
	}

	fmt.Println("Citadel service installed and started.")

	if cfg.UserMode {
		fmt.Println("\nNote: User LaunchAgents start at login. For headless/boot-time startup,")
		fmt.Println("install as a system daemon: sudo citadel service install --system")
	}

	fmt.Println("\nUseful commands:")
	fmt.Printf("  launchctl list %s            - Check status\n", launchdLabel)
	fmt.Printf("  citadel service stop         - Stop service\n")
	fmt.Printf("  tail -f %s/citadel.log       - View logs\n", ld)
	return nil
}

func (m *launchdManager) Uninstall() error {
	userMode := detectInstalledMode()

	pp, err := plistPath(userMode)
	if err != nil {
		return err
	}

	// Bootout (ignore errors — may already be booted out / not loaded), with a
	// legacy unload fallback.
	target := launchdDomainTarget(userMode, resolveLaunchUID())
	if err := runCmdQuiet("launchctl", bootoutArgs(target, pp)...); err != nil {
		_ = runCmdQuiet("launchctl", "unload", pp)
	}
	fmt.Println("Unloaded citadel service")

	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist: %w", err)
	}
	fmt.Printf("Removed %s\n", pp)
	fmt.Println("Citadel service uninstalled.")
	return nil
}

func (m *launchdManager) Start() error {
	userMode := detectInstalledMode()
	pp, err := plistPath(userMode)
	if err != nil {
		return err
	}
	target := launchdDomainTarget(userMode, resolveLaunchUID())
	if err := runCmd("launchctl", bootstrapArgs(target, pp)...); err != nil {
		// Fallback to the legacy verb (e.g. an SSH session with no Aqua/GUI
		// session where bootstrap into gui/<uid> fails).
		return runCmd("launchctl", "load", pp)
	}
	return nil
}

func (m *launchdManager) Stop() error {
	userMode := detectInstalledMode()
	pp, err := plistPath(userMode)
	if err != nil {
		return err
	}
	target := launchdDomainTarget(userMode, resolveLaunchUID())
	if err := runCmd("launchctl", bootoutArgs(target, pp)...); err != nil {
		return runCmd("launchctl", "unload", pp)
	}
	return nil
}

// reload (re)loads the plist via bootout+bootstrap, the modern launchctl verbs.
// bootout is best-effort (the service may not currently be loaded); bootstrap
// is the meaningful step. If bootstrap fails (e.g. no Aqua session for a
// gui/<uid> target over SSH), it falls back to the legacy unload+load pair so a
// manual `citadel service install` still works in those environments.
func (m *launchdManager) reload(pp string, userMode bool) error {
	target := launchdDomainTarget(userMode, resolveLaunchUID())
	_ = runCmdQuiet("launchctl", bootoutArgs(target, pp)...)
	if err := runCmd("launchctl", bootstrapArgs(target, pp)...); err != nil {
		_ = runCmdQuiet("launchctl", "unload", pp)
		return runCmd("launchctl", "load", pp)
	}
	return nil
}

// runCmdQuiet runs a command discarding its output, for best-effort calls (a
// bootout/unload of a service that may not be loaded prints a noisy
// "No such process" that should not reach the operator).
func runCmdQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func (m *launchdManager) Status() (*ServiceStatus, error) {
	userMode := detectInstalledMode()
	pp, err := plistPath(userMode)
	if err != nil {
		return &ServiceStatus{Installed: false}, nil
	}
	if _, err := os.Stat(pp); os.IsNotExist(err) {
		return &ServiceStatus{Installed: false}, nil
	}

	st := &ServiceStatus{Installed: true}

	// Parse `launchctl list <label>` to get PID and status.
	out, err := exec.Command("launchctl", "list", launchdLabel).Output()
	if err != nil {
		// Service is installed but not loaded.
		return st, nil
	}

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "\"PID\"") || strings.HasPrefix(line, "PID") {
			// launchctl list output: "PID" = 12345;
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				pidStr := strings.TrimRight(strings.TrimSpace(parts[1]), ";")
				if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
					st.PID = pid
					st.Running = true
				}
			}
		}
	}

	// Fallback: if we got output without error, the service is loaded.
	if !st.Running && len(out) > 0 {
		// Check if PID appears in tab-separated format (launchctl list output varies).
		lines := strings.Split(string(out), "\n")
		for _, l := range lines {
			fields := strings.Fields(l)
			if len(fields) >= 3 && fields[2] == launchdLabel {
				if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
					st.PID = pid
					st.Running = true
				}
			}
		}
	}

	// Fetch recent log lines (best-effort).
	ld, err := logDir()
	if err == nil {
		logFile := filepath.Join(ld, "citadel.log")
		if logOut, err := exec.Command("tail", "-n", "10", logFile).Output(); err == nil {
			for _, l := range strings.Split(strings.TrimSpace(string(logOut)), "\n") {
				if l != "" {
					st.RecentLogs = append(st.RecentLogs, l)
				}
			}
		}
	}

	return st, nil
}

// detectInstalledMode checks whether the plist is installed as user or system.
func detectInstalledMode() bool {
	if p, err := plistPath(true); err == nil {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// runCmd executes a command, forwarding stdout/stderr.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
