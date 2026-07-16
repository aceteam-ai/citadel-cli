// Background-service installation for the device daemon (#5959).
//
// Deliberately separate from internal/service (which manages the Citadel node
// agent under the ai.aceteam.citadel label): a laptop in device mode is not a
// node, and flipping it into one later must not collide with this unit. The
// daemon runs as a per-user service — device mode never needs root.
//
//	macOS: ~/Library/LaunchAgents/ai.aceteam.citadel-device.plist
//	Linux: ~/.config/systemd/user/citadel-device.service
package devicemode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	// LaunchdLabel identifies the macOS LaunchAgent.
	LaunchdLabel = "ai.aceteam.citadel-device"
	// SystemdUnitName identifies the Linux systemd user unit.
	SystemdUnitName = "citadel-device.service"
)

// LaunchdPlist renders the LaunchAgent plist that keeps `citadel device run`
// alive across logins. Exported for tests.
func LaunchdPlist(execPath, home string) string {
	logDir := filepath.Join(home, "Library", "Logs", "citadel")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>device</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/citadel-device.log</string>
    <key>StandardErrorPath</key>
    <string>%s/citadel-device.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>%s</string>
    </dict>
</dict>
</plist>
`, LaunchdLabel, execPath, logDir, logDir, home)
}

// SystemdUnit renders the systemd user unit for the device daemon. Exported
// for tests.
func SystemdUnit(execPath string) string {
	return fmt.Sprintf(`[Unit]
Description=AceTeam device identity daemon (citadel device run)
After=network-online.target

[Service]
ExecStart=%s device run
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
`, execPath)
}

// InstallService installs and starts the device daemon as a per-user service
// for the current OS.
func InstallService() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve citadel binary path: %w", err)
	}
	if execPath, err = filepath.EvalSymlinks(execPath); err != nil {
		return fmt.Errorf("resolve citadel binary path: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(execPath)
	case "linux":
		return installSystemdUser(execPath)
	default:
		return fmt.Errorf(
			"automatic service install is not supported on %s yet; "+
				"run 'citadel device run' under your preferred supervisor", runtime.GOOS)
	}
}

// UninstallService stops and removes the per-user device daemon service.
func UninstallService() error {
	switch runtime.GOOS {
	case "darwin":
		pp, err := launchdPlistPath()
		if err != nil {
			return err
		}
		_ = exec.Command("launchctl", "unload", pp).Run()
		if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove plist: %w", err)
		}
		return nil
	case "linux":
		_ = exec.Command("systemctl", "--user", "disable", "--now", SystemdUnitName).Run()
		up, err := systemdUnitPath()
		if err != nil {
			return err
		}
		if err := os.Remove(up); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove unit: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("automatic service uninstall is not supported on %s", runtime.GOOS)
	}
}

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
}

func systemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", SystemdUnitName), nil
}

func installLaunchd(execPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("determine home directory: %w", err)
	}
	pp, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "Library", "Logs", "citadel"), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	if err := os.WriteFile(pp, []byte(LaunchdPlist(execPath, home)), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	// Reload cleanly if a previous copy is loaded.
	_ = exec.Command("launchctl", "unload", pp).Run()
	if out, err := exec.Command("launchctl", "load", pp).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w: %s", err, out)
	}
	fmt.Printf("Installed LaunchAgent: %s\n", pp)
	return nil
}

func installSystemdUser(execPath string) error {
	up, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		return fmt.Errorf("create systemd user dir: %w", err)
	}
	if err := os.WriteFile(up, []byte(SystemdUnit(execPath)), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	if out, err := exec.Command(
		"systemctl", "--user", "enable", "--now", SystemdUnitName,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, out)
	}
	fmt.Printf("Installed systemd user unit: %s\n", up)
	return nil
}
