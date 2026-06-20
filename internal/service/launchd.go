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

const launchdLabel = "ai.aceteam.citadel"

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
// Exported so tests can verify the output.
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

	// Build ProgramArguments array.
	var progArgs strings.Builder
	progArgs.WriteString(fmt.Sprintf("        <string>%s</string>\n", cfg.ExecPath))
	for _, a := range cfg.Args {
		progArgs.WriteString(fmt.Sprintf("        <string>%s</string>\n", a))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/citadel.log</string>
    <key>StandardErrorPath</key>
    <string>%s/citadel-error.log</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>%s</string>
        <key>CITADEL_SERVICE</key>
        <string>true</string>
    </dict>
</dict>
</plist>
`, launchdLabel, progArgs.String(), ld, ld, home, home), nil
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

	if err := os.WriteFile(pp, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}
	fmt.Printf("Created plist: %s\n", pp)

	// Load the service.
	if err := runCmd("launchctl", "load", pp); err != nil {
		return fmt.Errorf("launchctl load failed: %w", err)
	}

	fmt.Println("Citadel service installed and started.")

	if cfg.UserMode {
		fmt.Println("\nNote: User LaunchAgents start at login. For headless/boot-time startup,")
		fmt.Println("install as a system daemon: sudo citadel service install --system")
	}

	fmt.Println("\nUseful commands:")
	fmt.Printf("  launchctl list %s            - Check status\n", launchdLabel)
	fmt.Printf("  launchctl unload %s          - Stop service\n", pp)
	fmt.Printf("  tail -f %s/citadel.log       - View logs\n", ld)
	return nil
}

func (m *launchdManager) Uninstall() error {
	userMode := detectInstalledMode()

	pp, err := plistPath(userMode)
	if err != nil {
		return err
	}

	// Unload (ignore errors — may already be unloaded).
	_ = runCmd("launchctl", "unload", pp)
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
	return runCmd("launchctl", "load", pp)
}

func (m *launchdManager) Stop() error {
	userMode := detectInstalledMode()
	pp, err := plistPath(userMode)
	if err != nil {
		return err
	}
	return runCmd("launchctl", "unload", pp)
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
