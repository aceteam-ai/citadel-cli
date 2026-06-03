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

// VNCManager interface defines operations for VNC server management.
type VNCManager interface {
	IsInstalled() bool
	Install() error
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

	// Detect package manager and install
	if _, err := exec.LookPath("apt-get"); err == nil {
		updateCmd := exec.Command("apt-get", "update", "-qq")
		_ = updateCmd.Run() // best-effort; install may succeed from cache

		installCmd := exec.Command("apt-get", "install", "-y", "-qq", "x11vnc")
		installCmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		if err := installCmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via apt: %w", err)
		}
		return nil
	}
	if _, err := exec.LookPath("yum"); err == nil {
		cmd := exec.Command("yum", "install", "-y", "x11vnc")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via yum: %w", err)
		}
		return nil
	}
	if _, err := exec.LookPath("pacman"); err == nil {
		cmd := exec.Command("pacman", "-S", "--noconfirm", "x11vnc")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install x11vnc via pacman: %w", err)
		}
		return nil
	}

	return fmt.Errorf("no supported package manager found (tried apt-get, yum, pacman)")
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

	// Write VNC passwd file directly using DES encryption (avoids leaking
	// password in /proc/PID/cmdline via x11vnc -storepasswd)
	passwdFile := filepath.Join(vncDir, "passwd")
	encrypted, err := EncryptVNCPassword(password)
	if err != nil {
		return fmt.Errorf("failed to encrypt VNC password: %w", err)
	}
	if err := os.WriteFile(passwdFile, encrypted, 0600); err != nil {
		return fmt.Errorf("failed to write VNC password file: %w", err)
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

	// Start x11vnc in the background
	cmd := exec.Command("x11vnc",
		"-display", ":0",
		"-auth", "guess",
		"-rfbauth", passwdFile,
		"-rfbport", strconv.Itoa(port),
		"-forever",
		"-bg",
	)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start x11vnc: %w", err)
	}

	return nil
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
