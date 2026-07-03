//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type systemdManager struct{}

func newPlatformManager() Manager {
	return &systemdManager{}
}

// unitFilePath returns the path to the systemd unit file.
// User mode:  ~/.config/systemd/user/citadel.service
// System mode: /etc/systemd/system/citadel.service
func unitFilePath(userMode bool) (string, error) {
	if userMode {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, ".config", "systemd", "user", ServiceName+".service"), nil
	}
	return filepath.Join("/etc/systemd/system", ServiceName+".service"), nil
}

// GenerateUnitFile produces a systemd unit file from the given config.
// Exported so tests can verify the output without needing systemctl.
func GenerateUnitFile(cfg ServiceConfig) (string, error) {
	if cfg.Description == "" {
		cfg.Description = DefaultDescription
	}

	execLine := cfg.ExecPath
	if len(cfg.Args) > 0 {
		execLine += " " + strings.Join(cfg.Args, " ")
	}

	if cfg.UserMode {
		return generateUserUnit(cfg.Description, execLine), nil
	}
	return generateSystemUnit(cfg, execLine)
}

func generateUserUnit(description, execLine string) string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target
Wants=network-online.target
# Defense in depth against a crash-loop self-DoS (#443): if the process keeps
# failing fast, enter a cooldown instead of a 10s restart storm.
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
# Exponential restart backoff: start at 10s and grow to 5m so a genuinely
# failing start does not hammer the control plane. The worker also backs off
# in-process, so this is a secondary safety net.
RestartSec=10
RestartSteps=5
RestartMaxDelaySec=300
StandardOutput=journal+console
StandardError=journal+console
Environment=CITADEL_SERVICE=true

[Install]
WantedBy=default.target
`, description, execLine)
}

func generateSystemUnit(cfg ServiceConfig, execLine string) (string, error) {
	// Resolve the owning user (the person who ran sudo, or root).
	username := os.Getenv("SUDO_USER")
	if username == "" {
		username = "root"
	}

	homeDir, err := resolveHomeDir(username)
	if err != nil {
		return "", err
	}

	group := username
	citadelDir := filepath.Join(homeDir, ".citadel-cli")
	// The tsnet state (machine key) lives under <home>/citadel-node/network by
	// default. With ProtectHome=read-only, that path is NOT writable unless it is
	// listed in ReadWritePaths — otherwise the service cannot persist the machine
	// key, so on every restart tsnet mints a fresh one and Headscale registers a
	// duplicate node (aceteam-ai/citadel-cli#383). Grant write access to it.
	nodeStateDir := filepath.Join(homeDir, "citadel-node")

	return fmt.Sprintf(`[Unit]
Description=%s
Documentation=https://github.com/aceteam-ai/citadel-cli
After=network-online.target docker.service
Wants=network-online.target
# Defense in depth against a crash-loop self-DoS (#443): if the process keeps
# failing fast, enter a cooldown instead of a 10s restart storm.
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
# Exponential restart backoff: start at 10s and grow to 5m so a genuinely
# failing start does not hammer the control plane. The worker also backs off
# in-process, so this is a secondary safety net.
RestartSec=10
RestartSteps=5
RestartMaxDelaySec=300
User=%s
Group=%s
Environment=HOME=%s
Environment=CITADEL_SERVICE=true
StandardOutput=journal+console
StandardError=journal+console
SyslogIdentifier=citadel

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s %s

[Install]
WantedBy=multi-user.target
`, cfg.Description, execLine, username, group, homeDir, citadelDir, nodeStateDir), nil
}

func resolveHomeDir(username string) (string, error) {
	if username == "root" {
		return "/root", nil
	}
	u, err := user.Lookup(username)
	if err != nil {
		return "", fmt.Errorf("failed to lookup user %s: %w", username, err)
	}
	return u.HomeDir, nil
}

// Install creates the systemd unit file, reloads the daemon, enables, and starts
// the service.
func (m *systemdManager) Install(cfg ServiceConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	// System mode requires root.
	if !cfg.UserMode && os.Geteuid() != 0 {
		return fmt.Errorf("installing a system service requires root privileges.\nRun: sudo citadel service install --system")
	}

	unitContent, err := GenerateUnitFile(cfg)
	if err != nil {
		return fmt.Errorf("failed to generate unit file: %w", err)
	}

	unitPath, err := unitFilePath(cfg.UserMode)
	if err != nil {
		return err
	}

	// Ensure parent directory exists (user mode).
	if cfg.UserMode {
		if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
			return fmt.Errorf("failed to create unit directory: %w", err)
		}
	}

	if err := os.WriteFile(unitPath, []byte(unitContent), 0644); err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}
	fmt.Printf("Created unit file: %s\n", unitPath)

	// Reload, enable, start.
	ctl := systemctlArgs(cfg.UserMode)

	if err := runCmd("systemctl", append(ctl, "daemon-reload")...); err != nil {
		return fmt.Errorf("daemon-reload failed: %w", err)
	}
	if err := runCmd("systemctl", append(ctl, "enable", ServiceName)...); err != nil {
		return fmt.Errorf("enable failed: %w", err)
	}
	if err := runCmd("systemctl", append(ctl, "start", ServiceName)...); err != nil {
		return fmt.Errorf("start failed: %w", err)
	}

	// Enable lingering so the user service survives logout and starts on boot
	// without requiring a login session. Best-effort; non-fatal if it fails
	// (e.g., loginctl not available in a container).
	if cfg.UserMode {
		u, _ := user.Current()
		if u != nil {
			if err := runCmd("loginctl", "enable-linger", u.Username); err != nil {
				fmt.Printf("Warning: loginctl enable-linger failed: %v\n", err)
				fmt.Println("  The service may not start on boot without an active login session.")
				fmt.Println("  Run manually: loginctl enable-linger " + u.Username)
			} else {
				fmt.Println("Enabled linger (service will start on boot without login).")
			}
		}
	}

	fmt.Println("Citadel service installed and started.")
	m.printHelp(cfg.UserMode)
	return nil
}

// Uninstall stops, disables, and removes the unit file.
func (m *systemdManager) Uninstall() error {
	userMode := detectInstalledMode()

	if !userMode && os.Geteuid() != 0 {
		return fmt.Errorf("uninstalling the system service requires root.\nRun: sudo citadel service uninstall")
	}

	ctl := systemctlArgs(userMode)

	// Stop + disable (ignore errors — service may already be stopped).
	_ = runCmd("systemctl", append(ctl, "stop", ServiceName)...)
	_ = runCmd("systemctl", append(ctl, "disable", ServiceName)...)

	unitPath, err := unitFilePath(userMode)
	if err != nil {
		return err
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove unit file: %w", err)
	}
	fmt.Printf("Removed %s\n", unitPath)

	_ = runCmd("systemctl", append(ctl, "daemon-reload")...)
	fmt.Println("Citadel service uninstalled.")
	return nil
}

func (m *systemdManager) Start() error {
	userMode := detectInstalledMode()
	ctl := systemctlArgs(userMode)
	return runCmd("systemctl", append(ctl, "start", ServiceName)...)
}

func (m *systemdManager) Stop() error {
	userMode := detectInstalledMode()
	ctl := systemctlArgs(userMode)
	return runCmd("systemctl", append(ctl, "stop", ServiceName)...)
}

func (m *systemdManager) Status() (*ServiceStatus, error) {
	userMode := detectInstalledMode()
	ctl := systemctlArgs(userMode)

	unitPath, err := unitFilePath(userMode)
	if err != nil {
		return &ServiceStatus{Installed: false}, nil
	}
	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return &ServiceStatus{Installed: false}, nil
	}

	st := &ServiceStatus{Installed: true}

	// Parse `systemctl show` for state + PID.
	args := append(ctl, "show", ServiceName,
		"--property=ActiveState,MainPID,ExecMainStartTimestamp")
	out, err := exec.Command("systemctl", args...).Output()
	if err != nil {
		return st, nil
	}

	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "ActiveState":
			st.Running = val == "active"
		case "MainPID":
			if pid, err := strconv.Atoi(val); err == nil {
				st.PID = pid
			}
		}
	}

	// Fetch recent log lines (best-effort).
	jArgs := []string{"-u", ServiceName, "-n", "10", "--no-pager"}
	if userMode {
		jArgs = append([]string{"--user"}, jArgs...)
	}
	if logOut, err := exec.Command("journalctl", jArgs...).Output(); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(logOut)), "\n") {
			if l != "" {
				st.RecentLogs = append(st.RecentLogs, l)
			}
		}
	}

	return st, nil
}

func (m *systemdManager) printHelp(userMode bool) {
	flag := ""
	sudo := ""
	if userMode {
		flag = " --user"
	} else {
		sudo = "sudo "
	}
	jflag := ""
	if userMode {
		jflag = " --user"
	}
	fmt.Println("\nUseful commands:")
	fmt.Printf("  %ssystemctl%s status %s   - Check status\n", sudo, flag, ServiceName)
	fmt.Printf("  %ssystemctl%s stop %s     - Stop service\n", sudo, flag, ServiceName)
	fmt.Printf("  %ssystemctl%s restart %s  - Restart service\n", sudo, flag, ServiceName)
	fmt.Printf("  journalctl%s -u %s -f      - View logs\n", jflag, ServiceName)
}

// detectInstalledMode checks whether the unit is installed as a user or
// system service. User mode is checked first.
func detectInstalledMode() bool {
	if p, err := unitFilePath(true); err == nil {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// systemctlArgs returns the extra arguments for user vs system mode.
func systemctlArgs(userMode bool) []string {
	if userMode {
		return []string{"--user"}
	}
	return nil
}

// runCmd executes a command, forwarding stdout/stderr.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
