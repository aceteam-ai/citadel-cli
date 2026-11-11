//go:build linux

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const configName = "config.yml"

// openPrivateConfigDir anchors all operations to the config directory. This is
// deliberately Linux-only: nvrconfig is a Linux init container, not a host-side
// repair command. Refuse symlinks and foreign-owned mounts rather than chmodding
// arbitrary targets or loosening permissions for a mismatched container user.
func openPrivateConfigDir(path string) (*os.File, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open config directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, error) {
		dir.Close()
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fail(fmt.Errorf("stat config directory: %w", err))
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fail(fmt.Errorf("config directory %q must belong to init-container uid %d; chown the host bind source ~/.citadel-cli/nvr/config to the matching mapped uid, not broader modes", path, os.Geteuid()))
	}
	if err := dir.Chmod(0o700); err != nil {
		return fail(fmt.Errorf("restrict config directory: %w", err))
	}
	// Frigate prefers config.yaml when it exists. Harden both names even when
	// later input validation prevents writing the module's config.yml.
	for _, name := range []string{configName, "config.yaml"} {
		if err := hardenExistingConfig(dir, name); err != nil {
			return fail(err)
		}
	}
	return dir, nil
}

func hardenExistingConfig(dir *os.File, name string) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open existing config: %w", err)
	}
	f := os.NewFile(uintptr(fd), "existing nvr config")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat existing config: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return errors.New("existing config must be a regular, singly linked file")
	}
	if st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("config %q in %q must belong to init-container uid %d; chown its host file under ~/.citadel-cli/nvr/config to the matching mapped uid, not broader modes", name, dir.Name(), os.Geteuid())
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict existing config: %w", err)
	}
	return nil
}

// writePrivateConfig never truncates the old config. The temporary file is on
// the same mount, private from creation, synced and closed before atomic rename.
// Replacing the inode also fixes old 0644 modes that WriteFile would preserve.
func writePrivateConfig(dir *os.File, data []byte) error {
	name := ".config.yml-" + rand.Text()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create private config: %w", err)
	}
	f := os.NewFile(uintptr(fd), "new nvr config")
	defer f.Close()
	defer unix.Unlinkat(int(dir.Fd()), name, 0)
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict new config: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write private config: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync private config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close private config: %w", err)
	}
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), configName); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync config directory: %w", err)
	}
	return nil
}
