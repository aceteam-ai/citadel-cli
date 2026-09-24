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

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const podmanDelegateUnit = "[Service]\nDelegate=cpu memory pids\n"

const nvidiaCDIRefreshScript = `#!/bin/sh
set -eu
for attempt in $(seq 1 30); do
    if nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml &&
       nvidia-ctk cdi list | grep -F 'nvidia.com/gpu' >/dev/null; then
        exit 0
    fi
    sleep 2
done
echo 'NVIDIA CDI devices unavailable after readiness retries' >&2
exit 1
`

func isJetsonAt(release string, modelPaths []string, read func(string) ([]byte, error)) bool {
	if data, err := read(release); err == nil && strings.Contains(string(data), "R") {
		return true
	}
	for _, path := range modelPaths {
		if data, err := read(path); err == nil && (strings.Contains(strings.ToLower(string(data)), "jetson") || strings.Contains(strings.ToLower(string(data)), "tegra")) {
			return true
		}
	}
	return false
}

func isJetsonLinux() bool {
	return isJetsonAt("/etc/nv_tegra_release", []string{"/proc/device-tree/model", "/sys/firmware/devicetree/base/model"}, os.ReadFile)
}

func hasNvidiaHardwareLinux() bool {
	if isJetsonLinux() {
		return true
	}
	devices, err := filepath.Glob("/sys/bus/pci/devices/*/vendor")
	if err == nil {
		for _, path := range devices {
			if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == "0x10de" {
				return true
			}
		}
	}
	if entries, err := os.ReadDir("/proc/driver/nvidia/gpus"); err == nil && len(entries) > 0 {
		return true
	}
	return false
}

var legacySystemWorkerUnits = []string{
	"/etc/systemd/system/citadel-worker.service",
	"/etc/systemd/system/citadel.service",
}

// E5 handles migration. This gate is deliberately read-only and runs before
// node identity, enrollment, logs or account creation can mutate the host.
func rejectLegacySystemWorkerAt(paths []string, lstat func(string) (os.FileInfo, error)) error {
	for _, path := range paths {
		if _, err := lstat(path); err == nil {
			return fmt.Errorf("existing system worker %s requires E5 migration; refusing fresh rootless provisioning", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("cannot verify legacy worker %s: %w", path, err)
		}
	}
	return nil
}

func prepareLinuxPodmanProvision() error {
	if err := rejectLegacySystemWorkerAt(legacySystemWorkerUnits, os.Lstat); err != nil {
		return err
	}
	if !platform.IsRoot() {
		return nil // existing privilege diagnostic runs next
	}
	// All state resolvers in the enrollment flow must see the worker's home,
	// not the human sudo caller's or root's home. Refuse a pre-existing foreign
	// state pointer rather than silently moving its machine key.
	root := "/home/citadel"
	want := filepath.Join(root, "citadel-node")
	if got := network.GetNodeConfigDir(); got != want {
		if entries, err := os.ReadDir(got); err == nil && len(entries) > 0 {
			return fmt.Errorf("existing node state at %s requires explicit E5 migration before provisioning %s", got, want)
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect existing node state %s: %w", got, err)
		}
	}
	if err := ensureDedicatedPodmanUser(); err != nil {
		return err
	}
	if err := os.Setenv("SUDO_USER", podmanServiceUser); err != nil {
		return err
	}
	return os.Setenv("HOME", root)
}

func ensureDedicatedPodmanUser() error {
	owner, err := user.Lookup(podmanServiceUser)
	if err != nil {
		if err := provisionCommand("useradd", "--create-home", "--shell", "/bin/bash", podmanServiceUser); err != nil {
			return err
		}
		owner, err = user.Lookup(podmanServiceUser)
	}
	if err != nil {
		return fmt.Errorf("lookup dedicated worker user: %w", err)
	}
	if owner.Uid == "0" || owner.HomeDir != "/home/citadel" {
		return fmt.Errorf("dedicated %s account must be unprivileged with home /home/citadel", podmanServiceUser)
	}
	groups, err := exec.Command("id", "-nG", podmanServiceUser).Output()
	if err != nil {
		return fmt.Errorf("inspect dedicated worker groups: %w", err)
	}
	for _, group := range strings.Fields(string(groups)) {
		if group == "sudo" || group == "wheel" || group == "docker" {
			return fmt.Errorf("dedicated %s account belongs to privileged group %s; remove that membership before provisioning", podmanServiceUser, group)
		}
	}
	if _, err := os.Lstat("/etc/sudoers.d/99-citadel-" + podmanServiceUser); err == nil {
		return fmt.Errorf("dedicated %s account has a legacy passwordless sudo grant; remove it before provisioning", podmanServiceUser)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := exec.Command("runuser", "-u", podmanServiceUser, "--", "sudo", "-n", "true").Run(); err == nil {
		return fmt.Errorf("dedicated %s account has passwordless sudo; remove that grant before provisioning", podmanServiceUser)
	}
	return nil
}

func ownProvisionedLinuxState(nodeDir string) error {
	for _, path := range []string{nodeDir, filepath.Join("/home/citadel", ".citadel-cli")} {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := platform.ChownR(path, podmanServiceUser); err != nil {
			return err
		}
	}
	return nil
}

func podmanProvisionUser() (string, string, string, error) {
	name := podmanServiceUser
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
	} {
		if err := provisionCommand(command[0], command[1:]...); err != nil {
			return err
		}
	}
	if err := refreshPodmanUserManager(name, uid, home, nil); err != nil {
		return err
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

func requirePodmanDelegation(uid string) error {
	path := filepath.Join("/sys/fs/cgroup/user.slice", "user-"+uid+".slice", "user@"+uid+".service", "cgroup.controllers")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read active user manager controllers %s: %w", path, err)
	}
	return checkRequiredWords("delegated controllers", string(data), []string{"cpu", "memory", "pids"})
}

func checkRequiredWords(label, value string, required []string) error {
	have := make(map[string]bool)
	for _, word := range strings.Fields(value) {
		have[word] = true
	}
	for _, word := range required {
		if !have[word] {
			return fmt.Errorf("%s missing %s (got %q)", label, word, strings.TrimSpace(value))
		}
	}
	return nil
}

func requireManagerGroups(uid string, required []string) error {
	out, err := exec.Command("systemctl", "show", "user@"+uid+".service", "-p", "MainPID", "--value").Output()
	if err != nil {
		return fmt.Errorf("inspect active user manager PID: %w", err)
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" || pid == "0" {
		return fmt.Errorf("dedicated user manager is not active")
	}
	status, err := os.ReadFile(filepath.Join("/proc", pid, "status"))
	if err != nil {
		return fmt.Errorf("read active user manager groups: %w", err)
	}
	var active string
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Groups:") {
			active = strings.TrimPrefix(line, "Groups:")
			break
		}
	}
	var gids []string
	for _, group := range required {
		g, err := user.LookupGroup(group)
		if err != nil {
			return fmt.Errorf("required GPU group %s unavailable: %w", group, err)
		}
		gids = append(gids, g.Gid)
	}
	return checkRequiredWords("active user manager supplementary groups", active, gids)
}

func refreshPodmanUserManager(name, uid, home string, groups []string) error {
	unit := "user@" + uid + ".service"
	if err := provisionCommand("systemctl", "is-active", "--quiet", unit); err == nil {
		for _, worker := range []string{"citadel-worker.service", "citadel.service"} {
			if err := runAsPodmanUser(name, uid, home, "systemctl", "--user", "is-active", "--quiet", worker); err == nil {
				return fmt.Errorf("active %s worker %s must be stopped before refreshing user manager; refusing to disrupt it", name, worker)
			}
		}
		if err := provisionCommand("systemctl", "restart", unit); err != nil {
			return err
		}
	} else if err := provisionCommand("systemctl", "start", unit); err != nil {
		return err
	}
	if err := requirePodmanDelegation(uid); err != nil {
		return err
	}
	if len(groups) > 0 {
		return requireManagerGroups(uid, groups)
	}
	return nil
}

func configureNvidiaCDILinux() error {
	if !hasNvidiaHardwareLinux() {
		return nil
	}
	if _, err := exec.LookPath("nvidia-ctk"); err != nil {
		return fmt.Errorf("NVIDIA GPU present but nvidia-ctk unavailable: %w", err)
	}
	name, uid, home, err := podmanProvisionUser()
	if err != nil {
		return err
	}
	for _, group := range []string{"video", "render"} {
		if _, err := user.LookupGroup(group); err != nil {
			return fmt.Errorf("required GPU group %s unavailable: %w", group, err)
		}
	}
	if err := provisionCommand("usermod", "-aG", "video,render", name); err != nil {
		return err
	}
	if err := refreshPodmanUserManager(name, uid, home, []string{"video", "render"}); err != nil {
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
	if err := os.MkdirAll("/usr/local/libexec", 0755); err != nil {
		return err
	}
	if err := os.WriteFile("/usr/local/libexec/citadel-nvidia-cdi-refresh", []byte(nvidiaCDIRefreshScript), 0755); err != nil {
		return err
	}
	const refreshUnit = "[Unit]\nDescription=Refresh Citadel NVIDIA CDI devices after driver initialization\n" +
		"After=systemd-udev-settle.service\n\n" +
		"[Service]\nType=oneshot\nExecStart=/usr/local/libexec/citadel-nvidia-cdi-refresh\n\n" +
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
	if err := provisionCommand("nvidia-ctk", "cdi", "generate", "--output=/etc/cdi/nvidia.yaml"); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  NVIDIA CDI generation pending driver readiness; boot unit will retry: %v\n", err)
		return nil
	}
	out, err := exec.Command("nvidia-ctk", "cdi", "list").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "nvidia.com/gpu") {
		fmt.Fprintf(os.Stderr, "⚠️  NVIDIA CDI devices not yet listed; boot unit will retry: %s %v\n", strings.TrimSpace(string(out)), err)
	}
	return nil
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
	if err := rejectLegacySystemWorkerAt(legacySystemWorkerUnits, os.Lstat); err != nil {
		fmt.Fprintln(os.Stderr, "⚠️  Legacy system worker exists; migrate its state before installing a rootless user worker.")
		return true
	}
	if err := ownProvisionedLinuxState(filepath.Join(home, "citadel-node")); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not hand node state to rootless worker: %v\n", err)
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
