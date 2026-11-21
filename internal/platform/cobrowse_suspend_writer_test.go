package platform

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// All process facts and cleanup are fake; only this test's private registry and
// profile are touched. This exercises the actual legacy writer used by starts.
func suspendWriterFixture(t *testing.T) (*cobrowseSession, sessionRecord) {
	t.Helper()
	oldIdentity, oldAdoption := procIdentity, adoptionSupported
	procIdentity = func(int) (uint64, bool, error) { return 42, true, nil }
	adoptionSupported = true
	t.Cleanup(func() { procIdentity, adoptionSupported = oldIdentity, oldAdoption })
	base := t.TempDir()
	profile := filepath.Join(base, "profile")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &cobrowseSession{
		id: "cb-abcd", generation: 7, state: SessionRunning,
		profile: profile, regDir: filepath.Join(base, "registry"),
		proc: &cobrowseProc{browserID: "fake-browser", browserPID: 100001, xvfbPID: 200001, stop: func() error { return nil }},
	}
	s.persistRecord()
	rec, err := loadRecord(s.regDir, s.id)
	if err != nil {
		t.Fatal(err)
	}
	return s, rec
}

func TestSuspendWriterDoesNotResurrectStoppedRecord(t *testing.T) {
	s, _ := suspendWriterFixture(t)
	identity := procIdentity
	stopped := false
	procIdentity = func(pid int) (uint64, bool, error) {
		if pid == 100001 && !stopped {
			stopped = true
			if !s.mu.TryLock() {
				t.Error("identity probe held the lifecycle lock")
				return identity(pid)
			}
			s.mu.Unlock()
			if err := s.teardown(); err != nil {
				t.Fatal(err)
			}
		}
		return identity(pid)
	}
	s.persistRecord()
	if !stopped {
		t.Fatal("test did not interrupt the process identity read")
	}
	if _, err := os.Stat(recordFile(s.regDir, s.id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale live writer resurrected the record after teardown")
	}
}

func TestSuspendWriterDoesNotOverwriteNewLifecycle(t *testing.T) {
	for _, kind := range []string{"process", "generation", "suspending", "suspended", "resuming", "exited"} {
		t.Run(kind, func(t *testing.T) {
			s, original := suspendWriterFixture(t)
			identity := procIdentity
			var expected sessionRecord
			changed := false
			procIdentity = func(pid int) (uint64, bool, error) {
				if pid == 100001 && !changed {
					changed = true
					if !s.mu.TryLock() {
						t.Error("identity probe held the lifecycle lock")
						return identity(pid)
					}
					next := original
					switch kind {
					case "process":
						s.proc = &cobrowseProc{browserID: "replacement", browserPID: 300001}
						next.BrowserID, next.BrowserPID = "replacement", 300001
					case "generation":
						s.generation++
						next.Generation = s.generation
					default:
						s.state = CobrowseSessionState(kind)
						next.State = kind
					}
					s.mu.Unlock()
					if err := saveRecord(s.regDir, next); err != nil {
						t.Fatal(err)
					}
					var err error
					expected, err = loadRecord(s.regDir, s.id)
					if err != nil {
						t.Fatal(err)
					}
				}
				return identity(pid)
			}
			writes := 0
			s.persistRecordWithWriter(func(dir string, rec sessionRecord) error {
				writes++
				return saveRecordUnderRegistryGate(dir, rec)
			})
			got, err := loadRecord(s.regDir, s.id)
			if !changed || writes != 0 || err != nil || !reflect.DeepEqual(got, expected) {
				t.Fatal("stale live writer overwrote a newer lifecycle record")
			}
		})
	}
}

func TestSuspendWriterPublicationSharesLifecycleLock(t *testing.T) {
	for _, state := range []CobrowseSessionState{SessionRunning, SessionAttached} {
		t.Run(string(state), func(t *testing.T) {
			s, _ := suspendWriterFixture(t)
			s.state = state
			calls := 0
			s.persistRecordWithWriter(func(dir string, rec sessionRecord) error {
				calls++
				if s.mu.TryLock() {
					s.mu.Unlock()
					t.Error("publication released the lifecycle lock after its final check")
				}
				if dir != s.regDir || rec.ID != s.id || rec.Generation != 7 || rec.State != RecordLive ||
					rec.OwnerStart != 42 || rec.BrowserStart != 42 || rec.XvfbStart != 42 {
					t.Error("live publication lost its current session or observed process facts")
				}
				return saveRecordUnderRegistryGate(dir, rec)
			})
			if calls != 1 {
				t.Fatal("stable running/attached session did not persist once")
			}
			if !s.mu.TryLock() {
				t.Fatal("publication retained the lifecycle lock after returning")
			}
			s.mu.Unlock()
		})
	}
}

func TestSuspendWriterSnapshotsMetadataAfterIdentity(t *testing.T) {
	s, _ := suspendWriterFixture(t)
	identity := procIdentity
	changed := false
	end := &EndedSession{Reason: "synthetic-restart", At: time.Unix(123, 0).UTC().Format(time.RFC3339)}
	procIdentity = func(pid int) (uint64, bool, error) {
		if pid == 100001 && !changed {
			changed = true
			if !s.mu.TryLock() {
				t.Error("identity probe held the lifecycle lock")
				return identity(pid)
			}
			s.tracking, s.pendingEnd = true, end
			s.mu.Unlock()
		}
		return identity(pid)
	}
	s.persistRecord()
	rec, err := loadRecord(s.regDir, s.id)
	if !changed || err != nil || !rec.Tracking || !reflect.DeepEqual(rec.PendingEnd, end) {
		t.Fatal("live writer published metadata captured before the identity await")
	}
}

func TestSuspendWriterNonLiveAdmissionAndWriteFailure(t *testing.T) {
	for _, kind := range []string{"launching", "exited", "suspending", "suspended", "resuming", "missing-process", "no-registry", "unsupported", "nil-writer", "write-error"} {
		t.Run(kind, func(t *testing.T) {
			s, original := suspendWriterFixture(t)
			dir := s.regDir
			probes, writes := 0, 0
			procIdentity = func(int) (uint64, bool, error) { probes++; return 42, true, nil }
			switch kind {
			case "missing-process":
				s.proc = nil
			case "no-registry":
				s.regDir = ""
			case "unsupported":
				adoptionSupported = false
			case "nil-writer", "write-error":
			default:
				s.state = CobrowseSessionState(kind)
			}
			writer := func(string, sessionRecord) error { writes++; return errors.New("synthetic disk failure") }
			if kind == "nil-writer" {
				writer = nil
			}
			s.persistRecordWithWriter(writer)
			if kind == "write-error" {
				if writes != 1 || probes == 0 {
					t.Fatal("write failure fixture did not reach publication")
				}
			} else if probes != 0 || writes != 0 {
				t.Fatal("ineligible live writer performed an identity probe or publication")
			}
			got, err := loadRecord(dir, s.id)
			if err != nil || !reflect.DeepEqual(got, original) {
				t.Fatal("refused or failed publication changed the existing record")
			}
			if !s.mu.TryLock() {
				t.Fatal("refused or failed publication retained the session lock")
			}
			s.mu.Unlock()
		})
	}
}
