//go:build linux

package mediainput

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

type workspaceOpenTrace struct {
	calls   []string
	fds     []int
	pin     int
	proc    int
	fail    string
	pinMode *uint32
}

// These fixtures use only owned regular files/directories, symlinks and FIFOs.
// Device observations are synthesized after a real O_PATH regular-file pin;
// no device node is created or opened, and no global syscall hook is changed.
func tracedWorkspaceOpens(t *testing.T) (workspaceOpenOps, *workspaceOpenTrace) {
	t.Helper()
	trace := &workspaceOpenTrace{pin: -1, proc: -1}
	t.Cleanup(func() {
		for _, fd := range trace.fds {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
				t.Errorf("acquisition FD %d was not closed: %v", fd, err)
			}
		}
	})
	remember := func(fd int, err error) (int, error) {
		if err == nil {
			trace.fds = append(trace.fds, fd)
			flags, flagErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if flagErr != nil || flags&unix.FD_CLOEXEC == 0 {
				t.Fatal("acquisition descriptor was not close-on-exec")
			}
		}
		return fd, err
	}
	ops := workspaceOpenOps{
		open: func(path string, flags int, mode uint32) (int, error) {
			trace.calls = append(trace.calls, "proc-open")
			if path != "/proc/self/fd" || flags != unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC || mode != 0 {
				t.Fatal("procfs anchor did not use the fixed path and directory-only pin flags")
			}
			if trace.fail == "proc-open" {
				return -1, unix.ENOENT
			}
			fd, err := remember(unix.Open(path, flags, mode))
			trace.proc = fd
			return fd, err
		},
		openat: func(dir int, path string, flags int, mode uint32) (int, error) {
			stage := "pin-open"
			if trace.pin < 0 {
				if path != "source.png" || flags != unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC || mode != 0 {
					t.Fatal("workspace leaf was opened without exact no-follow O_PATH flags")
				}
			} else {
				stage = "read-open"
				if dir != trace.proc || path != strconv.Itoa(trace.pin) || flags != unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC || mode != 0 {
					t.Fatal("readable open was not confined to the numeric pinned FD alias")
				}
				if _, err := unix.FcntlInt(uintptr(trace.pin), unix.F_GETFD, 0); err != nil {
					t.Fatal("O_PATH pin was closed before its readable open")
				}
			}
			trace.calls = append(trace.calls, stage)
			if trace.fail == stage {
				return -1, unix.EACCES
			}
			fd, err := remember(unix.Openat(dir, path, flags, mode))
			if stage == "pin-open" {
				trace.pin = fd
			}
			return fd, err
		},
		fstat: func(fd int, stat *unix.Stat_t) error {
			stage := "read-stat"
			if fd == trace.pin {
				stage = "pin-stat"
			}
			trace.calls = append(trace.calls, stage)
			if trace.fail == stage {
				return unix.EIO
			}
			if err := unix.Fstat(fd, stat); err != nil {
				return err
			}
			if stage == "pin-stat" && trace.pinMode != nil {
				stat.Mode = *trace.pinMode
			}
			if stage == "read-stat" {
				switch trace.fail {
				case "read-type":
					stat.Mode = unix.S_IFCHR
				case "read-device":
					stat.Dev++
				case "read-inode":
					stat.Ino++
				}
			}
			return nil
		},
		fstatfs: func(fd int, stat *unix.Statfs_t) error {
			trace.calls = append(trace.calls, "proc-statfs")
			if fd != trace.proc {
				t.Fatal("filesystem check did not inspect the anchored procfs directory")
			}
			if trace.fail == "proc-statfs" {
				return unix.EIO
			}
			if err := unix.Fstatfs(fd, stat); err != nil {
				return err
			}
			if trace.fail == "non-procfs" {
				stat.Type = 0
			}
			return nil
		},
	}
	return ops, trace
}

func ownedWorkspaceRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func TestWorkspacePinRefusesAllNonRegularTypesBeforeReadableOpen(t *testing.T) {
	for _, mode := range []uint32{0, unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFDIR, unix.S_IFLNK} {
		t.Run(strconv.FormatUint(uint64(mode), 8), func(t *testing.T) {
			root, dir := ownedWorkspaceRoot(t)
			writeOwnedInput(t, dir, "source.png", []byte("owned fixture"))
			ops, trace := tracedWorkspaceOpens(t)
			trace.pinMode = &mode
			file, err := openWorkspaceInputWithOps(root, "source.png", ops)
			if file != nil || !errors.Is(err, os.ErrInvalid) {
				t.Fatalf("non-regular type was not refused: %v", err)
			}
			if !reflect.DeepEqual(trace.calls, []string{"pin-open", "pin-stat"}) {
				t.Fatalf("non-regular type reached readable acquisition: %v", trace.calls)
			}
		})
	}
}

func TestWorkspacePinRefusesRealNonRegularLeavesBeforeReadableOpen(t *testing.T) {
	for _, kind := range []string{"fifo", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, dir := ownedWorkspaceRoot(t)
			path := filepath.Join(dir, "source.png")
			var err error
			switch kind {
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink":
				writeOwnedInput(t, dir, "target.png", []byte("must not follow"))
				err = os.Symlink("target.png", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			ops, trace := tracedWorkspaceOpens(t)
			file, err := openWorkspaceInputWithOps(root, "source.png", ops)
			if file != nil || !errors.Is(err, os.ErrInvalid) {
				t.Fatalf("non-regular leaf was not refused: %v", err)
			}
			if !reflect.DeepEqual(trace.calls, []string{"pin-open", "pin-stat"}) {
				t.Fatalf("non-regular leaf reached readable acquisition: %v", trace.calls)
			}
		})
	}
}

func TestWorkspaceReadableOpenKeepsPinnedInodeAcrossPathChanges(t *testing.T) {
	for _, replacement := range []string{"regular", "fifo", "symlink", "removed"} {
		t.Run(replacement, func(t *testing.T) {
			root, dir := ownedWorkspaceRoot(t)
			original := "owned original image bytes"
			writeOwnedInput(t, dir, "source.png", []byte(original))
			ops, trace := tracedWorkspaceOpens(t)
			openProc := ops.open
			ops.open = func(path string, flags int, mode uint32) (int, error) {
				if err := os.Remove(filepath.Join(dir, "source.png")); err != nil {
					t.Fatal(err)
				}
				switch replacement {
				case "regular":
					writeOwnedInput(t, dir, "source.png", []byte("different inode"))
				case "fifo":
					if err := unix.Mkfifo(filepath.Join(dir, "source.png"), 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					outside := t.TempDir()
					writeOwnedInput(t, outside, "other.png", []byte("outside owned fixture"))
					if err := os.Symlink(filepath.Join(outside, "other.png"), filepath.Join(dir, "source.png")); err != nil {
						t.Fatal(err)
					}
				}
				return openProc(path, flags, mode)
			}
			file, err := openWorkspaceInputWithOps(root, "source.png", ops)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { file.Close() })
			got, err := io.ReadAll(file)
			if err != nil || string(got) != original {
				t.Fatal("readable open lost the original pinned inode")
			}
			flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
			if err != nil || flags&unix.O_ACCMODE != unix.O_RDONLY || flags&unix.O_PATH != 0 || flags&unix.O_NONBLOCK == 0 {
				t.Fatal("returned FD was not read-only and nonblocking")
			}
			want := []string{"pin-open", "pin-stat", "proc-open", "proc-statfs", "read-open", "read-stat"}
			if !reflect.DeepEqual(trace.calls, want) {
				t.Fatalf("unsafe acquisition order: %v", trace.calls)
			}
		})
	}
}

func TestWorkspacePinFailsClosedWithoutLeaksOrPathFallback(t *testing.T) {
	stages := []string{"pin-open", "pin-stat", "proc-open", "proc-statfs", "read-open", "read-stat"}
	for _, failure := range []string{
		"pin-open", "pin-stat", "proc-open", "proc-statfs", "non-procfs",
		"read-open", "read-stat", "read-type", "read-device", "read-inode",
	} {
		t.Run(failure, func(t *testing.T) {
			root, dir := ownedWorkspaceRoot(t)
			writeOwnedInput(t, dir, "source.png", []byte("owned fixture"))
			ops, trace := tracedWorkspaceOpens(t)
			trace.fail = failure
			file, err := openWorkspaceInputWithOps(root, "source.png", ops)
			if file != nil || err == nil {
				t.Fatal("failed acquisition returned a readable descriptor")
			}
			stop := failure
			switch failure {
			case "non-procfs":
				stop = "proc-statfs"
			case "read-type", "read-device", "read-inode":
				stop = "read-stat"
			}
			for i, stage := range stages {
				if stage == stop && !reflect.DeepEqual(trace.calls, stages[:i+1]) {
					t.Fatalf("failed acquisition continued or fell back to a path: %v", trace.calls)
				}
			}
		})
	}
}

func TestWorkspacePinDoesNotGrantReadPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may bypass the fixture's ordinary read permission")
	}
	root, dir := ownedWorkspaceRoot(t)
	writeOwnedInput(t, dir, "source.png", []byte("owned unreadable fixture"))
	if err := os.Chmod(filepath.Join(dir, "source.png"), 0); err != nil {
		t.Fatal(err)
	}
	ops, trace := tracedWorkspaceOpens(t)
	file, err := openWorkspaceInputWithOps(root, "source.png", ops)
	if file != nil || !errors.Is(err, unix.EACCES) {
		t.Fatalf("O_PATH pin bypassed readable-open permission: %v", err)
	}
	want := []string{"pin-open", "pin-stat", "proc-open", "proc-statfs", "read-open"}
	if !reflect.DeepEqual(trace.calls, want) {
		t.Fatalf("read refusal was bypassed or fell back to a path: %v", trace.calls)
	}
}

func TestWorkspaceInvalidPathNeverReachesLeafAcquisition(t *testing.T) {
	for _, path := range []string{"", "../source.png", "/source.png", "nested/../source.png", "source\\png"} {
		t.Run(path, func(t *testing.T) {
			root, _ := ownedWorkspaceRoot(t)
			ops, trace := tracedWorkspaceOpens(t)
			file, err := openWorkspaceInputWithOps(root, path, ops)
			if file != nil || !errors.Is(err, os.ErrInvalid) || len(trace.calls) != 0 {
				t.Fatal("invalid lexical path reached acquisition")
			}
		})
	}
}
