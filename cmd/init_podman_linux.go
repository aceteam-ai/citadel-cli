//go:build linux

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const podmanDelegateUnit = "[Service]\nDelegate=cpu memory pids\n"

func podmanProvisionUser() (string, string, string, error) {
	name := platform.GetSudoUser()
	if name == "" || name == "root" {
		return "", "", "", fmt.Errorf("rootless Podman provisioning requires sudo from the node's non-root owner")
	}
	owner, err := user.Lookup(name)
	if err != nil {
		return "", "", "", fmt.Errorf("lookup node owner: %w", err)
	}
	return name, owner.Uid, owner.HomeDir, nil
}

func provisionCommand(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runAsPodmanUser(name, uid, home string, args ...string) error {
	prefix := []string{"-u", name, "--", "env", "-u", "SUDO_USER", "-u", "SUDO_UID", "-u", "SUDO_GID",
		"HOME=" + home, "XDG_RUNTIME_DIR=/run/user/" + uid,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus"}
	return provisionCommand("runuser", append(prefix, args...)...)
}

// nextSubIDRange picks a fresh 65,536-ID block beyond every existing UID and
// GID mapping. Already mapped users are never rewritten during reprovision.
func nextSubIDRange(uidMap, gidMap []byte) int64 {
	end := int64(100000)
	for _, content := range [][]byte{uidMap, gidMap} {
		for _, line := range strings.Split(string(content), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) != 3 {
				continue
			}
			start, startErr := strconv.ParseInt(fields[1], 10, 64)
			count, countErr := strconv.ParseInt(fields[2], 10, 64)
			if startErr == nil && countErr == nil && start >= 0 && count > 0 && start+count > end {
				end = start + count
			}
		}
	}
	return ((end + 65535) / 65536) * 65536
}

func hasSubIDRange(data []byte, owner string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 3 && fields[0] == owner {
			count, err := strconv.ParseInt(fields[2], 10, 64)
			if err == nil && count >= 65536 {
				return true
			}
		}
	}
	return false
}

func ensurePodmanSubIDs(name string) error {
	uidMap, err := os.ReadFile("/etc/subuid")
	if err != nil {
		return fmt.Errorf("read subordinate UIDs: %w", err)
	}
	gidMap, err := os.ReadFile("/etc/subgid")
	if err != nil {
		return fmt.Errorf("read subordinate GIDs: %w", err)
	}
	if !hasSubIDRange(uidMap, name) {
		start := nextSubIDRange(uidMap, gidMap)
		if err := provisionCommand("usermod", "--add-subuids", fmt.Sprintf("%d-%d", start, start+65535), name); err != nil {
			return err
		}
		uidMap = append(uidMap, []byte(fmt.Sprintf("%s:%d:65536\n", name, start))...)
	}
	if !hasSubIDRange(gidMap, name) {
		start := nextSubIDRange(uidMap, gidMap)
		if err := provisionCommand("usermod", "--add-subgids", fmt.Sprintf("%d-%d", start, start+65535), name); err != nil {
			return err
		}
	}
	return nil
}

func installRootlessPodmanLinux() error {
	name, uid, home, err := podmanProvisionUser()
	if err != nil {
		return err
	}
	pm, err := platform.GetPackageManager()
	if err != nil {
		return err
	}
	if err := pm.Update(); err != nil {
		return err
	}
	if err := pm.Install("podman", "podman-compose", "uidmap", "fuse-overlayfs", "crun", "slirp4netns", "dbus-user-session", "git", "gnupg"); err != nil {
		return fmt.Errorf("install rootless Podman dependencies: %w", err)
	}
	if err := provisionCommand("apt-cache", "show", "passt"); err == nil {
		if err := pm.Install("passt"); err != nil {
			return fmt.Errorf("install pasta networking: %w", err)
		}
	}
	if err := ensurePodmanSubIDs(name); err != nil {
		return err
	}
	dropin := "/etc/systemd/system/user@.service.d/50-citadel-delegate.conf"
	if err := os.MkdirAll(filepath.Dir(dropin), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(dropin, []byte(podmanDelegateUnit), 0644); err != nil {
		return err
	}
	for _, command := range [][]string{
		{"systemctl", "daemon-reload"},
		{"loginctl", "enable-linger", name},
		{"systemctl", "start", "user@" + uid + ".service"},
	} {
		if err := provisionCommand(command[0], command[1:]...); err != nil {
			return err
		}
	}
	if err := runAsPodmanUser(name, uid, home, "systemctl", "--user", "enable", "--now", "podman.socket"); err != nil {
		return err
	}
	if err := runAsPodmanUser(name, uid, home, "podman", "info"); err != nil {
		return err
	}
	if binary, err := os.Executable(); err == nil {
		if err := runAsPodmanUser(name, uid, home, binary, "service", "catalog", "update"); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Trusted catalog could not be updated for app runtime pre-pull: %v\n", err)
		} else if err := runAsPodmanUser(name, uid, home, binary, "service", "catalog", "pre-pull-runtimes"); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Trusted app runtime pre-pull is pending: %v\n", err)
		}
	}
	return nil
}

func configureNvidiaCDILinux() error {
	if _, err := exec.LookPath("nvidia-ctk"); err != nil {
		// No toolkit on this CPU-only node.
		return nil
	}
	name, _, _, err := podmanProvisionUser()
	if err != nil {
		return err
	}
	if err := provisionCommand("usermod", "-aG", "video,render", name); err != nil {
		return err
	}
	rule := "KERNEL==\"nvidia[0-9]*\", GROUP=\"video\", MODE=\"0660\"\n" +
		"KERNEL==\"nvidiactl\", GROUP=\"video\", MODE=\"0660\"\n" +
		"KERNEL==\"nvidia-uvm*\", GROUP=\"video\", MODE=\"0660\"\n" +
		"KERNEL==\"nvidia-cap*\", GROUP=\"video\", MODE=\"0660\"\n"
	if err := os.WriteFile("/etc/udev/rules.d/70-citadel-nvidia.rules", []byte(rule), 0644); err != nil {
		return err
	}
	if err := provisionCommand("udevadm", "control", "--reload-rules"); err != nil {
		return err
	}
	if err := os.MkdirAll("/etc/cdi", 0755); err != nil {
		return err
	}
	const refreshUnit = "[Unit]\nDescription=Refresh Citadel NVIDIA CDI devices after driver initialization\n" +
		"After=systemd-udev-settle.service\nConditionPathExists=/dev/nvidiactl\n\n" +
		"[Service]\nType=oneshot\nExecStart=/usr/bin/nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile("/etc/systemd/system/citadel-nvidia-cdi.service", []byte(refreshUnit), 0644); err != nil {
		return err
	}
	if err := provisionCommand("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := provisionCommand("systemctl", "enable", "citadel-nvidia-cdi.service"); err != nil {
		return err
	}
	if err := provisionCommand("nvidia-smi"); err != nil {
		// The driver may not be active until reboot; the unit generates CDI then.
		return nil
	}
	if err := provisionCommand("nvidia-ctk", "cdi", "generate", "--output=/etc/cdi/nvidia.yaml"); err != nil {
		return err
	}
	return provisionCommand("nvidia-ctk", "cdi", "list")
}

func finishProvisionedLinuxUserWorker() bool {
	if !initProvision || !platform.IsRoot() {
		return false
	}
	name, uid, home, err := podmanProvisionUser()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not install rootless worker: %v\n", err)
		return true
	}
	if _, err := os.Stat("/etc/systemd/system/citadel-worker.service"); err == nil {
		fmt.Fprintln(os.Stderr, "⚠️  Legacy system worker exists; migrate its state before installing a rootless user worker.")
		return true
	}
	if _, err := os.Stat("/etc/systemd/user/citadel-worker.service"); err == nil {
		err = runAsPodmanUser(name, uid, home, "systemctl", "--user", "enable", "--now", "citadel-worker.service")
	} else {
		binary, binaryErr := os.Executable()
		if binaryErr != nil {
			err = binaryErr
		} else {
			err = runAsPodmanUser(name, uid, home, binary, "service", "install")
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not start rootless user worker: %v\n", err)
		fmt.Printf("   Run as %s: citadel service install\n", name)
	} else {
		fmt.Printf("\n✅ Rootless Citadel user worker started for %s.\n", name)
	}
	return true
}
