package cobrowserecovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testScope = Scope{"dfc2ce8c-2089-40bb-b4ed-057465aeeff5", "1297", "b21fb4bb-f292-4335-9acc-6d08e3cfdb82", "2b047249-f058-43f7-baea-8e687464b6a0"}

type fakeOps struct {
	starts, stops     int
	state             State
	attempted         bool
	startErr, stopErr error
	confirmed         bool
	beforeStart       func(string)
}

func (o *fakeOps) Start(id string) (bool, error) {
	o.starts++
	if o.beforeStart != nil {
		o.beforeStart(id)
	}
	if o.startErr == nil {
		o.state = Running
	}
	return o.attempted, o.startErr
}
func (o *fakeOps) Observe(string) State { return o.state }
func (o *fakeOps) Stop(string) (bool, error) {
	o.stops++
	if o.confirmed && o.stopErr == nil {
		o.state = Ended
	}
	return o.confirmed, o.stopErr
}
func testEngine(t *testing.T) (Engine, string) {
	t.Helper()
	if !Supported() {
		t.Skip("Unix durable adapter")
	}
	dir := filepath.Join(t.TempDir(), "journal")
	return Engine{Store: FileStore{Dir: dir}}, dir
}
func digest(t *testing.T, url, profile string) string {
	t.Helper()
	d, e := Digest(url, profile)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func mustDo(t *testing.T, e Engine, action string, scope Scope, d string, o Ops) Result {
	t.Helper()
	r, err := e.Do(action, scope, d, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLostReplyEmptyReaderAndQuotaSweep(t *testing.T) {
	e, dir := testEngine(t)
	o := &fakeOps{confirmed: true}
	d := digest(t, "https://example.test/path?secret=do-not-store", "private-profile")
	o.beforeStart = func(id string) {
		b, err := os.ReadFile(filepath.Join(dir, "records.json"))
		if err != nil {
			t.Fatal(err)
		}
		var rows []Record
		if json.Unmarshal(b, &rows) != nil || len(rows) != 1 || rows[0].State != Launching || rows[0].SessionID != id {
			t.Fatalf("no pre-launch record: %s", b)
		}
		if strings.Contains(string(b), "secret") || strings.Contains(string(b), "private-profile") {
			t.Fatal("request leaked into journal")
		}
	}
	// Start's reply is lost. No result reader or old request process remains.
	started := mustDo(t, e, "start", testScope, d, o)
	restarted := Engine{Store: FileStore{Dir: dir}}
	status := mustDo(t, restarted, "job_status", testScope, "", o)
	if status.State != Running || status.SessionID != started.SessionID {
		t.Fatalf("recovery: %+v", status)
	}
	replay := mustDo(t, restarted, "start", testScope, d, o)
	if replay != status || o.starts != 1 {
		t.Fatalf("duplicate launch: %+v starts=%d", replay, o.starts)
	}
	// The backend quota sweep addresses the original job, not its lost result.
	stopped := mustDo(t, restarted, "stop_by_job_id", testScope, "", o)
	if stopped.State != Stopped || o.stops != 1 {
		t.Fatalf("quota stop: %+v", stopped)
	}
	if r := mustDo(t, restarted, "start", testScope, d, o); r.State != Stopped || o.starts != 1 {
		t.Fatal("terminal replay launched")
	}
	if r := mustDo(t, restarted, "stop_by_job_id", testScope, "", o); r.State != Stopped || o.stops != 1 {
		t.Fatal("stop was not idempotent")
	}
}
func TestStopBeforeStartCreatesDurableTombstone(t *testing.T) {
	e, dir := testEngine(t)
	o := &fakeOps{}
	r := mustDo(t, e, "stop_by_job_id", testScope, "", o)
	if r.State != Stopped || r.SessionID != "" {
		t.Fatalf("%+v", r)
	}
	e = Engine{Store: FileStore{Dir: dir}}
	if r = mustDo(t, e, "start", testScope, digest(t, "https://example.test", ""), o); r.State != Stopped || o.starts != 0 {
		t.Fatal("late start escaped tombstone")
	}
}
func TestScopeAndRequestConflictNeverRevealOrTouchSession(t *testing.T) {
	e, _ := testEngine(t)
	o := &fakeOps{confirmed: true}
	d := digest(t, "https://example.test/?x=one", "p")
	mustDo(t, e, "start", testScope, d, o)
	for _, field := range []string{"Org", "Owner", "Node"} {
		t.Run(field, func(t *testing.T) {
			other := testScope
			switch field {
			case "Org":
				other.Org = uuid.NewString()
			case "Owner":
				other.Owner = uuid.NewString()
			case "Node":
				other.Node = "another"
			}
			for _, action := range []string{"start", "job_status", "stop_by_job_id"} {
				r, err := e.Do(action, other, d, o)
				if !errors.Is(err, ErrConflict) || r.SessionID != "" || r.State != "" {
					t.Fatalf("scope leak %s: %+v %v", action, r, err)
				}
			}
		})
	}
	if _, err := e.Do("start", testScope, digest(t, "https://example.test/?x=two", "p"), o); !errors.Is(err, ErrConflict) {
		t.Fatal("query not bound")
	}
	if _, err := e.Do("start", testScope, digest(t, "https://example.test/?x=one", "different"), o); !errors.Is(err, ErrConflict) {
		t.Fatal("profile not bound")
	}
	if o.starts != 1 || o.stops != 0 {
		t.Fatal("conflict touched browser")
	}
}
func TestFailedPINIsTerminalButUncertainLaunchIsNot(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(fmt.Sprint(attempted), func(t *testing.T) {
			e, _ := testEngine(t)
			o := &fakeOps{attempted: attempted, startErr: errors.New("PIN sentinel; URL query sentinel")}
			d := digest(t, "https://example.test", "")
			_, err := e.Do("start", testScope, d, o)
			if err == nil || strings.Contains(err.Error(), "sentinel") {
				t.Fatal("start error leaked or absent")
			}
			r := mustDo(t, e, "start", testScope, d, o)
			want := Failed
			if attempted {
				want = Unknown
			}
			if r.State != want || o.starts != 1 {
				t.Fatalf("%+v starts=%d", r, o.starts)
			}
		})
	}
}
func TestUnknownAbsenceAndStopNeverConfirmOrRelaunch(t *testing.T) {
	e, _ := testEngine(t)
	o := &fakeOps{}
	d := digest(t, "https://example.test", "")
	mustDo(t, e, "start", testScope, d, o)
	o.state = Unknown
	if r := mustDo(t, e, "job_status", testScope, "", o); r.State != Unknown {
		t.Fatalf("%+v", r)
	}
	if r := mustDo(t, e, "stop_by_job_id", testScope, "", o); r.State != StopRequested {
		t.Fatalf("%+v", r)
	}
	if r := mustDo(t, e, "start", testScope, d, o); r.State != StopRequested || o.starts != 1 {
		t.Fatal("uncertain stop replay launched")
	}
	o.confirmed = true
	if r := mustDo(t, e, "stop_by_job_id", testScope, "", o); r.State != Stopped {
		t.Fatalf("%+v", r)
	}
}
func TestStopDuringLaunchCannotConfirmBeforeStartFinishes(t *testing.T) {
	e, _ := testEngine(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	o := &fakeOps{confirmed: true, beforeStart: func(string) { close(entered); <-release }}
	go func() { _, err := e.Do("start", testScope, digest(t, "https://example.test", ""), o); done <- err }()
	<-entered
	if r, err := e.Do("stop_by_job_id", testScope, "", &fakeOps{confirmed: true}); !errors.Is(err, ErrUnavailable) || r.State == Stopped {
		t.Fatalf("false stop while launching: %+v %v", r, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := mustDo(t, e, "stop_by_job_id", testScope, "", o); r.State != Stopped {
		t.Fatal("retry did not stop")
	}
}
func TestJournalFailureBeforeAttemptAndAfterRenameIsFailClosed(t *testing.T) {
	if !Supported() {
		t.Skip("Unix durable adapter")
	}
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "journal")
			calls := 0
			store := FileStore{Dir: dir, sync: func(f *os.File) error {
				calls++
				if calls == failAt {
					return errors.New("injected fsync")
				}
				return f.Sync()
			}}
			e := Engine{Store: store}
			o := &fakeOps{}
			d := digest(t, "https://example.test", "")
			if _, err := e.Do("start", testScope, d, o); !errors.Is(err, ErrUnavailable) || o.starts != 0 {
				t.Fatalf("uncommitted launch: %v %d", err, o.starts)
			}
			if failAt == 2 {
				// Rename happened but directory fsync was uncertain. Its visible launching
				// record must never become authority for a second launch on restart.
				e.Store = FileStore{Dir: dir}
				r := mustDo(t, e, "start", testScope, d, o)
				if r.State != Unknown || o.starts != 0 {
					t.Fatal("uncertain commit re-executed")
				}
			}
		})
	}
}
func TestStoreRejectsCorruptionLinksAndPublicPermissions(t *testing.T) {
	if !Supported() {
		t.Skip("Unix durable adapter")
	}
	for _, kind := range []string{"corrupt", "oversize", "duplicate", "file-link", "dir-link", "public-file", "public-dir", "staging-link"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "journal")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "records.json")
			r := Record{Scope: testScope, State: Stopped, TerminalAt: time.Now()}
			b, _ := json.Marshal([]Record{r, r})
			switch kind {
			case "corrupt":
				os.WriteFile(path, []byte("{"), 0600)
			case "oversize":
				os.WriteFile(path, make([]byte, maxJournalBytes+1), 0600)
			case "duplicate":
				os.WriteFile(path, b, 0600)
			case "file-link":
				os.Symlink(filepath.Join(root, "missing"), path)
			case "dir-link":
				os.Remove(dir)
				os.Symlink(root, dir)
			case "public-file":
				os.WriteFile(path, []byte("[]"), 0644)
			case "public-dir":
				os.Chmod(dir, 0755)
			case "staging-link":
				os.WriteFile(path, []byte("[]"), 0600)
				os.Symlink(filepath.Join(root, "missing"), filepath.Join(dir, "records.next"))
			}
			o := &fakeOps{}
			e := Engine{Store: FileStore{Dir: dir}}
			if _, err := e.Do("start", testScope, digest(t, "https://example.test", ""), o); err == nil || o.starts != 0 {
				t.Fatal("unsafe journal allowed start")
			}
		})
	}
}
func TestCapacityKeepsActiveAnd48HourTombstones(t *testing.T) {
	e, _ := testEngine(t)
	at := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return at }
	tx, err := e.Store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxRecords; i++ {
		s := testScope
		s.JobID = fmt.Sprintf("%08x-2089-40bb-b4ed-057465aeeff5", i+1)
		tx.Records()[s.JobID] = Record{Scope: s, State: Stopped, TerminalAt: at}
	}
	if tx.Save() != nil {
		t.Fatal("fill")
	}
	tx.Close()
	o := &fakeOps{}
	at = at.Add(Retention - time.Second)
	if _, err = e.Do("start", testScope, digest(t, "https://example.test", ""), o); !errors.Is(err, ErrUnavailable) || o.starts != 0 {
		t.Fatal("evicted protected tombstone")
	}
	at = at.Add(time.Second)
	mustDo(t, e, "start", testScope, digest(t, "https://example.test", ""), o)
	if o.starts != 1 {
		t.Fatal("expired tombstones not reclaimed")
	}
}
func TestConcurrentDuplicateLaunchesOnlyOnce(t *testing.T) {
	e, _ := testEngine(t)
	var wg sync.WaitGroup
	entered := make(chan struct{})
	release := make(chan struct{})
	o := &fakeOps{beforeStart: func(string) { close(entered); <-release }}
	d := digest(t, "https://example.test", "")
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := e.Do("start", testScope, d, o)
		if err != nil {
			t.Error(err)
		}
	}()
	<-entered
	if _, err := e.Do("start", testScope, d, &fakeOps{}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("parallel duplicate admitted")
	}
	close(release)
	wg.Wait()
	mustDo(t, e, "start", testScope, d, o)
	if o.starts != 1 {
		t.Fatal("duplicate launch")
	}
}

func TestCanonicalScopeAndFramedDigest(t *testing.T) {
	e, _ := testEngine(t)
	d := digest(t, "https://example.test", "")
	o := &fakeOps{}
	for _, bad := range []Scope{
		{JobID: strings.ToUpper(testScope.JobID), Node: testScope.Node, Org: testScope.Org, Owner: testScope.Owner},
		{JobID: testScope.JobID, Node: "../node", Org: testScope.Org, Owner: testScope.Owner},
		{JobID: testScope.JobID, Node: testScope.Node, Org: "", Owner: testScope.Owner},
		{JobID: testScope.JobID, Node: testScope.Node, Org: testScope.Org, Owner: uuid.Nil.String()},
	} {
		if _, err := e.Do("start", bad, d, o); !errors.Is(err, ErrConflict) {
			t.Fatal("invalid scope accepted")
		}
	}
	if o.starts != 0 {
		t.Fatal("invalid scope executed")
	}
	left := digest(t, "https://example.test/?value=a&profile=b", "c")
	right := digest(t, "https://example.test/?value=a", "b&profile=c")
	if left == right {
		t.Fatal("ambiguous request framing")
	}
	if digest(t, "HTTPS://EXAMPLE.TEST/path", "") != digest(t, "https://example.test/path", "") {
		t.Fatal("canonical host/scheme mismatch")
	}
}
func TestTerminalRetentionBeginsAfterConfirmedExit(t *testing.T) {
	e, dir := testEngine(t)
	clock := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return clock }
	o := &delayedStopOps{fakeOps: fakeOps{confirmed: true}, advance: func() { clock = clock.Add(time.Minute) }}
	mustDo(t, e, "start", testScope, digest(t, "https://example.test", ""), o)
	mustDo(t, e, "stop_by_job_id", testScope, "", o)
	tx, err := (FileStore{Dir: dir}).Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	if !tx.Records()[testScope.JobID].TerminalAt.Equal(clock) {
		t.Fatal("retention started before browser exit")
	}
}

type delayedStopOps struct {
	fakeOps
	advance func()
}

func (o *delayedStopOps) Stop(id string) (bool, error) { o.advance(); return o.fakeOps.Stop(id) }
