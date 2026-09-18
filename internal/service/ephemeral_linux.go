//go:build linux

package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var durableSystemExecPath = "/usr/local/bin/citadel"
var userHomeDirForExec = os.UserHomeDir

const tmpfsMagic = 0x01021994

// ephemeralExecPath identifies paths that cannot safely be baked into a systemd ExecStart.
func ephemeralExecPath(path string) (bool, string) {
	clean := filepath.Clean(path)
	for _, root := range []string{"/tmp", "/var/tmp", "/dev/shm"} {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true, root
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(clean, &st); err == nil && uint64(st.Type) == tmpfsMagic {
		return true, "tmpfs"
	}
	return false, ""
}

func materializeEphemeralExec(cfg ServiceConfig) (ServiceConfig, error) {
	ephemeral, _ := ephemeralExecPath(cfg.ExecPath)
	if !ephemeral {
		return cfg, nil
	}
	destination := durableSystemExecPath
	if cfg.UserMode {
		home, err := userHomeDirForExec()
		if err != nil {
			return ServiceConfig{}, fmt.Errorf("determine home for durable user executable: %w", err)
		}
		destination = filepath.Join(home, ".local", "bin", "citadel")
	}
	if err := copyExecutable(cfg.ExecPath, destination); err != nil {
		return ServiceConfig{}, fmt.Errorf("copy ephemeral executable from %s to %s: %w", cfg.ExecPath, destination, err)
	}
	cfg.ExecPath = destination
	return cfg, nil
}

func copyExecutable(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".citadel-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm() | 0o111); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, destination)
}

// EphemeralManagedExecStarts returns unsafe ExecStart paths in Citadel-owned units.
func EphemeralManagedExecStarts() []string {
	var warnings []string
	for _, candidate := range candidateManagedUnits() {
		content, err := os.ReadFile(candidate.path)
		if err != nil || !isCitadelManagedUnit(string(content)) {
			continue
		}
		if path, location, ok := ephemeralExecStart(string(content)); ok {
			warnings = append(warnings, ephemeralExecWarning(candidate, path, location))
		}
	}
	return warnings
}

func ephemeralExecWarning(candidate managedUnitCandidate, path, location string) string {
	command := "citadel service install"
	if !candidate.userMode {
		command = "sudo citadel service install --system"
	}
	return fmt.Sprintf("%s runs %s from ephemeral %s; reinstall with `%s` before reboot", candidate.path, path, location, command)
}

func ephemeralExecStart(content string) (path, location string, ok bool) {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		if len(fields) == 0 {
			continue
		}
		candidate := strings.TrimLeft(fields[0], "-+!@:")
		if unsafe, why := ephemeralExecPath(candidate); unsafe {
			return candidate, why, true
		}
	}
	return "", "", false
}
