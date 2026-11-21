package platform

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

func suspendScopesFixture(t *testing.T) (*suspendScopeStop, [2]suspendScopeResource) {
	t.Helper()
	var resources [2]suspendScopeResource
	var reapers [2]<-chan struct{}
	for i, role := range []string{"browser", "xvfb"} {
		name := "citadel-cobrowse-cb-ab12-" + role + "-42-1.scope"
		resources[i] = suspendScopeResource{
			scope: procscope.ScopeIdentity{Name: name, InvocationID: strings.Repeat([]string{"a", "b"}[i], 32),
				ControlGroup: "/user.slice/test/" + name, UID: 1000},
			holder: procscope.ProcessIdentity{PID: 40 + i, StartTicks: uint64(100 + i), UID: 1000, BootID: "fake-boot"},
		}
		done := make(chan struct{})
		close(done)
		reapers[i] = done
	}
	s, err := newSuspendScopeStop("cb-ab12", resources, reapers,
		func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s, resources
}

func TestSuspendScopesAdmissionBeforeEffects(t *testing.T) {
	for _, kind := range []string{"session", "scope-name", "role", "other-session", "empty-invocation", "zero-invocation", "bad-invocation", "upper-invocation", "root-uid", "wrong-uid", "pid", "birth", "boot", "relative-group", "unclean-group", "group-name", "group-newline", "same-pid", "same-invocation", "different-user", "different-boot", "nested-groups", "missing-reaper", "missing-stop"} {
		t.Run(kind, func(t *testing.T) {
			s, resources := suspendScopesFixture(t)
			id := "cb-ab12"
			exact := s.exact
			switch kind {
			case "session":
				id = "not a session"
			case "scope-name":
				resources[1].scope.Name = "other.scope"
			case "role":
				resources[1].scope.Name = resources[0].scope.Name
			case "other-session":
				id = "cb-ab13"
			case "empty-invocation":
				resources[1].scope.InvocationID = ""
			case "zero-invocation":
				resources[1].scope.InvocationID = strings.Repeat("0", 32)
			case "bad-invocation":
				resources[1].scope.InvocationID = strings.Repeat("z", 32)
			case "upper-invocation":
				resources[1].scope.InvocationID = strings.Repeat("B", 32)
			case "root-uid":
				resources[1].scope.UID = 0
			case "wrong-uid":
				resources[1].holder.UID++
			case "pid":
				resources[1].holder.PID = 0
			case "birth":
				resources[1].holder.StartTicks = 0
			case "boot":
				resources[1].holder.BootID = ""
			case "relative-group":
				resources[1].scope.ControlGroup = "relative/" + resources[1].scope.Name
			case "unclean-group":
				resources[1].scope.ControlGroup = "/parent/../" + resources[1].scope.Name
			case "group-name":
				resources[1].scope.ControlGroup = "/other.scope"
			case "group-newline":
				resources[1].scope.ControlGroup = "/bad\n/" + resources[1].scope.Name
			case "same-pid":
				resources[1].holder.PID = resources[0].holder.PID
			case "same-invocation":
				resources[1].scope.InvocationID = resources[0].scope.InvocationID
			case "different-user":
				resources[1].scope.UID++
				resources[1].holder.UID++
			case "different-boot":
				resources[1].holder.BootID += "-other"
			case "nested-groups":
				resources[1].scope.ControlGroup = resources[0].scope.ControlGroup + "/" + resources[1].scope.Name
			case "missing-reaper":
				s.reapers[1] = nil
			case "missing-stop":
				exact = nil
			}
			if _, err := newSuspendScopeStop(id, resources, s.reapers, exact); err == nil || strings.Contains(err.Error(), "fake-") {
				t.Fatal("invalid paired scope plan admitted or detail leaked")
			}
		})
	}
}

func TestSuspendScopesRequireBothExactProofsAndReapers(t *testing.T) {
	s, resources := suspendScopesFixture(t)
	// Constructor input changes must not retarget a previously prepared proof.
	original := resources
	resources[0].scope.InvocationID = strings.Repeat("c", 32)
	resources[0].holder.StartTicks++
	var calls []int
	s.exact = func(ctx context.Context, scope procscope.ScopeIdentity, holder procscope.ProcessIdentity) error {
		i := len(calls) % 2
		if ctx.Err() != nil || scope != original[i].scope || holder != original[i].holder {
			t.Fatal("exact scope or process birth substituted")
		}
		calls = append(calls, i)
		return nil
	}
	for range 2 {
		if err := s.stopForSuspend(suspendPagesContext(t), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(calls, []int{0, 1, 0, 1}) || len(s.gate) != 0 {
		t.Fatal("already reaped roots bypassed proof, retry cached proof, or gate leaked", calls)
	}
}

func TestSuspendScopesComposeWithGracefulResourceStop(t *testing.T) {
	for _, failDisplay := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "display-unknown"}[failDisplay], func(t *testing.T) {
			s, resources := suspendScopesFixture(t)
			browserDone, displayDone := make(chan struct{}), make(chan struct{})
			s.reapers = [2]<-chan struct{}{browserDone, displayDone}
			proc := &cobrowseProc{browserID: "fake-browser", debugPort: 12345,
				browserPID: resources[0].holder.PID, xvfbPID: resources[1].holder.PID,
				browserScope: resources[0].scope.Name, xvfbScope: resources[1].scope.Name,
				scopeUser: true, exited: browserDone}
			legacy := 0
			proc.stop = func() error { legacy++; return nil }
			fence := func() error { return nil }
			proc.stopForSuspend = func(ctx context.Context) error { return s.stopForSuspend(ctx, fence) }
			var order []string
			s.exact = func(ctx context.Context, scope procscope.ScopeIdentity, holder procscope.ProcessIdentity) error {
				if scope == resources[0].scope && holder == resources[0].holder {
					order = append(order, "browser-proof")
					return nil
				}
				if scope != resources[1].scope || holder != resources[1].holder {
					t.Fatal("display proof used another incarnation")
				}
				order = append(order, "display-proof")
				if failDisplay {
					return errors.New("private-display-detail")
				}
				close(displayDone)
				return nil
			}
			closeRoot := func(context.Context, suspendBrowserClosePlan, func() error) error {
				order = append(order, "graceful")
				close(browserDone)
				return nil
			}
			err := stopSuspendProcWith(suspendPagesContext(t), proc, fence, closeRoot)
			if (err != nil) != failDisplay || legacy != 0 ||
				!reflect.DeepEqual(order, []string{"graceful", "browser-proof", "display-proof"}) {
				t.Fatal("graceful root exit replaced whole-resource proof", err, order)
			}
		})
	}
}

func TestSuspendScopesFailuresAreRetryableAndOpaque(t *testing.T) {
	for _, kind := range []string{"nil", "unbounded", "cancelled", "nil-fence", "stale", "first-error", "second-error", "cancel-after-first", "stale-after-first", "stale-after-second", "browser-unreaped", "display-unreaped"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := suspendScopesFixture(t)
			originalReapers, originalExact := s.reapers, s.exact
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			defer cancel()
			calls, stale := 0, false
			fence := func() error {
				if stale {
					return errors.New("private-scope-detail")
				}
				return nil
			}
			s.exact = func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error {
				calls++
				if kind == "first-error" && calls == 1 || kind == "second-error" && calls == 2 {
					return errors.New("private-scope-detail")
				}
				if kind == "cancel-after-first" {
					cancel()
				}
				if kind == "stale-after-first" || kind == "stale-after-second" && calls == 2 {
					stale = true
				}
				return nil
			}
			switch kind {
			case "nil":
				ctx = nil
			case "unbounded":
				ctx = context.Background()
			case "cancelled":
				cancel()
			case "nil-fence":
				fence = nil
			case "stale":
				stale = true
			case "browser-unreaped":
				s.reapers[0] = make(chan struct{})
			case "display-unreaped":
				s.reapers[1] = make(chan struct{})
			}
			if err := s.stopForSuspend(ctx, fence); err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatal("uncertain teardown accepted or details leaked")
			}
			if kind == "nil" || kind == "unbounded" || kind == "cancelled" || kind == "nil-fence" || kind == "stale" {
				if calls != 0 {
					t.Fatal("invalid admission reached an effect")
				}
			}
			if kind == "first-error" || kind == "cancel-after-first" || kind == "stale-after-first" {
				if calls != 1 {
					t.Fatal("uncertain first stop still stopped the display")
				}
			}
			if len(s.gate) != 0 {
				t.Fatal("failed proof leaked exclusion")
			}
			// Failed attempts never install a success receipt or poison the gate.
			s.reapers, s.exact = originalReapers, originalExact
			if err := s.stopForSuspend(suspendPagesContext(t), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSuspendScopesShareBudgetAndSerialize(t *testing.T) {
	s, _ := suspendScopesFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	var active, max atomic.Int32
	s.exact = func(ctx context.Context, _ procscope.ScopeIdentity, _ procscope.ProcessIdentity) error {
		deadline, finite := ctx.Deadline()
		if !finite || deadline != parentDeadline {
			t.Fatal("parent budget extended")
		}
		n := active.Add(1)
		if n > 1 {
			max.Store(n)
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return nil
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.stopForSuspend(ctx, func() error { return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if max.Load() != 0 {
		t.Fatal("exact stop calls overlapped")
	}
	s.gate <- struct{}{}
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer waitCancel()
	if err := s.stopForSuspend(waitCtx, func() error { return nil }); err == nil {
		t.Fatal("gate wait escaped cancellation")
	}
	<-s.gate
	long, longCancel := context.WithTimeout(context.Background(), time.Hour)
	defer longCancel()
	s.exact = func(got context.Context, _ procscope.ScopeIdentity, _ procscope.ProcessIdentity) error {
		deadline, _ := got.Deadline()
		if remaining := time.Until(deadline); remaining <= 0 || remaining > suspendJobProofLimit {
			t.Fatal("total scope stop lacks ceiling")
		}
		return nil
	}
	if err := s.stopForSuspend(long, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
