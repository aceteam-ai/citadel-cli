//go:build windows

package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	updateLockWait     = 25 * time.Millisecond
	updateLockDeadline = 10 * time.Second
)

type installedIdentity struct {
	owner *windows.SID
}

type fileIdentity struct {
	volume uint32
	high   uint32
	low    uint32
}

type mutationLock struct {
	handle   windows.Handle
	path     string
	identity installedIdentity
	released bool
}

var mutationLockWait = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func acquireMutationLock(ctx context.Context, destination string, allowCreate bool) (*mutationLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, updateLockDeadline)
	defer cancel()

	identity, err := windowsInstalledIdentity(destination)
	if err != nil {
		return nil, fmt.Errorf("update lock unavailable: installed executable identity: %w", err)
	}
	path := destination + ".citadel-update.lock"
	handle, err := openWindowsMutationLock(path, identity, allowCreate)
	if err != nil {
		return nil, fmt.Errorf("update lock unavailable/not safely provisioned: %w", err)
	}
	lock := &mutationLock{handle: handle, path: path, identity: identity}
	for {
		var overlapped windows.Overlapped
		err = windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			if err := validateWindowsLock(handle, path, identity); err != nil {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
				_ = windows.CloseHandle(handle)
				return nil, fmt.Errorf("update lock unavailable/not safely provisioned: %w", err)
			}
			return lock, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("update lock unavailable: %w", err)
		}
		if err := mutationLockWait(ctx, updateLockWait); err != nil {
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("update lock unavailable: timed out waiting for updater transaction: %w", err)
		}
	}
}

func acquireRecoveryMutationLock(ctx context.Context, destination string) (*mutationLock, error) {
	if _, err := os.Stat(destination); err == nil {
		return acquireMutationLock(ctx, destination, false)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	oldPath := destination + ".old"
	newPath := destination + ".new"
	identity, oldErr := windowsInstalledIdentity(oldPath)
	if oldErr != nil {
		if !windowsPathAbsent(oldErr) {
			return nil, fmt.Errorf("update recovery cannot validate original backup: %w", oldErr)
		}
		identity, oldErr = windowsInstalledIdentity(newPath)
		if oldErr != nil {
			return nil, fmt.Errorf("update recovery has no validated owner artifact: %w", oldErr)
		}
	} else if newIdentity, err := windowsInstalledIdentity(newPath); err == nil {
		if !newIdentity.owner.Equals(identity.owner) {
			return nil, fmt.Errorf("update recovery artifacts have mismatched owners")
		}
	} else if !windowsPathAbsent(err) {
		return nil, fmt.Errorf("update recovery cannot validate staged artifact: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, updateLockDeadline)
	defer cancel()
	path := destination + ".citadel-update.lock"
	handle, err := openWindowsMutationLock(path, identity, false)
	if err != nil {
		return nil, fmt.Errorf("update recovery requires existing validated lock: %w", err)
	}
	lock := &mutationLock{handle: handle, path: path, identity: identity}
	for {
		var overlapped windows.Overlapped
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			if err := validateWindowsLock(handle, path, identity); err != nil {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
				_ = windows.CloseHandle(handle)
				return nil, err
			}
			return lock, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("update recovery lock unavailable: %w", err)
		}
		if err := mutationLockWait(ctx, updateLockWait); err != nil {
			_ = windows.CloseHandle(handle)
			return nil, fmt.Errorf("update recovery lock timeout: %w", err)
		}
	}
}

func windowsPathAbsent(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func windowsInstalledIdentity(path string) (installedIdentity, error) {
	h, err := openWindowsPath(path, windows.GENERIC_READ|windows.READ_CONTROL, windows.OPEN_EXISTING, nil)
	if err != nil {
		return installedIdentity{}, err
	}
	defer windows.CloseHandle(h)
	if err := validateWindowsPathIdentity(h, path); err != nil {
		return installedIdentity{}, err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return installedIdentity{}, fmt.Errorf("read executable owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return installedIdentity{}, fmt.Errorf("read executable owner: %w", err)
	}
	copy, err := owner.Copy()
	if err != nil {
		return installedIdentity{}, err
	}
	return installedIdentity{owner: copy}, nil
}

func openWindowsMutationLock(path string, identity installedIdentity, allowCreate bool) (windows.Handle, error) {
	h, err := openWindowsPath(path, windows.GENERIC_READ|windows.SYNCHRONIZE|windows.READ_CONTROL, windows.OPEN_EXISTING, nil)
	if err == nil {
		if err := validateWindowsLock(h, path, identity); err != nil {
			windows.CloseHandle(h)
			return 0, err
		}
		return h, nil
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || !allowCreate {
		return 0, err
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return 0, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || !user.User.Sid.Equals(identity.owner) {
		return 0, fmt.Errorf("first lock creation requires installed executable owner")
	}
	sddl := windowsSecuritySDDL(identity.owner, true)
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err = openWindowsPath(path, windows.GENERIC_READ|windows.SYNCHRONIZE|windows.READ_CONTROL, windows.CREATE_NEW, sa)
	if errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return openWindowsMutationLock(path, identity, false)
	}
	if err != nil {
		return 0, err
	}
	if err := validateWindowsLock(h, path, identity); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

func openWindowsPath(path string, access uint32, creation uint32, sa *windows.SecurityAttributes) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ, sa, creation, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

func openWindowsDirectory(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

func validateWindowsLock(handle windows.Handle, path string, identity installedIdentity) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return fmt.Errorf("read lock security: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(identity.owner) {
		return fmt.Errorf("lock owner does not match installed executable")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("lock DACL is not protected")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("lock DACL is missing")
	}
	if err := validateWindowsACL(dacl, owner, true); err != nil {
		return err
	}
	return validateWindowsPathIdentity(handle, path)
}

func validateWindowsACL(acl *windows.ACL, owner *windows.SID, authenticatedRead bool) error {
	if acl == nil || owner == nil {
		return fmt.Errorf("security descriptor has a null owner or DACL")
	}
	system, _ := windows.StringToSid("S-1-5-18")
	admins, _ := windows.StringToSid("S-1-5-32-544")
	authenticated, _ := windows.StringToSid("S-1-5-11")
	seen := map[string]bool{}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			return fmt.Errorf("lock DACL contains unsupported or inherited ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		key := sid.String()
		if seen[key] {
			return fmt.Errorf("lock DACL contains duplicate principal")
		}
		seen[key] = true
		switch {
		case sid.Equals(owner), sid.Equals(system), sid.Equals(admins):
			if uint32(ace.Mask) != uint32(windows.GENERIC_ALL) && uint32(ace.Mask) != 0x001f01ff {
				return fmt.Errorf("lock DACL has incorrect privileged rights")
			}
		case sid.Equals(authenticated):
			if !authenticatedRead {
				return fmt.Errorf("private DACL contains unexpected principal")
			}
			if uint32(ace.Mask)&uint32(windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|windows.DELETE|windows.WRITE_DAC|windows.WRITE_OWNER|windows.GENERIC_ALL|windows.GENERIC_WRITE) != 0 {
				return fmt.Errorf("lock DACL grants mutating rights to authenticated users")
			}
			if uint32(ace.Mask) != uint32(windows.GENERIC_READ) && uint32(ace.Mask) != uint32(windows.FILE_GENERIC_READ) {
				return fmt.Errorf("lock DACL has incorrect authenticated-user rights")
			}
		default:
			return fmt.Errorf("lock DACL contains unexpected principal")
		}
	}
	expected := map[string]bool{owner.String(): true, system.String(): true, admins.String(): true}
	if authenticatedRead {
		expected[authenticated.String()] = true
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("lock DACL does not match the required principals")
	}
	for sid := range expected {
		if !seen[sid] {
			return fmt.Errorf("lock DACL does not match the required principals")
		}
	}
	return nil
}

func windowsSecuritySDDL(owner *windows.SID, authenticatedRead bool) string {
	ownerText := owner.String()
	entries := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, principal := range []string{ownerText, "SY", "BA"} {
		resolved := principal
		if principal == "SY" {
			resolved = "S-1-5-18"
		} else if principal == "BA" {
			resolved = "S-1-5-32-544"
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		entries = append(entries, "(A;;FA;;;"+principal+")")
	}
	if authenticatedRead && !seen["S-1-5-11"] {
		entries = append(entries, "(A;;GR;;;AU)")
	}
	return "O:" + ownerText + "D:P" + strings.Join(entries, "")
}

func validateWindowsPathIdentity(handle windows.Handle, path string) error {
	var held windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &held); err != nil {
		return err
	}
	other, err := openWindowsPath(path, windows.GENERIC_READ, windows.OPEN_EXISTING, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(other)
	var named windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(other, &named); err != nil {
		return err
	}
	if held.VolumeSerialNumber != named.VolumeSerialNumber || held.FileIndexHigh != named.FileIndexHigh || held.FileIndexLow != named.FileIndexLow {
		return fmt.Errorf("lock path identity changed")
	}
	if held.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || held.NumberOfLinks != 1 {
		return fmt.Errorf("path is not a single-link regular non-reparse file")
	}
	return nil
}

func validateWindowsDirectoryPathIdentity(handle windows.Handle, path string) error {
	var held windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &held); err != nil {
		return err
	}
	other, err := openWindowsDirectory(path, windows.GENERIC_READ)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(other)
	var named windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(other, &named); err != nil {
		return err
	}
	if held.VolumeSerialNumber != named.VolumeSerialNumber || held.FileIndexHigh != named.FileIndexHigh || held.FileIndexLow != named.FileIndexLow {
		return fmt.Errorf("directory path identity changed")
	}
	if held.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || held.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || held.NumberOfLinks != 1 {
		return fmt.Errorf("path is not a single-link non-reparse directory")
	}
	return nil
}

func (l *mutationLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	var overlapped windows.Overlapped
	unlockErr := windows.UnlockFileEx(l.handle, 0, 1, 0, &overlapped)
	closeErr := windows.CloseHandle(l.handle)
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func setStageOwner(file *os.File, identity installedIdentity) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(identity.owner) {
		return fmt.Errorf("new stage owner does not match installed executable")
	}
	return nil
}

func createPrivateAttemptDir(parent string) (string, os.FileInfo, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", nil, err
	}
	sddl := windowsSecuritySDDL(user.User.Sid, false)
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return "", nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for tries := 0; tries < 16; tries++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		dir := filepath.Join(parent, ".attempt-"+hex.EncodeToString(random[:]))
		path16, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return "", nil, err
		}
		if err := windows.CreateDirectory(path16, sa); errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		} else if err != nil {
			return "", nil, err
		}
		h, err := openWindowsDirectory(dir, windows.GENERIC_READ|windows.READ_CONTROL)
		if err != nil {
			_ = os.Remove(dir)
			return "", nil, err
		}
		actual, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil || actual == nil {
			windows.CloseHandle(h)
			_ = os.Remove(dir)
			return "", nil, fmt.Errorf("validate private attempt security: %w", err)
		}
		owner, _, ownerErr := actual.Owner()
		dacl, _, daclErr := actual.DACL()
		control, _, controlErr := actual.Control()
		if ownerErr != nil || daclErr != nil || controlErr != nil || owner == nil || !owner.Equals(user.User.Sid) || control&windows.SE_DACL_PROTECTED == 0 {
			windows.CloseHandle(h)
			_ = os.Remove(dir)
			return "", nil, fmt.Errorf("private attempt directory security does not match policy")
		}
		if err := validateWindowsACL(dacl, owner, false); err != nil {
			windows.CloseHandle(h)
			_ = os.Remove(dir)
			return "", nil, err
		}
		if err := validateWindowsDirectoryPathIdentity(h, dir); err != nil {
			windows.CloseHandle(h)
			_ = os.Remove(dir)
			return "", nil, err
		}
		heldDir := os.NewFile(uintptr(h), dir)
		info, err := heldDir.Stat()
		_ = heldDir.Close()
		if err != nil || !info.IsDir() {
			_ = os.Remove(dir)
			return "", nil, fmt.Errorf("private attempt directory identity unavailable: %w", err)
		}
		return dir, info, nil
	}
	return "", nil, fmt.Errorf("could not allocate unique private update attempt")
}

func openRegularNoFollow(path string) (*os.File, error) {
	h, err := openWindowsPath(path, windows.GENERIC_READ|windows.READ_CONTROL, windows.OPEN_EXISTING, nil)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(h), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("not a regular file")
	}
	if err := validateWindowsPathIdentity(h, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func captureFileIdentity(file *os.File) (fileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return fileIdentity{}, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return fileIdentity{}, fmt.Errorf("file is not a single-link regular non-reparse file")
	}
	return fileIdentity{volume: info.VolumeSerialNumber, high: info.FileIndexHigh, low: info.FileIndexLow}, nil
}

func validateCapturedFileIdentity(file *os.File, expected fileIdentity) error {
	actual, err := captureFileIdentity(file)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("file identity changed")
	}
	return nil
}
