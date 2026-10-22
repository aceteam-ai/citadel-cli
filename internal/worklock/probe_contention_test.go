package worklock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type contentionProbeInfo struct {
	id   string
	mode os.FileMode
}

func (info contentionProbeInfo) Name() string       { return info.id }
func (info contentionProbeInfo) Size() int64        { return 0 }
func (info contentionProbeInfo) Mode() os.FileMode  { return info.mode }
func (info contentionProbeInfo) ModTime() time.Time { return time.Time{} }
func (info contentionProbeInfo) IsDir() bool        { return info.mode.IsDir() }
func (info contentionProbeInfo) Sys() any           { return nil }

type fakeContentionProbeFile struct {
	info     os.FileInfo
	statErr  error
	closeErr error
	closed   int
}

func (file *fakeContentionProbeFile) Stat() (os.FileInfo, error) { return file.info, file.statErr }
func (file *fakeContentionProbeFile) Fd() uintptr               { return 0 }
func (file *fakeContentionProbeFile) Close() error {
	file.closed++
	return file.closeErr
}

func TestProbeContentionUnknownNeverMeansAbsent(t *testing.T) {
	fault := errors.New("private path or record must never be exposed")
	contention := errors.New("kernel contention")
	for _, name := range []string{
		"initial missing", "initial denied", "initial symlink", "initial directory", "initial fifo",
		"open denied", "disappeared before open", "fd stat failed", "fd not regular", "substituted at open",
		"unheld", "held", "held with arbitrary record", "unexpected lock error", "unlock failed", "close failed",
		"held close failed", "disappeared after open", "substituted after open", "held substituted after open", "symlink after open",
	} {
		t.Run(name, func(t *testing.T) {
			original := contentionProbeInfo{id: "original", mode: 0600}
			file := &fakeContentionProbeFile{info: original}
			stats, opens, locks, unlocks := 0, 0, 0, 0
			ops := contentionProbeOps{
				lstat: func(string) (os.FileInfo, error) {
					stats++
					if stats == 1 {
						switch name {
						case "initial missing":
							return nil, os.ErrNotExist
						case "initial denied":
							return nil, fault
						case "initial symlink":
							return contentionProbeInfo{mode: os.ModeSymlink}, nil
						case "initial directory":
							return contentionProbeInfo{mode: os.ModeDir}, nil
						case "initial fifo":
							return contentionProbeInfo{mode: os.ModeNamedPipe}, nil
						}
					} else {
						switch name {
						case "disappeared after open":
							return nil, os.ErrNotExist
						case "substituted after open", "held substituted after open":
							return contentionProbeInfo{id: "replacement", mode: 0600}, nil
						case "symlink after open":
							return contentionProbeInfo{id: "original", mode: os.ModeSymlink}, nil
						}
					}
					return original, nil
				},
				open: func(string) (contentionProbeFile, error) {
					opens++
					if name == "open denied" {
						return nil, fault
					}
					if name == "disappeared before open" {
						return nil, os.ErrNotExist
					}
					return file, nil
				},
				sameFile: func(a, b os.FileInfo) bool { return a.Name() == b.Name() },
				lock: func(contentionProbeFile) error {
					locks++
					if strings.HasPrefix(name, "held") {
						return contention
					}
					if name == "unexpected lock error" {
						return fault
					}
					return nil
				},
				unlock: func(contentionProbeFile) error {
					unlocks++
					if name == "unlock failed" {
						return fault
					}
					return nil
				},
				isContended: func(err error) bool { return err == contention },
			}
			switch name {
			case "fd stat failed":
				file.statErr = fault
			case "fd not regular":
				file.info = contentionProbeInfo{id: "original", mode: os.ModeNamedPipe}
			case "substituted at open":
				file.info = contentionProbeInfo{id: "replacement", mode: 0600}
			case "close failed", "held close failed":
				file.closeErr = fault
			}
			held, err := probeContention("not a real path", ops)
			wantKnown := name == "initial missing" || name == "unheld" || name == "held" || name == "held with arbitrary record"
			wantHeld := name == "held" || name == "held with arbitrary record"
			if (err == nil) != wantKnown || held != wantHeld {
				t.Fatalf("held=%v err=%v", held, err)
			}
			if err != nil && err != errContentionProbeUnavailable {
				t.Fatal("probe exposed an underlying error")
			}
			if strings.HasPrefix(name, "initial") && (opens != 0 || locks != 0 || file.closed != 0) {
				t.Fatal("initial observation unexpectedly opened a file")
			}
			if opens > 0 && name != "open denied" && name != "disappeared before open" && file.closed != 1 {
				t.Fatal("opened descriptor was not closed exactly once")
			}
			if locks > 0 && name != "unexpected lock error" && !strings.HasPrefix(name, "held") && unlocks != 1 {
				t.Fatal("successful probe was not released even after identity failure")
			}
			if strings.HasPrefix(name, "held") && unlocks != 0 {
				t.Fatal("contended lock was unexpectedly unlocked")
			}
		})
	}
}

// These temp-only fixtures exercise the production FD path without processes,
// goroutines, listeners, record parsing, or ownership-changing operations.
func TestProbeContentionNativePreservesExistingFile(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	path := LockPathForStateDir(stateDir)
	marker := []byte("not JSON and not a PID\n")
	if err := os.WriteFile(path, marker, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := ProbeContention(stateDir); held || err != nil {
		t.Fatalf("unheld fixture: held=%v err=%v", held, err)
	}
	ops := nativeContentionProbeOps()
	owner, err := ops.open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.lock(owner); err != nil {
		_ = owner.Close()
		t.Fatal(err)
	}
	held, probeErr := ProbeContention(stateDir)
	unlockErr := ops.unlock(owner)
	closeErr := owner.Close()
	if !held || probeErr != nil || unlockErr != nil || closeErr != nil {
		t.Fatalf("held fixture: held=%v probe=%v unlock=%v close=%v", held, probeErr, unlockErr, closeErr)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("probe replaced the inode or changed permissions")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(marker) {
		t.Fatal("probe changed the file contents")
	}
	missingState := filepath.Join(t.TempDir(), "missing-parent", "network")
	if held, err := ProbeContention(missingState); held || err != nil {
		t.Fatal("initially missing fixture was not absent")
	}
	if _, err := os.Lstat(filepath.Dir(missingState)); !os.IsNotExist(err) {
		t.Fatal("probe created a missing parent")
	}
}

func TestProbeContentionNativeOpenPolicySource(t *testing.T) {
	for file, fragments := range map[string][]string{
		"probe_contention_unix.go": {
			"syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC",
			"syscall.LOCK_EX|syscall.LOCK_NB", "syscall.LOCK_UN",
		},
		"probe_contention_windows.go": {
			"windows.CreateFile(name, windows.GENERIC_READ,",
			"windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,",
			"windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT",
			"windows.FILE_TYPE_DISK", "windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY",
			"windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY",
		},
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(data), fragment) {
				t.Fatal("existing-only nonblocking native probe policy changed")
			}
		}
		for _, forbidden := range []string{"O_CREATE", "OPEN_ALWAYS", "CREATE_ALWAYS", "FILE_SHARE_DELETE", "ReadAt(", "readRecord(", "processAlive(", "Chmod(", "Chown(", "Remove("} {
			if strings.Contains(string(data), forbidden) {
				t.Fatal("probe gained creation, record reads, or mutation")
			}
		}
	}
}
