//go:build linux

package service

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

var workerManagerEffectiveUID = os.Geteuid
var lookupWorkerManagerUser = user.LookupId

// A bus address alone does not let root impersonate a logged-in user. Drop to
// the pinned manager account for both discovery (including cat) and restart.
// Never carry root's supplementary groups or working directory into that child.
func applyWorkerManagerIdentity(command *exec.Cmd, userMode bool, managerUID, effectiveUID int) error {
	command.Env = discoveryManagerEnvironment(userMode, managerUID)
	if !userMode || managerUID == effectiveUID {
		return nil
	}
	if effectiveUID != 0 || managerUID <= 0 {
		return errors.New("user manager account could not be verified")
	}
	account, err := lookupWorkerManagerUser(strconv.Itoa(managerUID))
	if err != nil || account == nil || account.Uid != strconv.Itoa(managerUID) {
		return errors.New("user manager account could not be verified")
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil || strconv.FormatUint(gid, 10) != account.Gid || !filepath.IsAbs(account.HomeDir) {
		return errors.New("user manager account could not be verified")
	}
	if uint64(managerUID) > uint64(^uint32(0)) {
		return errors.New("user manager account could not be verified")
	}
	command.Dir = account.HomeDir
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(managerUID), Gid: uint32(gid),
		// nil Groups with NoSetGroups=false requests setgroups(0, NULL).
	}}
	return nil
}
