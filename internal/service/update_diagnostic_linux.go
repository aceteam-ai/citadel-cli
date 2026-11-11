//go:build linux

package service

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// DiagnoseWorkerUpdate reads metadata only. In particular, a sudo invocation
// must not test permissions by creating a file as root and attribute that
// result to the managed unit's non-root account.
func DiagnoseWorkerUpdate(worker ManagedWorker) WorkerUpdateDiagnostic {
	lookup := func(name string) (*user.User, error) {
		if _, err := strconv.ParseUint(name, 10, 32); err == nil {
			return user.LookupId(name)
		}
		return user.Lookup(name)
	}
	return diagnoseWorkerUpdate(worker, os.Stat, lookup, func(account *user.User) ([]string, error) { return account.GroupIds() })
}

func diagnoseWorkerUpdate(worker ManagedWorker, stat func(string) (os.FileInfo, error), lookup func(string) (*user.User, error), groups func(*user.User) ([]string, error)) (d WorkerUpdateDiagnostic) {
	d = WorkerUpdateDiagnostic{Unit: worker.Unit.Description(), WorkerUser: worker.User, Directory: filepath.Dir(worker.Executable), Warning: true, Detail: "update permissions could not be verified; inspect the worker account and its install directory"}
	defer func() {
		if worker.AutoUpdateDisabled {
			d.AutoUpdateDisabled = true
			d.Warning = false
			d.Detail = "automatic updates deliberately disabled; " + d.Detail
		}
	}()
	if worker.User == "" || worker.Executable == "" {
		return d
	}
	account, err := lookup(worker.User)
	if err != nil {
		return d
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return d
	}
	info, err := stat(worker.Executable)
	if err != nil || !info.Mode().IsRegular() {
		return d
	}
	binary, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return d
	}
	d.BinaryOwner = "uid " + strconv.FormatUint(uint64(binary.Uid), 10)
	d.WorkerUser = fmt.Sprintf("%s (uid %d)", worker.User, uid)
	if uid != uint64(binary.Uid) {
		d.Detail = "worker and binary owners differ; first update-lock creation requires the binary owner; keep updating with its privileged installer, or explicitly configure a self-updating install owned by the worker"
		return d
	}
	directory, err := stat(d.Directory)
	if err != nil || !directory.IsDir() {
		return d
	}
	owner, ok := directory.Sys().(*syscall.Stat_t)
	if !ok {
		return d
	}
	mode := directory.Mode().Perm()
	permission := mode & 7
	if uid == uint64(owner.Uid) {
		permission = mode >> 6 & 7
	} else {
		ids, err := groups(account)
		if err != nil {
			return d
		}
		for _, id := range ids {
			if id == strconv.FormatUint(uint64(owner.Gid), 10) {
				permission = mode >> 3 & 7
				break
			}
		}
	}
	if permission&3 != 3 && uid != 0 {
		d.Detail = "directory mode bits do not grant the configured worker write and search access; inspect ACLs and the service sandbox; keep updating manually, or explicitly configure a worker-owned self-updating install"
		return d
	}
	d.Warning = false
	d.Detail = "ownership and directory mode checks found no obstacle; effective ACL, mount and service-sandbox permissions remain unverified until the worker's own preflight"
	return d
}
