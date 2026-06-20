//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const windowsServiceName = "CitadelAgent"
const windowsDisplayName = "Citadel Node Agent"

type windowsManager struct{}

func newPlatformManager() Manager {
	return &windowsManager{}
}

func (m *windowsManager) Install(cfg ServiceConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	if !isWindowsAdmin() {
		return fmt.Errorf("installing a Windows service requires Administrator privileges.\nRight-click your terminal and select 'Run as administrator'")
	}

	// Build binPath for sc.exe. Quotes around the full command are required
	// so that spaces in the path are handled correctly.
	binPath := fmt.Sprintf("\"%s\" %s", cfg.ExecPath, strings.Join(cfg.Args, " "))

	// Delete any existing service (ignore error).
	_ = runCmd("sc.exe", "delete", windowsServiceName)

	// Create the service.
	if err := runCmd("sc.exe", "create", windowsServiceName,
		"binPath=", binPath,
		"DisplayName=", windowsDisplayName,
		"start=", "auto",
		"obj=", "LocalSystem",
	); err != nil {
		return fmt.Errorf("sc create failed: %w", err)
	}
	fmt.Println("Created Windows service: " + windowsServiceName)

	// Set description.
	desc := cfg.Description
	if desc == "" {
		desc = DefaultDescription
	}
	_ = runCmd("sc.exe", "description", windowsServiceName, desc)

	// Configure failure recovery: restart after 60s on each of the first 3 failures.
	_ = runCmd("sc.exe", "failure", windowsServiceName,
		"reset=", "86400",
		"actions=", "restart/60000/restart/60000/restart/60000")

	// Set the CITADEL_SERVICE environment variable for the service.
	// Windows services inherit system environment, so we set it at the
	// machine level. This is best-effort; the service will work without it.
	_ = runCmd("setx", "/M", "CITADEL_SERVICE", "true")

	// Start the service.
	if err := runCmd("sc.exe", "start", windowsServiceName); err != nil {
		return fmt.Errorf("sc start failed: %w", err)
	}

	fmt.Println("Citadel service installed and started.")
	fmt.Println("\nUseful commands:")
	fmt.Printf("  sc query %s       - Check status\n", windowsServiceName)
	fmt.Printf("  sc stop %s        - Stop service\n", windowsServiceName)
	fmt.Printf("  sc start %s       - Start service\n", windowsServiceName)
	fmt.Println("  services.msc                  - Manage via GUI")
	return nil
}

func (m *windowsManager) Uninstall() error {
	if !isWindowsAdmin() {
		return fmt.Errorf("uninstalling a Windows service requires Administrator privileges.\nRight-click your terminal and select 'Run as administrator'")
	}

	// Stop (ignore error).
	_ = runCmd("sc.exe", "stop", windowsServiceName)
	fmt.Println("Stopped " + windowsServiceName)

	if err := runCmd("sc.exe", "delete", windowsServiceName); err != nil {
		return fmt.Errorf("sc delete failed: %w", err)
	}
	fmt.Println("Deleted " + windowsServiceName)
	fmt.Println("Citadel service uninstalled.")
	return nil
}

func (m *windowsManager) Start() error {
	return runCmd("sc.exe", "start", windowsServiceName)
}

func (m *windowsManager) Stop() error {
	return runCmd("sc.exe", "stop", windowsServiceName)
}

func (m *windowsManager) Status() (*ServiceStatus, error) {
	st := &ServiceStatus{}

	out, err := exec.Command("sc.exe", "query", windowsServiceName).Output()
	if err != nil {
		// Service not found.
		return st, nil
	}

	st.Installed = true

	// Parse sc query output for STATE line.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "STATE") {
			// e.g. "STATE              : 4  RUNNING"
			if strings.Contains(line, "RUNNING") {
				st.Running = true
			}
		}
		if strings.HasPrefix(line, "PID") || strings.Contains(line, "PID") {
			// Parse PID if present: "PID                : 1234"
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				pidStr := strings.TrimSpace(parts[1])
				var pid int
				if _, scanErr := fmt.Sscanf(pidStr, "%d", &pid); scanErr == nil && pid > 0 {
					st.PID = pid
				}
			}
		}
	}

	// Fetch recent Event Log entries (best-effort).
	logOut, logErr := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`Get-EventLog -LogName Application -Source '%s' -Newest 10 -ErrorAction SilentlyContinue | Format-List -Property TimeGenerated,Message`, windowsServiceName),
	).Output()
	if logErr == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(logOut)), "\n") {
			l = strings.TrimSpace(l)
			if l != "" {
				st.RecentLogs = append(st.RecentLogs, l)
			}
		}
	}

	return st, nil
}

// isWindowsAdmin checks for admin privileges by trying to open a protected handle.
func isWindowsAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	return err == nil
}

// runCmd executes a command, forwarding stdout/stderr.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
