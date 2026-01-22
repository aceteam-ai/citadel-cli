// internal/platform/machineid.go
package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// GenerateMachineID returns a stable SHA-256 hash of hardware identifiers.
// The hash is computed from: machineUUID + ":" + macAddress + ":" + hostname
// This provides a stable fingerprint for device re-registration support.
func GenerateMachineID() (string, error) {
	machineUUID, err := getMachineUUID()
	if err != nil {
		// If we can't get machine UUID, use hostname as fallback
		machineUUID = ""
	}

	macAddr, err := getPrimaryMAC()
	if err != nil {
		macAddr = ""
	}

	hostname, _ := os.Hostname()

	// Ensure we have at least some identifying information
	if machineUUID == "" && macAddr == "" && hostname == "" {
		return "", fmt.Errorf("unable to gather any machine identifiers")
	}

	// Compute SHA-256 hash of combined identifiers
	data := machineUUID + ":" + macAddr + ":" + hostname
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:]), nil
}

// getMachineUUID reads the machine UUID from the operating system.
func getMachineUUID() (string, error) {
	switch runtime.GOOS {
	case "linux":
		return getLinuxMachineUUID()
	case "darwin":
		return getDarwinMachineUUID()
	case "windows":
		return getWindowsMachineUUID()
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// getLinuxMachineUUID reads machine ID from Linux.
// Primary: /etc/machine-id (no root required)
// Fallback: /sys/class/dmi/id/product_uuid (requires root)
func getLinuxMachineUUID() (string, error) {
	// Try /etc/machine-id first (most common, no root required)
	if data, err := os.ReadFile("/etc/machine-id"); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	}

	// Fallback to DMI product UUID (requires root)
	if data, err := os.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	}

	return "", fmt.Errorf("could not read machine ID from /etc/machine-id or /sys/class/dmi/id/product_uuid")
}

// getDarwinMachineUUID reads machine UUID from macOS using ioreg.
func getDarwinMachineUUID() (string, error) {
	cmd := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run ioreg: %w", err)
	}

	// Parse output for IOPlatformUUID
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "IOPlatformUUID") {
			// Line format: "IOPlatformUUID" = "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX"
			parts := strings.Split(line, "=")
			if len(parts) >= 2 {
				uuid := strings.TrimSpace(parts[1])
				uuid = strings.Trim(uuid, `"`)
				if uuid != "" {
					return uuid, nil
				}
			}
		}
	}

	return "", fmt.Errorf("IOPlatformUUID not found in ioreg output")
}

// getWindowsMachineUUID reads machine GUID from Windows Registry.
// Primary: Registry HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid
// Fallback: wmic csproduct get UUID
func getWindowsMachineUUID() (string, error) {
	// Try registry first via reg query
	cmd := exec.Command("reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid")
	output, err := cmd.Output()
	if err == nil {
		// Parse registry output - format:
		// HKEY_LOCAL_MACHINE\SOFTWARE\Microsoft\Cryptography
		//     MachineGuid    REG_SZ    xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, "MachineGuid") {
				fields := strings.Fields(line)
				if len(fields) >= 3 {
					guid := fields[len(fields)-1]
					guid = strings.TrimSpace(guid)
					if guid != "" {
						return guid, nil
					}
				}
			}
		}
	}

	// Fallback to WMIC
	cmd = exec.Command("wmic", "csproduct", "get", "UUID")
	output, err = cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get machine GUID: %w", err)
	}

	// Parse WMIC output - first line is header "UUID", second line is the value
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip header and empty lines
		if line == "" || strings.EqualFold(line, "UUID") {
			continue
		}
		if line != "" {
			return line, nil
		}
	}

	return "", fmt.Errorf("could not read machine GUID from registry or WMIC")
}

// getPrimaryMAC returns the MAC address of the first non-loopback network interface.
func getPrimaryMAC() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("failed to get network interfaces: %w", err)
	}

	for _, iface := range interfaces {
		// Skip loopback and interfaces without hardware address
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		// Skip virtual interfaces (common patterns)
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "virbr") {
			continue
		}
		return iface.HardwareAddr.String(), nil
	}

	return "", fmt.Errorf("no suitable network interface found")
}
