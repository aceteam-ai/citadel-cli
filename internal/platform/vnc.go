package platform

import (
	"crypto/des"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultVNCPort is the standard VNC port.
const DefaultVNCPort = 5900

// vncDESKey is the well-known fixed key used by all VNC implementations
// (RFB protocol) to DES-encrypt stored passwords. This is NOT a secret --
// it is hardcoded in every VNC implementation's source code.
var vncDESKey = []byte{0x17, 0x52, 0x6b, 0x06, 0x23, 0x4e, 0x58, 0x07}

// embeddedVNCPort is set by the TUI when the embedded VNC server starts.
// The status collector reads this to report vnc_port in heartbeats.
var embeddedVNCPort int

// SetEmbeddedVNCPort records that the embedded VNC server is running on port.
func SetEmbeddedVNCPort(port int) { embeddedVNCPort = port }

// ClearEmbeddedVNCPort records that the embedded VNC server has stopped.
func ClearEmbeddedVNCPort() { embeddedVNCPort = 0 }

// EmbeddedVNCPort returns the port of the embedded VNC server, or 0 if not running.
func EmbeddedVNCPort() int { return embeddedVNCPort }

// VNCManager interface defines operations for VNC server management.
type VNCManager interface {
	IsInstalled() bool
	Install() error
	Uninstall() error
	Configure(password string, port int) error
	Start() error
	Stop() error
	IsRunning() bool
	Port() int
}

// GetVNCManager returns the appropriate VNC manager for the current OS.
func GetVNCManager() VNCManager {
	switch OS() {
	case "windows":
		return &WindowsVNCManager{}
	case "linux":
		return &LinuxVNCManager{}
	case "darwin":
		return &DarwinVNCManager{}
	default:
		return &LinuxVNCManager{}
	}
}

// GenerateVNCPassword generates a random alphanumeric password.
// VNC passwords are capped at 8 characters by the DES encryption scheme.
func GenerateVNCPassword() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 8
	result := make([]byte, length)
	for i := range result {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("failed to generate random password: %w", err)
		}
		result[i] = charset[idx.Int64()]
	}
	return string(result), nil
}

// ValidateVNCPort checks whether a port number is valid for VNC.
func ValidateVNCPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", port)
	}
	return nil
}

// encryptVNCPassword encrypts a password using the standard VNC DES scheme.
// The password is truncated/padded to exactly 8 bytes, then DES-encrypted
// with the well-known fixed VNC key. Returns the hex-encoded ciphertext
// suitable for writing to the Windows registry as REG_BINARY.
func encryptVNCPassword(password string) (string, error) {
	// Truncate or pad to exactly 8 bytes
	pwBytes := make([]byte, 8)
	copy(pwBytes, []byte(password))

	// DES requires the key bits to be reversed per byte (VNC quirk)
	reversedKey := make([]byte, 8)
	for i, b := range vncDESKey {
		reversedKey[i] = reverseBits(b)
	}

	block, err := des.NewCipher(reversedKey)
	if err != nil {
		return "", fmt.Errorf("failed to create DES cipher: %w", err)
	}

	encrypted := make([]byte, 8)
	block.Encrypt(encrypted, pwBytes)

	return hex.EncodeToString(encrypted), nil
}

// reverseBits reverses the bit order in a byte.
// VNC's DES implementation uses reversed bit ordering for the key.
func reverseBits(b byte) byte {
	var result byte
	for i := 0; i < 8; i++ {
		result = (result << 1) | (b & 1)
		b >>= 1
	}
	return result
}

// --- Windows implementation ---

// WindowsVNCManager manages TightVNC on Windows.
type WindowsVNCManager struct{}

func (w *WindowsVNCManager) IsInstalled() bool {
	// Check if TightVNC service is registered (works even if not running)
	cmd := exec.Command("sc", "query", "tvnserver")
	output, err := cmd.CombinedOutput()
	if err == nil && strings.Contains(string(output), "tvnserver") {
		return true
	}
	// Fallback: check for the executable
	cmd = exec.Command("cmd", "/c", `if exist "C:\Program Files\TightVNC\tvnserver.exe" echo found`)
	output, err = cmd.Output()
	return err == nil && strings.Contains(string(output), "found")
}

func (w *WindowsVNCManager) Install() error {
	if w.IsInstalled() {
		return nil // Already installed, idempotent
	}

	fmt.Println("Installing TightVNC...")

	// Try winget first (no password setting via winget -- configure separately)
	cmd := exec.Command("winget", "install", "GlavSoft.TightVNC",
		"--accept-package-agreements", "--accept-source-agreements", "--silent")
	if err := cmd.Run(); err == nil {
		fmt.Println("TightVNC installed via winget.")
		return nil
	}

	// Fallback: download MSI and run silent install
	fmt.Println("winget install failed, attempting MSI fallback...")
	downloadCmd := exec.Command("powershell", "-NoProfile", "-Command",
		`$url = "https://www.tightvnc.com/download/2.8.85/tightvnc-2.8.85-gpl-setup-64bit.msi"; `+
			`$dest = "$env:TEMP\tightvnc-setup.msi"; `+
			`Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing; `+
			`Write-Output $dest`)
	msiPathBytes, err := downloadCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to download TightVNC MSI: %w", err)
	}
	msiPath := strings.TrimSpace(string(msiPathBytes))

	installCmd := exec.Command("msiexec", "/i", msiPath,
		"/quiet", "/norestart",
		"ADDLOCAL=Server",
		"SERVER_REGISTER_AS_SERVICE=1",
		"SERVER_ADD_FIREWALL_EXCEPTION=1")
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("failed to install TightVNC via MSI: %w", err)
	}

	fmt.Println("TightVNC installed via MSI.")
	return nil
}

func (w *WindowsVNCManager) Uninstall() error {
	// Stop the service first (best-effort)
	if err := w.Stop(); err != nil {
		fmt.Printf("Warning: failed to stop VNC service: %v\n", err)
	}

	// Find the MSI product GUID for TightVNC
	cmd := exec.Command("wmic", "product", "where", "name like '%TightVNC%'",
		"get", "IdentifyingNumber", "/format:list")
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("Warning: could not query TightVNC product GUID: %v\n", err)
	}

	// Parse GUID from "IdentifyingNumber={GUID}" output
	guid := ""
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(line, "IdentifyingNumber=") {
			guid = strings.TrimPrefix(line, "IdentifyingNumber=")
			guid = strings.TrimSpace(guid)
			break
		}
	}

	// Run msiexec to uninstall if we found a GUID
	if guid != "" {
		uninstallCmd := exec.Command("msiexec", "/x", guid, "/quiet", "/norestart")
		if err := uninstallCmd.Run(); err != nil {
			return fmt.Errorf("failed to uninstall TightVNC (GUID %s): %w", guid, err)
		}
		fmt.Println("TightVNC uninstalled via MSI.")
	} else {
		fmt.Println("Warning: TightVNC MSI product not found, skipping MSI removal.")
	}

	// Delete firewall rule (best-effort)
	fwCmd := exec.Command("netsh", "advfirewall", "firewall", "delete", "rule",
		"name=TightVNC Server (Port 5900)")
	if err := fwCmd.Run(); err != nil {
		fmt.Printf("Warning: failed to remove firewall rule: %v\n", err)
	}

	// Clean registry (best-effort, may already be gone)
	regCmd := exec.Command("reg", "delete", `HKLM\SOFTWARE\TightVNC`, "/f")
	if err := regCmd.Run(); err != nil {
		fmt.Printf("Warning: failed to clean TightVNC registry keys: %v\n", err)
	}

	return nil
}

func (w *WindowsVNCManager) Configure(password string, port int) error {
	if err := ValidateVNCPort(port); err != nil {
		return err
	}

	// Truncate password to 8 chars (VNC DES limit)
	if len(password) > 8 {
		password = password[:8]
	}

	// Set the VNC port via registry (plain DWORD, safe to write directly)
	portCmd := exec.Command("reg", "add",
		`HKLM\SOFTWARE\TightVNC\Server`,
		"/v", "RfbPort", "/t", "REG_DWORD",
		"/d", strconv.Itoa(port), "/f")
	if err := portCmd.Run(); err != nil {
		return fmt.Errorf("failed to set VNC port in registry: %w", err)
	}

	// Encrypt password using the standard VNC DES scheme and write as REG_BINARY.
	// TightVNC stores Password as a DES-encrypted blob, not plaintext.
	encryptedHex, err := encryptVNCPassword(password)
	if err != nil {
		return fmt.Errorf("failed to encrypt VNC password: %w", err)
	}

	// Write the encrypted password as REG_BINARY
	pwCmd := exec.Command("reg", "add",
		`HKLM\SOFTWARE\TightVNC\Server`,
		"/v", "Password", "/t", "REG_BINARY",
		"/d", encryptedHex, "/f")
	if err := pwCmd.Run(); err != nil {
		return fmt.Errorf("failed to set VNC password in registry: %w", err)
	}

	// Enable VNC authentication in registry
	authCmd := exec.Command("reg", "add",
		`HKLM\SOFTWARE\TightVNC\Server`,
		"/v", "UseVncAuthentication", "/t", "REG_DWORD",
		"/d", "1", "/f")
	if err := authCmd.Run(); err != nil {
		// Non-fatal: auth might already be enabled
		fmt.Printf("Warning: failed to enable VNC authentication in registry: %v\n", err)
	}

	// Add Windows Firewall rule for the VNC port
	ruleName := fmt.Sprintf("TightVNC Server (Port %d)", port)
	fwCmd := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+ruleName,
		"dir=in",
		"action=allow",
		"protocol=tcp",
		fmt.Sprintf("localport=%d", port))
	if err := fwCmd.Run(); err != nil {
		// Non-fatal: firewall rule may already exist or firewall may be disabled
		fmt.Printf("Warning: failed to add firewall rule: %v\n", err)
	}

	// Reload TightVNC service configuration so it picks up registry changes
	reloadCmd := exec.Command(`C:\Program Files\TightVNC\tvnserver.exe`,
		"-controlservice", "-reload")
	if err := reloadCmd.Run(); err != nil {
		// Non-fatal: service may not be running yet (will pick up on next start)
		fmt.Printf("Note: could not reload TightVNC config (will apply on next start): %v\n", err)
	}

	return nil
}

func (w *WindowsVNCManager) Start() error {
	// Start the TightVNC service
	cmd := exec.Command("sc", "start", "tvnserver")
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		// Already running is not an error
		if strings.Contains(outputStr, "1056") { // ERROR_SERVICE_ALREADY_RUNNING
			return nil
		}
		return fmt.Errorf("failed to start TightVNC service: %w (output: %s)", err, outputStr)
	}
	return nil
}

func (w *WindowsVNCManager) Stop() error {
	cmd := exec.Command("sc", "stop", "tvnserver")
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		// Not running is not an error
		if strings.Contains(outputStr, "1062") { // ERROR_SERVICE_NOT_ACTIVE
			return nil
		}
		return fmt.Errorf("failed to stop TightVNC service: %w (output: %s)", err, outputStr)
	}
	return nil
}

func (w *WindowsVNCManager) IsRunning() bool {
	cmd := exec.Command("sc", "query", "tvnserver")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "RUNNING")
}

func (w *WindowsVNCManager) Port() int {
	// Read port from registry (survives restarts, not in-memory)
	cmd := exec.Command("reg", "query",
		`HKLM\SOFTWARE\TightVNC\Server`,
		"/v", "RfbPort")
	output, err := cmd.Output()
	if err != nil {
		return DefaultVNCPort // Default if not configured
	}
	// Output format: "    RfbPort    REG_DWORD    0x1714"
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "RfbPort") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				valStr := fields[len(fields)-1]
				// Handle hex (0x...) or decimal
				if strings.HasPrefix(valStr, "0x") || strings.HasPrefix(valStr, "0X") {
					if val, err := strconv.ParseInt(valStr[2:], 16, 32); err == nil {
						return int(val)
					}
				}
				if val, err := strconv.Atoi(valStr); err == nil {
					return val
				}
			}
		}
	}
	return DefaultVNCPort
}

// ErrSudoRequired is returned when VNC installation needs elevated privileges.
// The caller (cmd/vnc.go) should display an actionable message to the user.
var ErrSudoRequired = fmt.Errorf("VNC server installation requires root privileges. Install sudo and run: sudo citadel vnc enable — or run directly as root: su -c 'citadel vnc enable'")

// --- Linux implementation ---

// LinuxVNCManager manages x11vnc on Linux.
type LinuxVNCManager struct {
	port int // configured port; 0 means use DefaultVNCPort
}

func (l *LinuxVNCManager) IsInstalled() bool {
	_, err := exec.LookPath("x11vnc")
	return err == nil
}

func (l *LinuxVNCManager) Install() error {
	if l.IsInstalled() {
		return nil
	}

	fmt.Println("Installing x11vnc...")

	needsSudo := os.Getuid() != 0

	// Check that sudo is available when needed
	if needsSudo {
		if _, err := exec.LookPath("sudo"); err != nil {
			return ErrSudoRequired
		}
	}

	sudoPrefix := func(name string, args ...string) *exec.Cmd {
		if needsSudo {
			return exec.Command("sudo", append([]string{name}, args...)...)
		}
		return exec.Command(name, args...)
	}

	if _, err := exec.LookPath("apt-get"); err == nil {
		updateCmd := sudoPrefix("apt-get", "update", "-qq")
		_ = updateCmd.Run()

		installCmd := sudoPrefix("apt-get", "install", "-y", "-qq", "x11vnc")
		installCmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		if err := installCmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via apt: %w", err)
		}
		return nil
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		cmd := sudoPrefix("dnf", "install", "-y", "x11vnc")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via dnf: %w", err)
		}
		return nil
	}
	if _, err := exec.LookPath("yum"); err == nil {
		cmd := sudoPrefix("yum", "install", "-y", "x11vnc")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via yum: %w", err)
		}
		return nil
	}
	if _, err := exec.LookPath("pacman"); err == nil {
		cmd := sudoPrefix("pacman", "-S", "--noconfirm", "x11vnc")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via pacman: %w", err)
		}
		return nil
	}
	if _, err := exec.LookPath("zypper"); err == nil {
		cmd := sudoPrefix("zypper", "install", "-y", "x11vnc")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via zypper: %w", err)
		}
		return nil
	}

	return fmt.Errorf("no supported package manager found (tried apt-get, dnf, yum, pacman, zypper)")
}

func (l *LinuxVNCManager) Uninstall() error {
	fmt.Println("VNC server uninstall not yet supported on Linux.")
	return nil
}

func (l *LinuxVNCManager) Configure(password string, port int) error {
	if err := ValidateVNCPort(port); err != nil {
		return err
	}
	l.port = port

	// Truncate password to 8 chars (VNC DES limit)
	if len(password) > 8 {
		password = password[:8]
	}

	// Ensure ~/.vnc directory exists
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	vncDir := filepath.Join(homeDir, ".vnc")
	if err := os.MkdirAll(vncDir, 0700); err != nil {
		return fmt.Errorf("failed to create .vnc directory: %w", err)
	}

	// Store password using x11vnc's password tool
	passwdFile := filepath.Join(vncDir, "passwd")
	cmd := exec.Command("x11vnc", "-storepasswd", password, passwdFile)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to store VNC password: %w", err)
	}

	// Restrict password file permissions
	if err := os.Chmod(passwdFile, 0600); err != nil {
		return fmt.Errorf("failed to set password file permissions: %w", err)
	}

	return nil
}

func (l *LinuxVNCManager) Start() error {
	if l.IsRunning() {
		return nil // Already running, idempotent
	}

	port := l.effectivePort()

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	passwdFile := filepath.Join(homeDir, ".vnc", "passwd")

	authFile, authNeedsSudo := l.resolveXAuth()
	needsSudo := authNeedsSudo && os.Getuid() != 0

	args := buildX11VNCArgs(passwdFile, port, authFile)

	var cmd *exec.Cmd
	if needsSudo {
		fmt.Println("Note: using display manager auth file, running x11vnc with sudo")
		cmd = exec.Command("sudo", append([]string{"x11vnc"}, args...)...)
	} else {
		cmd = exec.Command("x11vnc", args...)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start x11vnc: %w\noutput: %s", err, string(output))
	}

	return nil
}

// buildX11VNCArgs constructs the x11vnc command-line arguments.
// This is a pure function extracted for testability.
func buildX11VNCArgs(passwdFile string, port int, authFile string) []string {
	args := []string{
		"-rfbauth", passwdFile,
		"-rfbport", strconv.Itoa(port),
		"-forever",
		"-bg",
	}

	if authFile != "" {
		// An explicit auth file was found -- use it directly.
		// Also specify -display :0 since we know the target display.
		args = append([]string{"-display", ":0"}, args...)
		args = append(args, "-auth", authFile)
	} else {
		// No auth file found -- let x11vnc auto-discover the display
		// and auth cookie via -find, which probes /tmp/.X*-lock,
		// XAUTHORITY, and running X sessions without needing sudo.
		args = append([]string{"-find"}, args...)
	}

	return args
}

// resolveXAuth locates the X authority file and reports whether sudo
// is needed to read it. Returns ("", false) when no auth file is found,
// in which case Start() uses -find for auto-discovery.
//
// Priority order (first match wins):
//  1. $XAUTHORITY env var (user-readable, no sudo)
//  2. ~/.Xauthority (user-readable, no sudo)
//  3. Display-manager auth files (root-only, sudo required)
func (l *LinuxVNCManager) resolveXAuth() (authFile string, needsSudo bool) {
	// 1. Check XAUTHORITY environment variable
	if xauthEnv := os.Getenv("XAUTHORITY"); xauthEnv != "" {
		if _, err := os.Stat(xauthEnv); err == nil {
			return xauthEnv, false
		}
	}

	// 2. Check user's own ~/.Xauthority
	if home, err := os.UserHomeDir(); err == nil {
		xauth := filepath.Join(home, ".Xauthority")
		if _, err := os.Stat(xauth); err == nil {
			return xauth, false
		}
	}

	// 3. Display-manager auth files (all require root)

	// LightDM
	if exec.Command("pgrep", "-x", "lightdm").Run() == nil {
		ldmAuth := "/var/run/lightdm/root/:0"
		if _, err := os.Stat(ldmAuth); err == nil {
			return ldmAuth, true
		}
	}

	// GDM
	if exec.Command("pgrep", "-x", "gdm-session-wor").Run() == nil {
		gdmAuth := "/run/user/120/gdm/Xauthority"
		if _, err := os.Stat(gdmAuth); err == nil {
			return gdmAuth, true
		}
	}

	// SDDM
	if exec.Command("pgrep", "-x", "sddm").Run() == nil {
		sddmAuth := "/run/sddm/xauth"
		if _, err := os.Stat(sddmAuth); err == nil {
			return sddmAuth, true
		}
	}

	// No auth file found -- Start() will use -find for auto-discovery
	return "", false
}

func (l *LinuxVNCManager) Stop() error {
	cmd := exec.Command("pkill", "x11vnc")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Exit code 1 means no processes matched -- not an error
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("failed to stop x11vnc: %w (output: %s)", err, string(output))
	}
	return nil
}

func (l *LinuxVNCManager) IsRunning() bool {
	cmd := exec.Command("pgrep", "-x", "x11vnc")
	return cmd.Run() == nil
}

func (l *LinuxVNCManager) Port() int {
	port := l.effectivePort()
	// Verify the port is actually listening
	cmd := exec.Command("ss", "-tlnp")
	output, err := cmd.Output()
	if err != nil {
		return port
	}
	if strings.Contains(string(output), fmt.Sprintf(":%d ", port)) {
		return port
	}
	// Check common fallback ports
	for _, p := range []int{5900, 5901} {
		if strings.Contains(string(output), fmt.Sprintf(":%d ", p)) {
			return p
		}
	}
	return port
}

func (l *LinuxVNCManager) effectivePort() int {
	if l.port > 0 {
		return l.port
	}
	return DefaultVNCPort
}

// --- macOS implementation (stub) ---

// DarwinVNCManager is a stub VNC manager for macOS.
// macOS has built-in Screen Sharing that can be enabled via system preferences.
type DarwinVNCManager struct{}

func (d *DarwinVNCManager) IsInstalled() bool {
	// macOS has built-in Screen Sharing (VNC)
	return true
}

func (d *DarwinVNCManager) Install() error {
	fmt.Println("VNC server provisioning not yet supported on macOS (use built-in Screen Sharing)")
	return nil
}

func (d *DarwinVNCManager) Uninstall() error {
	fmt.Println("VNC server uninstall not yet supported on macOS.")
	return nil
}

func (d *DarwinVNCManager) Configure(password string, port int) error {
	fmt.Println("VNC server provisioning not yet supported on macOS (use built-in Screen Sharing)")
	return nil
}

func (d *DarwinVNCManager) Start() error {
	fmt.Println("VNC server provisioning not yet supported on macOS (use built-in Screen Sharing)")
	return nil
}

func (d *DarwinVNCManager) Stop() error {
	fmt.Println("VNC server provisioning not yet supported on macOS (use built-in Screen Sharing)")
	return nil
}

func (d *DarwinVNCManager) IsRunning() bool {
	// Check if Screen Sharing is active
	cmd := exec.Command("launchctl", "list", "com.apple.screensharing")
	return cmd.Run() == nil
}

func (d *DarwinVNCManager) Port() int {
	if d.IsRunning() {
		return DefaultVNCPort
	}
	return 0
}
