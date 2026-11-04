//go:build linux

package procscope

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func nativeOps() scopeOps {
	return scopeOps{
		uid:     nativeUID,
		self:    os.Getpid,
		process: observeNativeProcess,
		members: nativeMembers,
		connect: connectUserScopeBus,
	}
}

func nativeUID() uint32 {
	if os.Getuid() != os.Geteuid() || os.Getuid() <= 0 {
		return 0
	}
	return uint32(os.Getuid())
}

func ObserveProcess(pid int) (ProcessIdentity, error) {
	process, _, err := observeNativeProcess(pid)
	return process, err
}

func CurrentControlGroup() (string, error) {
	_, group, err := observeNativeProcess(os.Getpid())
	return group, err
}

func readObservation(name string) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 64*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > 64*1024 {
		return nil, ErrUnknown
	}
	return data, nil
}

func parseProcessStat(data []byte, expectedPID int) (uint64, error) {
	text := string(data)
	open, close := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if open < 1 || close <= open {
		return 0, ErrUnknown
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	fields := strings.Fields(text[close+1:])
	if err != nil || pid != expectedPID || len(fields) < 20 {
		return 0, ErrUnknown
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, ErrUnknown
	}
	return start, nil
}

func parseProcessUID(data []byte) (uint32, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) != 4 {
			return 0, ErrUnknown
		}
		uid, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil || uid == 0 {
			return 0, ErrUnknown
		}
		for _, value := range fields[1:] {
			parsed, err := strconv.ParseUint(value, 10, 32)
			if err != nil || parsed != uid {
				return 0, ErrUnknown
			}
		}
		return uint32(uid), nil
	}
	return 0, ErrUnknown
}

// This first implementation proves unified cgroup-v2 membership only. Other
// hierarchies refuse explicitly, rather than inheriting tmux's fallback.
func parseUnifiedCgroup(data []byte) (string, error) {
	var group string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" || group != "" || !validCgroup(parts[2]) {
			return "", ErrIsolation
		}
		group = parts[2]
	}
	if group == "" {
		return "", ErrIsolation
	}
	return group, nil
}

func observeNativeProcess(pid int) (ProcessIdentity, string, error) {
	if pid <= 0 {
		return ProcessIdentity{}, "", ErrUnknown
	}
	base := filepath.Join("/proc", strconv.Itoa(pid))
	first, err := readObservation(filepath.Join(base, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return ProcessIdentity{}, "", ErrProcessGone
	}
	if err != nil {
		return ProcessIdentity{}, "", ErrUnknown
	}
	start, err := parseProcessStat(first, pid)
	if err != nil {
		return ProcessIdentity{}, "", err
	}
	status, err := readObservation(filepath.Join(base, "status"))
	if err != nil {
		return ProcessIdentity{}, "", ErrUnknown
	}
	uid, err := parseProcessUID(status)
	if err != nil {
		return ProcessIdentity{}, "", err
	}
	cgroups, err := readObservation(filepath.Join(base, "cgroup"))
	if err != nil {
		return ProcessIdentity{}, "", ErrUnknown
	}
	group, err := parseUnifiedCgroup(cgroups)
	if err != nil {
		return ProcessIdentity{}, "", err
	}
	boot, err := readObservation("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ProcessIdentity{}, "", ErrUnknown
	}
	bootID := strings.TrimSpace(string(boot))
	if len(bootID) != 36 {
		return ProcessIdentity{}, "", ErrUnknown
	}
	if _, err := invocationBytes(strings.ReplaceAll(bootID, "-", "")); err != nil {
		return ProcessIdentity{}, "", ErrUnknown
	}
	last, err := readObservation(filepath.Join(base, "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return ProcessIdentity{}, "", ErrProcessGone
	}
	if err != nil {
		return ProcessIdentity{}, "", ErrUnknown
	}
	after, err := parseProcessStat(last, pid)
	if err != nil || after != start {
		return ProcessIdentity{}, "", ErrUnknown
	}
	return ProcessIdentity{PID: pid, StartTicks: start, UID: uid, BootID: bootID}, group, nil
}

func nativeMembers(ctx context.Context, group string) ([]ProcessIdentity, error) {
	if !validCgroup(group) {
		return nil, ErrUnknown
	}
	const mount = "/sys/fs/cgroup"
	var stat unix.Statfs_t
	if unix.Statfs(mount, &stat) != nil || stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, ErrIsolation
	}
	root := filepath.Join(mount, strings.TrimPrefix(group, "/"))
	seen := make(map[int]ProcessIdentity)
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return ErrUnknown
		}
		if walkErr != nil {
			// The exact old cgroup may disappear after a confirmed stop job.
			if name == root && errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return ErrUnknown
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnknown
		}
		if !entry.IsDir() {
			return nil
		}
		data, err := readObservation(filepath.Join(name, "cgroup.procs"))
		if err != nil {
			return ErrUnknown
		}
		for _, value := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return ErrUnknown
			}
			process, actualGroup, err := observeNativeProcess(pid)
			if errors.Is(err, ErrProcessGone) {
				continue
			}
			if err != nil || !within(actualGroup, group) || process.UID != nativeUID() {
				return ErrUnknown
			}
			if prior, found := seen[pid]; found && !sameProcess(prior, process) {
				return ErrUnknown
			}
			seen[pid] = process
		}
		return nil
	})
	if err != nil {
		return nil, ErrUnknown
	}
	result := make([]ProcessIdentity, 0, len(seen))
	for _, process := range seen {
		result = append(result, process)
	}
	return result, nil
}
