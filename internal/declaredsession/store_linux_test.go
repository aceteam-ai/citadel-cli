//go:build linux

package declaredsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fixtureStore(t *testing.T) (*Store, *SessionStore) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("supported storage fixture requires an unprivileged owner")
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session, err := store.OpenSession("fixture", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return store, session
}

func fixtureState() State {
	return State{Version: 1, Name: "fixture", Incarnation: strings.Repeat("a", 32), DeclarationRevision: digest([]byte("declaration")), Phase: "running"}
}

func TestStoreReadonlyAndUnsafeInputs(t *testing.T) {
	if os.Geteuid() == 0 {
		if _, err := OpenStore(t.TempDir()); !errors.Is(err, ErrUnsafe) {
			t.Fatal("root store accepted")
		}
		return
	}
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if snapshot, err := store.LoadConfig(); err != nil || len(snapshot.Sessions) != 0 {
		t.Fatalf("missing declarations: %v", err)
	}
	if _, err := store.OpenSession("missing", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readonly session discovery: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("readonly observation created storage")
	}
	path := filepath.Join(dir, "sessions.yaml")
	if err := os.WriteFile(path, []byte(fixtureConfig), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadConfig(); !errors.Is(err, ErrUnsafe) {
		t.Fatal("public declaration file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadConfig(); err != nil {
		t.Fatalf("private declaration file refused: %v", err)
	}
	if err := os.Link(path, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadConfig(); !errors.Is(err, ErrUnsafe) {
		t.Fatal("hardlinked declaration accepted")
	}
}

func TestPrivateMetadataOwnerTypeLinkAndMode(t *testing.T) {
	good := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: 123, Nlink: 1}
	if !privateFileStat(good, 123) {
		t.Fatal("private metadata refused")
	}
	for _, change := range []func(*unix.Stat_t){
		func(st *unix.Stat_t) { st.Uid = 124 },
		func(st *unix.Stat_t) { st.Nlink = 2 },
		func(st *unix.Stat_t) { st.Mode = unix.S_IFIFO | 0600 },
		func(st *unix.Stat_t) { st.Mode = unix.S_IFREG | 0644 },
		func(st *unix.Stat_t) { st.Mode = unix.S_IFREG | unix.S_ISUID | 0600 },
	} {
		bad := good
		change(&bad)
		if privateFileStat(bad, 123) {
			t.Fatal("unsafe owner/type/link/mode accepted")
		}
	}
}

func TestMutableCreationSyncFailureAndRetry(t *testing.T) {
	for _, failure := range []string{"", "inode", "parent"} {
		t.Run("failure-"+failure, func(t *testing.T) {
			store, session := fixtureStore(t)
			var calls []string
			file, err := openMutableAtWith(session.dir, "first.log", unix.O_WRONLY|unix.O_APPEND, store.uid, func(file *os.File) error {
				stage := "inode"
				if file == session.dir {
					stage = "parent"
				} else if _, err := validFile(int(file.Fd()), store.uid); err != nil {
					t.Fatal("new inode was synced before private metadata validation")
				}
				calls = append(calls, stage)
				if stage == failure {
					return errors.New("synthetic sync failure")
				}
				return file.Sync()
			})
			wantCalls := "inode,parent"
			if failure == "inode" {
				wantCalls = "inode"
			}
			if strings.Join(calls, ",") != wantCalls {
				t.Fatalf("sync order %v, want %s", calls, wantCalls)
			}
			if failure == "" {
				if err != nil || file == nil {
					t.Fatalf("creation refused: %v", err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			} else if file != nil || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("uncertain publication claimed success: %v", err)
			}
			path := filepath.Join(session.Paths().Dir, "first.log")
			before, err := os.Lstat(path)
			if err != nil || before.Mode().Perm() != 0600 {
				t.Fatal("failed publication repaired or removed the new inode")
			}
			calls = nil
			file, err = openMutableAtWith(session.dir, "first.log", unix.O_WRONLY|unix.O_APPEND, store.uid, func(file *os.File) error {
				if file != session.dir {
					t.Fatal("retry unexpectedly treated the existing inode as new")
				}
				calls = append(calls, "parent")
				return file.Sync()
			})
			if err != nil || file == nil || strings.Join(calls, ",") != "parent" {
				t.Fatalf("existing retry omitted publication sync: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("retry replaced or repaired the existing inode")
			}
		})
	}
}

func TestDirectoryCreationSyncFailureAndReadOnlyOpen(t *testing.T) {
	for _, failure := range []string{"inode", "parent"} {
		t.Run(failure, func(t *testing.T) {
			store, session := fixtureStore(t)
			var calls []string
			file, err := openDirectoryAtWith(session.dir, "new-dir", true, store.uid, func(file *os.File) error {
				stage := "inode"
				if file == session.dir {
					stage = "parent"
				} else if info, err := file.Stat(); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
					t.Fatal("new directory was synced before private metadata validation")
				}
				calls = append(calls, stage)
				if stage == failure {
					return errors.New("synthetic sync failure")
				}
				return file.Sync()
			})
			wantCalls := "inode,parent"
			if failure == "inode" {
				wantCalls = "inode"
			}
			if file != nil || !errors.Is(err, ErrUnavailable) || strings.Join(calls, ",") != wantCalls {
				t.Fatalf("directory sync failure claimed success: calls=%v err=%v", calls, err)
			}
			// An explicit creation retry persists the visible directory's own
			// metadata before its parent, even though mkdir now sees EEXIST.
			calls = nil
			file, err = openDirectoryAtWith(session.dir, "new-dir", true, store.uid, func(file *os.File) error {
				stage := "inode"
				if file == session.dir {
					stage = "parent"
				}
				calls = append(calls, stage)
				return file.Sync()
			})
			if err != nil || file == nil || strings.Join(calls, ",") != "inode,parent" {
				t.Fatalf("directory retry omitted durability: calls=%v err=%v", calls, err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			file, err = openDirectoryAtWith(session.dir, "new-dir", false, store.uid, func(*os.File) error {
				t.Fatal("read-only open attempted a sync")
				return nil
			})
			if err != nil || file == nil {
				t.Fatalf("read-only existing directory refused: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnvFileUnsafeMetadataFailsBeforeExecutableLookup(t *testing.T) {
	for _, unsafe := range []string{"mode", "symlink", "hardlink"} {
		t.Run(unsafe, func(t *testing.T) {
			store, _ := fixtureStore(t)
			path := filepath.Join(t.TempDir(), "fixture.env")
			if err := os.WriteFile(path, []byte("TOKEN=synthetic-only\n"), 0600); err != nil {
				t.Fatal(err)
			}
			switch unsafe {
			case "mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := path + ".link"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "hardlink":
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := DecodeConfig([]byte(fixtureConfig + "    env_file: " + path + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			owner := OwnerContext{UID: store.uid, Name: "fixture", Home: store.NodeDir(), Hostname: "fixture-host"}
			if _, err := store.BuildSpawn(snapshot.Sessions[0], false, owner, map[string]string{"PATH": "/nonexistent"}); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("env file did not fail for its exact metadata reason: %v", err)
			}
		})
	}
}

func TestStoreStateAndDurableStopOverlay(t *testing.T) {
	store, session := fixtureStore(t)
	if _, present, err := session.ReadState(); err != nil || present {
		t.Fatal("missing state was not absent")
	}
	state := fixtureState()
	if err := session.WriteState(state); err != nil {
		t.Fatal(err)
	}
	got, present, err := session.ReadState()
	if err != nil || !present || got.Incarnation != state.Incarnation {
		t.Fatalf("state roundtrip: %+v %v", got, err)
	}
	marker := StopMarker{Version: 1, Name: state.Name, DeclarationRevision: state.DeclarationRevision, Incarnation: state.Incarnation, StoppedAt: time.Unix(123, 0)}
	if err := session.WriteStopMarker(marker); err != nil {
		t.Fatal(err)
	}
	// A later human edit does not alter, expire or remove the per-name veto.
	if err := os.WriteFile(filepath.Join(store.NodeDir(), "sessions.yaml"), []byte(strings.Replace(fixtureConfig, "automated-citadel", "fixture", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadConfig()
	if err != nil || snapshot.Sessions[0].Revision == marker.DeclarationRevision {
		t.Fatal("fixture did not change the declaration revision")
	}
	stored, present, err := session.ReadStopMarker()
	if err != nil || !present || stored.DeclarationRevision != marker.DeclarationRevision {
		t.Fatal("config edit lost the old stop marker")
	}
	if err := session.ClearStopMarker(nil, stored); !errors.Is(err, ErrConflict) {
		t.Fatal("unguarded marker clear accepted")
	}
	guard, err := session.AcquireOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wrong := stored
	wrong.Incarnation = strings.Repeat("b", 32)
	if err := session.ClearStopMarker(guard, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal("changed marker clear accepted")
	}
	if err := session.ClearStopMarker(guard, stored); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.ClearStopMarker(guard, stored); !errors.Is(err, ErrConflict) {
		t.Fatal("released guard accepted")
	}
	for _, path := range []string{session.Paths().Dir, session.Paths().State} {
		info, err := os.Stat(path)
		if err != nil || (info.IsDir() && info.Mode().Perm() != 0700) || (!info.IsDir() && info.Mode().Perm() != 0600) {
			t.Fatalf("private modes lost: %s %v", path, err)
		}
	}
}

func TestStoreIndependentLockLifetimes(t *testing.T) {
	_, session := fixtureStore(t)
	op, err := session.AcquireOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	owner, err := session.AcquireOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := session.AcquireOwner(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal("second lifetime owner accepted")
	}
	// Holder final writes remain possible while an operator waits for its RPC.
	if err := session.WriteState(fixtureState()); err != nil {
		t.Fatalf("operation/owner lock blocked record storage: %v", err)
	}
	anchor, err := session.SocketAnchor()
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRefusesSymlinkAndFIFOWithoutRepair(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "wrong-mode"} {
		t.Run(kind, func(t *testing.T) {
			_, session := fixtureStore(t)
			path := session.Paths().State
			switch kind {
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "unwritten"), path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				// Pin the nonblocking production open before introducing a FIFO;
				// this fixture must not hang if that guard is accidentally removed.
				source, err := os.ReadFile("store_linux.go")
				if err != nil || !bytes.Contains(source, []byte("unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK")) {
					t.Fatal("nonblocking existing-file open prerequisite missing")
				}
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-mode":
				if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.WriteState(fixtureState()); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("unsafe existing inode repaired or replaced")
			}
		})
	}
}

func TestStorePublicationFailureAndUncertainDurability(t *testing.T) {
	for _, failure := range []string{"short-write", "file-sync", "close", "directory-sync"} {
		t.Run(failure, func(t *testing.T) {
			store, session := fixtureStore(t)
			old := []byte(`{"old":true}`)
			if err := writePrivateAt(session.dir, "fixture.json", old, MaxStateBytes, store.uid); err != nil {
				t.Fatal(err)
			}
			ops := recordIO{
				write: func(file *os.File, body []byte) (int, error) {
					if failure == "short-write" {
						return file.Write(body[:len(body)-1])
					}
					return file.Write(body)
				},
				sync: func(file *os.File) error {
					if (failure == "directory-sync" && file == session.dir) || (failure == "file-sync" && file != session.dir) {
						return errors.New("synthetic sync failure")
					}
					return file.Sync()
				},
				close: func(file *os.File) error {
					err := file.Close()
					if failure == "close" {
						return errors.New("synthetic close failure")
					}
					return err
				},
			}
			fresh := []byte(`{"new":true}`)
			if err := writePrivateAtWith(session.dir, "fixture.json", fresh, MaxStateBytes, store.uid, ops); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("publication failure claimed success: %v", err)
			}
			got, _, err := readPrivateAt(session.dir, "fixture.json", MaxStateBytes, store.uid)
			want := old
			if failure == "directory-sync" {
				want = fresh // Visible rename is not a durability/rollback promise.
			}
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("failure boundary bytes=%s want=%s err=%v", got, want, err)
			}
		})
	}
}

func TestSpawnUsesClosedEnvironmentAndDirectArgv(t *testing.T) {
	store, _ := fixtureStore(t)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "harness"), []byte("never executed"), 0700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "fixture.env")
	if err := os.WriteFile(envFile, []byte("TOKEN=synthetic-secret\nCHOICE=from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := fixtureConfig + "    env_file: " + envFile + "\n    env: {CHOICE: explicit}\n    path_prepend: [" + binDir + "]\n"
	snapshot, err := DecodeConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	owner := OwnerContext{UID: store.uid, Name: "fixture", Home: store.NodeDir(), Hostname: "fixture-host"}
	spec, err := store.BuildSpawn(snapshot.Sessions[0], false, owner, map[string]string{"PATH": "/usr/bin:/bin", "TERM": "xterm"})
	if err != nil || spec.Argv[0] != filepath.Join(binDir, "harness") || strings.Join(spec.Argv[1:], " ") != "exec literal" || !validDigest(spec.EnvironmentHash) {
		t.Fatalf("direct spawn spec: %+v err=%v", spec.Argv, err)
	}
	joined := "\n" + strings.Join(spec.Env, "\n") + "\n"
	for _, value := range []string{"TOKEN=synthetic-secret", "CHOICE=explicit", "ACETEAM_SESSION=automated-citadel", "ACETEAM_MACHINE=fixture-host"} {
		if !strings.Contains(joined, "\n"+value+"\n") {
			t.Fatal("closed environment precedence/identity lost")
		}
	}
	state := fixtureState()
	state.EnvironmentHash = spec.EnvironmentHash
	state.ResolvedExecutable = spec.Argv[0]
	metadata, _ := json.Marshal(state)
	if bytes.Contains(metadata, []byte("synthetic-secret")) {
		t.Fatal("environment values entered state")
	}
	if _, err := store.BuildSpawn(snapshot.Sessions[0], false, owner, map[string]string{"PATH": "/bin", "ACETEAM_API_KEY": "must-not-inherit"}); !errors.Is(err, ErrConfig) {
		t.Fatal("ambient credential accepted")
	}
	restarted, err := store.BuildSpawn(snapshot.Sessions[0], true, owner, map[string]string{"PATH": "/bin"})
	if err != nil || strings.Join(restarted.Argv, "\x00") != strings.Join(spec.Argv, "\x00") {
		t.Fatal("restart invented resume arguments")
	}
}
