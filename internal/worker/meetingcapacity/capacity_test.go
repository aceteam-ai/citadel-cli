package meetingcapacity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

var paths = []Path{APIRead, APIWake, RedisRead, RedisReclaim, RedisWake, WSInitial, WSReconnect, WSSubscription}

func binding(node string) Binding { return Binding{"org-a", node, "incarnation-a"} }

func reconciled(t *testing.T, b Binding) *Capacity {
	t.Helper()
	c, err := New(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileStartup(StartupProof{b, true, true, true}); err != nil {
		t.Fatal(err)
	}
	return c
}

func unit(b Binding, path Path) ClaimUnit {
	return ClaimUnit{b, MeetingOnly, path, "org-meeting-only", 1}
}

func reserve(t *testing.T, c *Capacity, b Binding, path Path) Reservation {
	t.Helper()
	r, err := c.BeforeClaim(unit(b, path))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func claim(t *testing.T, c *Capacity, b Binding, path Path, job string) Reservation {
	t.Helper()
	r := reserve(t, c, b, path)
	if err := c.ObserveRead(r, ReadObservation{b, ReadClaimed, job, true}); err != nil {
		t.Fatal(err)
	}
	return r
}

func requireOccupied(t *testing.T, c *Capacity, b Binding) {
	t.Helper()
	for _, path := range paths {
		if _, err := c.BeforeClaim(unit(b, path)); !errors.Is(err, ErrBusy) && !errors.Is(err, ErrUncertain) {
			t.Fatalf("path %d admitted occupied meeting capacity: %v", path, err)
		}
	}
}

func TestStartupAndIncarnationNeverImplyReconciliation(t *testing.T) {
	b := binding("node-a")
	for _, proof := range []StartupProof{
		{b, false, true, true}, {b, true, false, true}, {b, true, true, false},
		{Binding{b.Organization, b.Node, "old-incarnation"}, true, true, true},
		{Binding{"foreign-org", b.Node, b.Incarnation}, true, true, true},
	} {
		c, err := New(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.ReconcileStartup(proof); err == nil {
			t.Fatalf("accepted incomplete proof: %+v", proof)
		}
		requireOccupied(t, c, b)
	}
	var zero Capacity
	if _, err := zero.BeforeClaim(unit(Binding{}, APIRead)); err == nil {
		t.Fatal("zero value admitted")
	}
	if _, err := New(Binding{Organization: "org-a"}); err == nil {
		t.Fatal("missing node/incarnation admitted")
	}
	c := reconciled(t, b)
	old := claim(t, c, b, APIRead, "old-job")
	restarted, _ := New(b) // even reusing an incarnation does not free capacity
	requireOccupied(t, restarted, b)
	if err := restarted.ObserveRead(old, ReadObservation{b, ReadEmpty, "", true}); !errors.Is(err, ErrReservation) {
		t.Fatal(err)
	}
	requireOccupied(t, restarted, b)
}

// queue models a homogeneous, source-classified claim unit. Its counters are
// ownership effects, not a mock return asserted to come back unchanged.
type queue struct {
	mu                          sync.Mutex
	jobs                        []string
	owners                      map[string]string
	attempts, claims, ack, nack int
}

func (q *queue) consume(node string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.jobs) == 0 {
		return ""
	}
	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	q.owners[job] = node
	q.attempts++
	q.claims++
	return job
}

func consume(c *Capacity, b Binding, path Path, q *queue) (Reservation, string, error) {
	r, err := c.BeforeClaim(unit(b, path))
	if err != nil {
		return Reservation{}, "", err
	}
	job := q.consume(b.Node)
	outcome := ReadClaimed
	if job == "" {
		outcome = ReadEmpty
	}
	return r, job, c.ObserveRead(r, ReadObservation{b, outcome, job, true})
}

func TestEveryIntakePathLeavesBusyMeetingUnclaimedForPeer(t *testing.T) {
	for _, path := range paths {
		t.Run(fmt.Sprint(path), func(t *testing.T) {
			a, b := binding("node-a"), binding("node-b")
			ca, cb := reconciled(t, a), reconciled(t, b)
			claim(t, ca, a, APIRead, "live-meeting")
			before := ca.Snapshot()
			q := &queue{jobs: []string{"next-meeting"}, owners: make(map[string]string)}
			if _, _, err := consume(ca, a, path, q); !errors.Is(err, ErrBusy) {
				t.Fatal(err)
			}
			if len(q.jobs) != 1 || q.attempts != 0 || q.claims != 0 || q.ack != 0 || q.nack != 0 || len(q.owners) != 0 {
				t.Fatalf("busy path changed ownership: %+v", q)
			}
			if ca.Snapshot() != before {
				t.Fatal("busy read changed existing custody")
			}
			if _, job, err := consume(cb, b, path, q); err != nil || job != "next-meeting" {
				t.Fatalf("peer: %q %v", job, err)
			}
			if q.owners["next-meeting"] != "node-b" || q.claims != 1 || q.attempts != 1 || q.ack != 0 || q.nack != 0 {
				t.Fatalf("wrong peer effects: %+v", q)
			}
			other := ClaimUnit{a, UnrelatedOnly, path, "other-only", 5}
			if _, err := ca.BeforeClaim(other); err != nil {
				t.Fatalf("unrelated work blocked: %v", err)
			}
			if ca.Snapshot() != before {
				t.Fatal("unrelated job consumed meeting capacity")
			}
			// Other-only units keep their transport's existing configuration;
			// meeting policy does not impose a new batch/queue default on them.
			other.Queue, other.MaxDeliveries, other.Path = "", 0, Path(255)
			if _, err := ca.BeforeClaim(other); err != nil {
				t.Fatalf("meeting policy changed unrelated defaults: %v", err)
			}
		})
	}
}

func TestClaimUnitsRefuseMixedUnknownAndRepeatedBatchCredits(t *testing.T) {
	b := binding("node-a")
	for _, bad := range []ClaimUnit{
		{b, Mixed, APIRead, "mixed", 1}, {b, Unclassified, RedisRead, "unknown", 1},
		{b, MeetingOnly, WSInitial, "meeting", 2}, {b, MeetingOnly, WSReconnect, "meeting", 0},
		{b, MeetingOnly, Path(255), "meeting", 1},
		{Binding{"foreign", b.Node, b.Incarnation}, MeetingOnly, APIRead, "meeting", 1},
		{Binding{b.Organization, b.Node, "old"}, MeetingOnly, APIRead, "meeting", 1},
	} {
		c := reconciled(t, b)
		before := c.Snapshot()
		if _, err := c.BeforeClaim(bad); err == nil {
			t.Fatalf("accepted unsafe unit: %+v", bad)
		}
		if c.Snapshot() != before {
			t.Fatal("refused unit changed custody")
		}
	}
}

func TestOutstandingReadAndLateBlockedResultKeepSamePermit(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	r := reserve(t, c, b, APIRead)
	serial := c.Snapshot().Serial
	// Wake bursts and reconnect cannot acquire a second read in the
	// poll-to-dispatch gap, even before a job identity is available.
	requireOccupied(t, c, b)
	if err := c.ObserveRead(r, ReadObservation{b, ReadUncertain, "", false}); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	if err := c.ObserveRead(r, ReadObservation{b, ReadEmpty, "", false}); !errors.Is(err, ErrObservation) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	if err := c.ObserveRead(r, ReadObservation{b, ReadClaimed, "late-job", true}); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Serial != serial || got.Job != "late-job" || got.Phase != Held {
		t.Fatalf("late result lost custody: %+v", got)
	}
	requireOccupied(t, c, b)
	// Repeated receipt is idempotent and still confers no new intake credit.
	if err := c.ObserveRead(r, ReadObservation{b, ReadClaimed, "late-job", true}); err != nil {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
}

func TestDefiniteEmptyRequiresOneShotSettlementAndExactBinding(t *testing.T) {
	b := binding("node-a")
	for _, evidence := range []ReadObservation{
		{b, ReadEmpty, "", false}, {b, ReadUncertain, "", false},
		{Binding{b.Organization, b.Node, "old"}, ReadEmpty, "", true},
	} {
		c := reconciled(t, b)
		r := reserve(t, c, b, WSInitial)
		if err := c.ObserveRead(r, evidence); err == nil {
			t.Fatalf("accepted insufficient evidence: %+v", evidence)
		}
		requireOccupied(t, c, b)
		if err := c.ObserveRead(r, ReadObservation{b, ReadEmpty, "", true}); err != nil {
			t.Fatal(err)
		}
		if got := c.Snapshot(); got.Phase != Free {
			t.Fatal(got)
		}
	}
}

func TestActualHandlerExitAndPostExitTeardownAreBothRequired(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	r := claim(t, c, b, RedisReclaim, "meeting")
	// A deadline wrapper, publish, ACK, lease expiry or process liveness
	// gives the kernel no actual-exit proof. Early cleanup is insufficient.
	if err := c.ConfirmTeardownAfterExit(r, "meeting", true); !errors.Is(err, ErrHandlerLive) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	if err := c.HandlerExited(r, "wrong-job"); !errors.Is(err, ErrJob) {
		t.Fatal(err)
	}
	if err := c.HandlerExited(r, "meeting"); err != nil {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	if err := c.ConfirmTeardownAfterExit(r, "meeting", false); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	if err := c.ConfirmTeardownAfterExit(r, "wrong-job", true); !errors.Is(err, ErrJob) {
		t.Fatal(err)
	}
	if err := c.ConfirmTeardownAfterExit(r, "meeting", true); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Phase != Free {
		t.Fatal(got)
	}
}

func TestIncompleteOrContradictoryDeliveryCannotBeErased(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	r := reserve(t, c, b, WSInitial)
	if err := c.ObserveRead(r, ReadObservation{b, ReadClaimed, "meeting", false}); !errors.Is(err, ErrObservation) {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Job != "meeting" || got.ReadSettled {
		t.Fatal(got)
	}
	if err := c.HandlerExited(r, "meeting"); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfirmTeardownAfterExit(r, "meeting", true); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	if err := c.ObserveRead(r, ReadObservation{b, ReadEmpty, "", true}); !errors.Is(err, ErrObservation) {
		t.Fatal(err)
	}
	if err := c.ConfirmTeardownAfterExit(r, "meeting", true); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
}

func TestStaleAndForeignReservationsCannotReleaseNewExecution(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	old := claim(t, c, b, APIRead, "same-job")
	if err := c.HandlerExited(old, "same-job"); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfirmTeardownAfterExit(old, "same-job", true); err != nil {
		t.Fatal(err)
	}
	current := claim(t, c, b, WSReconnect, "same-job")
	foreignC := reconciled(t, b)
	// Align the serial as well as the binding/job. Only the owning Capacity
	// pointer distinguishes this other instance's otherwise identical receipt.
	foreignEmpty := reserve(t, foreignC, b, APIRead)
	if err := foreignC.ObserveRead(foreignEmpty, ReadObservation{b, ReadEmpty, "", true}); err != nil {
		t.Fatal(err)
	}
	foreign := claim(t, foreignC, b, APIRead, "same-job")
	if foreign.serial != current.serial {
		t.Fatal("fixture must isolate the reservation-owner fence")
	}
	for _, invalid := range []Reservation{old, foreign, {}} {
		if err := c.HandlerExited(invalid, "same-job"); !errors.Is(err, ErrReservation) {
			t.Fatal(err)
		}
		if err := c.ConfirmTeardownAfterExit(invalid, "same-job", true); !errors.Is(err, ErrReservation) {
			t.Fatal(err)
		}
	}
	requireOccupied(t, c, b)
	if err := c.HandlerExited(current, "same-job"); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfirmTeardownAfterExit(current, "same-job", true); err != nil {
		t.Fatal(err)
	}
}

func TestImpossibleLateClaimPoisonsCurrentCapacity(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	old := reserve(t, c, b, APIRead)
	if err := c.ObserveRead(old, ReadObservation{b, ReadEmpty, "", true}); err != nil {
		t.Fatal(err)
	}
	current := claim(t, c, b, APIWake, "current-job")
	if err := c.ObserveRead(old, ReadObservation{b, ReadClaimed, "unexpected-old-job", true}); !errors.Is(err, ErrReservation) {
		t.Fatal(err)
	}
	if err := c.HandlerExited(current, "current-job"); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfirmTeardownAfterExit(current, "current-job", true); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
}

func TestRacingWakePollReclaimAndSubscriptionShareOneReservation(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	q := &queue{jobs: []string{"one-meeting"}, owners: make(map[string]string)}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for _, path := range paths {
		wg.Add(1)
		go func(path Path) {
			defer wg.Done()
			<-start
			_, job, err := consume(c, b, path, q)
			if err == nil {
				if job != "one-meeting" {
					t.Errorf("winner job %q", job)
				}
				mu.Lock()
				winners++
				mu.Unlock()
			} else if !errors.Is(err, ErrBusy) {
				t.Errorf("racer: %v", err)
			}
		}(path)
	}
	close(start)
	wg.Wait()
	if winners != 1 || q.claims != 1 || q.attempts != 1 {
		t.Fatalf("winners=%d effects=%+v", winners, q)
	}
	requireOccupied(t, c, b)
}

func TestSerialCannotWrapAndReissueAnOldReservation(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	c.serial = ^uint64(0)
	if _, err := c.BeforeClaim(unit(b, APIRead)); !errors.Is(err, ErrSerial) {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Phase != Quarantined || !got.Poisoned {
		t.Fatal(got)
	}
	requireOccupied(t, c, b)
}

func TestUncooperativeHandlerOutlivesDeadlineWrapper(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	r := claim(t, c, b, APIRead, "uncooperative-meeting")
	started, allowExit, actualExit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(allowExit) }) }
	t.Cleanup(func() { release(); <-actualExit })
	go func() {
		close(started)
		<-allowExit
		close(actualExit)
	}()
	<-started
	// Fake deadline return does not unblock the actual executor.
	wrapperReturned := make(chan struct{})
	close(wrapperReturned)
	<-wrapperReturned
	select {
	case <-actualExit:
		t.Fatal("fixture executor exited before release")
	default:
	}
	if err := c.ConfirmTeardownAfterExit(r, "uncooperative-meeting", true); !errors.Is(err, ErrHandlerLive) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	release()
	<-actualExit
	if err := c.HandlerExited(r, "uncooperative-meeting"); err != nil {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	if err := c.ConfirmTeardownAfterExit(r, "uncooperative-meeting", true); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledCallerDoesNotSettleABlockedSourceRead(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	r := reserve(t, c, b, RedisRead)
	serial := c.Snapshot().Serial
	q := &queue{jobs: []string{"late-meeting"}, owners: make(map[string]string)}
	allowClaim, result := make(chan struct{}), make(chan string, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(allowClaim) }) }
	t.Cleanup(unblock)
	go func() { <-allowClaim; result <- q.consume(b.Node) }()
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(caller.Err(), context.Canceled) {
		t.Fatal("fixture caller did not cancel")
	}
	if err := c.ObserveRead(r, ReadObservation{b, ReadUncertain, "", false}); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
	unblock()
	job := <-result
	if err := c.ObserveRead(r, ReadObservation{b, ReadClaimed, job, true}); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Job != "late-meeting" || got.Serial != serial {
		t.Fatal(got)
	}
	if q.claims != 1 || q.attempts != 1 || q.ack != 0 || q.nack != 0 {
		t.Fatal(q)
	}
	requireOccupied(t, c, b)
}

func TestMalformedClaimIsNotAnEmptyRead(t *testing.T) {
	b := binding("node-a")
	c := reconciled(t, b)
	r := reserve(t, c, b, WSInitial)
	if err := c.ObserveRead(r, ReadObservation{b, ReadClaimed, "", true}); !errors.Is(err, ErrObservation) {
		t.Fatal(err)
	}
	if err := c.ObserveRead(r, ReadObservation{b, ReadEmpty, "", true}); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	requireOccupied(t, c, b)
}
