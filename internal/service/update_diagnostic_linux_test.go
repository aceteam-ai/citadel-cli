//go:build linux

package service

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDiagnoseWorkerUpdateReadOnlyTempDirectory(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "citadel")
	if err := os.WriteFile(binary, []byte("synthetic fixture, never executed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	uid := strconv.Itoa(os.Geteuid())
	d := diagnoseWorkerUpdate(ManagedWorker{Unit: ManagedUnit{Name: "fixture"}, Executable: binary, User: "unit-fixture"}, os.Stat, func(string) (*user.User, error) { return &user.User{Uid: uid}, nil }, func(*user.User) ([]string, error) { return nil, nil })
	if os.Geteuid() != 0 && (!d.Warning || !strings.Contains(d.Detail, "mode bits do not grant")) {
		t.Fatalf("0555 directory not flagged: %+v", d)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "citadel" {
		t.Fatalf("doctor wrote a probe or changed the install: %+v %v", entries, err)
	}
	bytes, err := os.ReadFile(binary)
	if err != nil || string(bytes) != "synthetic fixture, never executed" {
		t.Fatalf("doctor changed binary: %q %v", bytes, err)
	}
}

type updateFixtureInfo struct {
	mode     os.FileMode
	uid, gid uint32
}

func (f updateFixtureInfo) Name() string       { return "fixture" }
func (f updateFixtureInfo) Size() int64        { return 1 }
func (f updateFixtureInfo) Mode() os.FileMode  { return f.mode }
func (f updateFixtureInfo) ModTime() time.Time { return time.Time{} }
func (f updateFixtureInfo) IsDir() bool        { return f.mode.IsDir() }
func (f updateFixtureInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

func TestDiagnoseWorkerUpdateUsesUnitIdentityNotCLIInvoker(t *testing.T) {
	for _, tc := range []struct {
		name              string
		binary, directory updateFixtureInfo
		account           string
		groups            []string
		warning           bool
		message           string
	}{
		{"privilege separated", updateFixtureInfo{0755, 0, 0}, updateFixtureInfo{os.ModeDir | 0755, 0, 0}, "9001", []string{"9001"}, true, "owners differ"},
		{"owner matches but directory read only", updateFixtureInfo{0755, 9001, 9001}, updateFixtureInfo{os.ModeDir | 0555, 9001, 9001}, "9001", []string{"9001"}, true, "mode bits do not grant"},
		{"directory lacks search", updateFixtureInfo{0755, 9001, 9001}, updateFixtureInfo{os.ModeDir | 0600, 9001, 9001}, "9001", []string{"9001"}, true, "write and search"},
		{"same owner private directory", updateFixtureInfo{0755, 9001, 9001}, updateFixtureInfo{os.ModeDir | 0700, 9001, 9001}, "9001", []string{"9001"}, false, "remain unverified"},
		{"owner bits override other", updateFixtureInfo{0755, 9001, 9001}, updateFixtureInfo{os.ModeDir | 0577, 9001, 9001}, "9001", []string{"9001"}, true, "mode bits do not grant"},
		{"group grants", updateFixtureInfo{0755, 9001, 9001}, updateFixtureInfo{os.ModeDir | 0770, 9010, 9002}, "9001", []string{"9001", "9002"}, false, "remain unverified"},
		{"group bits override other", updateFixtureInfo{0755, 9001, 9001}, updateFixtureInfo{os.ModeDir | 0757, 9010, 9002}, "9001", []string{"9002"}, true, "mode bits do not grant"},
		{"other grants", updateFixtureInfo{0755, 9001, 9001}, updateFixtureInfo{os.ModeDir | 0703, 9010, 9002}, "9001", []string{"9001"}, false, "remain unverified"},
		{"root layout", updateFixtureInfo{0755, 0, 0}, updateFixtureInfo{os.ModeDir | 0555, 0, 0}, "0", []string{"0"}, false, "remain unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := ManagedWorker{Unit: ManagedUnit{Name: "citadel-work"}, Executable: "/fixture/managed/citadel", User: "unit-worker"}
			stat := func(path string) (os.FileInfo, error) {
				switch path {
				case worker.Executable:
					return tc.binary, nil
				case "/fixture/managed":
					return tc.directory, nil
				default:
					t.Fatalf("unexpected host path %q", path)
					return nil, nil
				}
			}
			lookup := func(name string) (*user.User, error) {
				if name != worker.User {
					t.Fatalf("looked up CLI user instead of unit user: %q", name)
				}
				return &user.User{Uid: tc.account}, nil
			}
			d := diagnoseWorkerUpdate(worker, stat, lookup, func(*user.User) ([]string, error) { return tc.groups, nil })
			if d.Warning != tc.warning || !strings.Contains(d.Detail, tc.message) || !strings.Contains(d.WorkerUser, "unit-worker (uid "+tc.account+")") || d.BinaryOwner == "" || d.Directory != "/fixture/managed" {
				t.Fatalf("diagnostic not tied to unit identity and real directory: %+v", d)
			}
		})
	}
}

func TestDiagnoseWorkerUpdateUnknownIsNeverWritable(t *testing.T) {
	for _, phase := range []string{"no account", "account lookup", "invalid UID", "binary stat", "nonregular binary", "directory stat", "not directory", "group lookup"} {
		t.Run(phase, func(t *testing.T) {
			worker := ManagedWorker{Unit: ManagedUnit{Name: "custom"}, Executable: "/fixture/bin/citadel", User: "unit-worker"}
			if phase == "no account" {
				worker.User = ""
			}
			stat := func(path string) (os.FileInfo, error) {
				if path == worker.Executable {
					if phase == "binary stat" {
						return nil, errors.New("fixture")
					}
					if phase == "nonregular binary" {
						return updateFixtureInfo{mode: os.ModeDir}, nil
					}
					return updateFixtureInfo{0755, 9001, 9001}, nil
				}
				if path != "/fixture/bin" {
					t.Fatalf("unexpected path %q", path)
				}
				if phase == "directory stat" {
					return nil, errors.New("fixture")
				}
				if phase == "not directory" {
					return updateFixtureInfo{}, nil
				}
				return updateFixtureInfo{os.ModeDir | 0770, 9002, 9002}, nil
			}
			lookup := func(string) (*user.User, error) {
				if phase == "account lookup" {
					return nil, errors.New("fixture")
				}
				if phase == "invalid UID" {
					return &user.User{Uid: "invalid"}, nil
				}
				return &user.User{Uid: "9001"}, nil
			}
			d := diagnoseWorkerUpdate(worker, stat, lookup, func(*user.User) ([]string, error) { return nil, errors.New("fixture") })
			if !d.Warning || !strings.Contains(d.Detail, "could not be verified") {
				t.Fatalf("unknown treated as writable: %+v", d)
			}
		})
	}
}

func TestDeliberatelyDisabledAutoUpdateIsInfoEvenForPrivilegedLayout(t *testing.T) {
	worker := ManagedWorker{Unit: ManagedUnit{Name: "fixture"}, Executable: "/fixture/citadel", User: "unit-worker", AutoUpdateDisabled: true}
	d := diagnoseWorkerUpdate(worker, func(string) (os.FileInfo, error) { return updateFixtureInfo{0755, 0, 0}, nil }, func(string) (*user.User, error) { return &user.User{Uid: "9001"}, nil }, func(*user.User) ([]string, error) { t.Fatal("policy diagnosis changed owner checks"); return nil, nil })
	if d.Warning || !d.AutoUpdateDisabled || d.BinaryOwner != "uid 0" || !strings.Contains(d.Detail, "deliberately disabled") || !strings.Contains(d.Detail, "owners differ") {
		t.Fatalf("manual policy was misreported as warning or writable authority: %+v", d)
	}
}
