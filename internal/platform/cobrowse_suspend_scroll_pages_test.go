package platform

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

type suspendPageScrollFixture struct {
	base      *suspendRestoredFixture
	positions map[string]suspendTab
	order     []string
	after     func(string)
}

func newSuspendPageScrollFixture(n int) *suspendPageScrollFixture {
	return &suspendPageScrollFixture{base: newSuspendRestoredFixture(n), positions: make(map[string]suspendTab)}
}

func (f *suspendPageScrollFixture) source(t *testing.T, restore bool) suspendRestoredPageSource {
	source := f.base.source(t)
	position := source.position
	source.position = func(ctx context.Context, p pin, tab suspendTab, fence func() error) error {
		// Retain the existing pin/argument and per-command fence assertions.
		if err := position(ctx, p, tab, fence); err != nil {
			return err
		}
		phase := "observe:"
		if restore {
			phase = "restore:"
			f.positions[p.id] = tab
		} else if f.positions[p.id] != tab {
			return errors.New("synthetic position changed")
		}
		step := phase + p.id
		f.order = append(f.order, step)
		if f.after != nil {
			f.after(step)
		}
		return nil
	}
	return source
}

func TestSuspendPageScrollsRestoreThenObserveEntireSet(t *testing.T) {
	for _, n := range []int{2, maxSuspendTabs} {
		f := newSuspendPageScrollFixture(n)
		// Equal URLs do not collapse distinct targets or their scroll positions.
		f.base.plan.manifest.Tabs[1].URL = f.base.plan.manifest.Tabs[0].URL
		f.base.expected[1].URL = f.base.expected[0].URL
		f.base.targets[1].URL = f.base.targets[0].URL
		if err := restoreSuspendPageScrollsWith(suspendPagesContext(t), f.base.plan, f.base.active, f.base.fence,
			f.source(t, true), f.source(t, false)); err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, phase := range []string{"restore:", "observe:"} {
			for _, id := range f.base.plan.targetIDs {
				want = append(want, phase+id)
			}
		}
		if !reflect.DeepEqual(f.order, want) || len(f.positions) != n {
			t.Fatal("whole-manifest restoration omitted, merged or reordered a phase")
		}
	}
}

func TestSuspendPageScrollsCopiesPlanAcrossBothPasses(t *testing.T) {
	for _, mutateAfter := range []string{"first-await", "last-restore"} {
		t.Run(mutateAfter, func(t *testing.T) {
			f := newSuspendPageScrollFixture(2)
			mutate := func() {
				f.base.plan.targetIDs[1] = "replacement-target"
				f.base.plan.manifest.Tabs[1].URL = "https://replacement.invalid"
				f.base.plan.manifest.Tabs[1].ScrollY = 999
			}
			if mutateAfter == "first-await" {
				f.base.after = func(step string) {
					if step == "verify" {
						mutate()
					}
				}
			} else {
				f.after = func(step string) {
					if step == "restore:page-1" {
						mutate()
					}
				}
			}
			if err := restoreSuspendPageScrollsWith(suspendPagesContext(t), f.base.plan, f.base.active, f.base.fence,
				f.source(t, true), f.source(t, false)); err != nil {
				t.Fatal("caller mutation replaced the plan between effects and final observations", err)
			}
			if !reflect.DeepEqual(f.order, []string{"restore:page-0", "restore:page-1", "observe:page-0", "observe:page-1"}) {
				t.Fatal("caller mutation changed pinned target ordering")
			}
		})
	}
}

func TestSuspendPageScrollsFinalObservationDoesNotRepair(t *testing.T) {
	f := newSuspendPageScrollFixture(2)
	f.after = func(step string) {
		if step == "restore:page-1" {
			changed := f.positions["page-0"]
			changed.ScrollY += 25
			f.positions["page-0"] = changed
		}
	}
	if err := restoreSuspendPageScrollsWith(suspendPagesContext(t), f.base.plan, f.base.active, f.base.fence,
		f.source(t, true), f.source(t, false)); err == nil {
		t.Fatal("later restore invalidated an earlier page without a final read-only refusal")
	}
	if !reflect.DeepEqual(f.order, []string{"restore:page-0", "restore:page-1"}) ||
		f.positions["page-0"].ScrollY != f.base.expected[0].ScrollY+25 {
		t.Fatal("final verification repaired a changed page or repeated restoration")
	}
}

func TestSuspendPageScrollsAdmissionBeforeEffects(t *testing.T) {
	for _, kind := range []string{"nil-context", "no-budget", "cancelled", "nil-active", "nil-fence", "bad-manifest", "many-tabs", "aggregate", "missing-target", "duplicate-target", "bad-port", "bad-browser", "restore-verify", "restore-list", "restore-position", "observe-verify", "observe-list", "observe-position"} {
		t.Run(kind, func(t *testing.T) {
			n := 2
			if kind == "many-tabs" {
				n = maxSuspendTabs + 1
			} else if kind == "aggregate" {
				n = maxSuspendTabs
			}
			f := newSuspendPageScrollFixture(n)
			ctx, active, fence := suspendPagesContext(t), f.base.active, f.base.fence
			restore, observe := f.source(t, true), f.source(t, false)
			switch kind {
			case "nil-context":
				ctx = nil
			case "no-budget":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil-active":
				active = nil
			case "nil-fence":
				fence = nil
			case "bad-manifest":
				f.base.plan.manifest.Generation = 0
			case "aggregate":
				for i := range f.base.plan.manifest.Tabs {
					f.base.plan.manifest.Tabs[i].URL = "https://example.invalid/" + strings.Repeat("a", 7000)
				}
			case "missing-target":
				f.base.plan.targetIDs = f.base.plan.targetIDs[:1]
			case "duplicate-target":
				f.base.plan.targetIDs[1] = f.base.plan.targetIDs[0]
			case "bad-port":
				f.base.plan.port = 0
			case "bad-browser":
				f.base.plan.browserID = "../browser"
			case "restore-verify":
				restore.verify = nil
			case "restore-list":
				restore.list = nil
			case "restore-position":
				restore.position = nil
			case "observe-verify":
				observe.verify = nil
			case "observe-list":
				observe.list = nil
			case "observe-position":
				observe.position = nil
			}
			if err := restoreSuspendPageScrollsWith(ctx, f.base.plan, active, fence, restore, observe); err == nil || len(f.order) != 0 || len(f.base.calls) != 0 {
				t.Fatal("invalid restoration crossed a provider/effect boundary")
			}
		})
	}
}

func TestSuspendPageScrollsFailureKeepsPartialEffectsUnacknowledged(t *testing.T) {
	for _, kind := range []string{"restore-error", "observe-error", "cancel-after-restore", "cancel-between-passes", "stale-between-passes", "selection-between-passes", "extra-page-between-passes"} {
		t.Run(kind, func(t *testing.T) {
			f := newSuspendPageScrollFixture(2)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			restore, observe := f.source(t, true), f.source(t, false)
			private := errors.New("private-scroll-provider-sentinel")
			switch kind {
			case "restore-error":
				restore.position = func(context.Context, pin, suspendTab, func() error) error { return private }
			case "observe-error":
				observe.position = func(context.Context, pin, suspendTab, func() error) error { return private }
			default:
				f.after = func(step string) {
					if kind == "cancel-after-restore" && step == "restore:page-0" {
						cancel()
					}
					if step != "restore:page-1" {
						return
					}
					switch kind {
					case "cancel-between-passes":
						cancel()
					case "stale-between-passes":
						f.base.stale = true
					case "selection-between-passes":
						f.base.activeID = "page-0"
					case "extra-page-between-passes":
						f.base.targets = append(f.base.targets, f.base.targets[0])
					}
				}
			}
			if err := restoreSuspendPageScrollsWith(ctx, f.base.plan, f.base.active, f.base.fence, restore, observe); err == nil || strings.Contains(err.Error(), "private-scroll-provider-sentinel") {
				t.Fatal("incomplete restoration acknowledged or exposed provider details")
			}
			for _, step := range f.order {
				if strings.HasPrefix(step, "observe:") {
					t.Fatal("failed transition continued into final observations")
				}
			}
		})
	}
}

func TestSuspendPageScrollsSharesOriginalBudget(t *testing.T) {
	f := newSuspendPageScrollFixture(2)
	ctx := suspendPagesContext(t)
	restore, observe := f.source(t, true), f.source(t, false)
	wrap := func(source *suspendRestoredPageSource) {
		verify, list, position := source.verify, source.list, source.position
		source.verify = func(got context.Context, port int, id string) error {
			if got != ctx {
				t.Error("browser discovery detached or reset the original budget")
			}
			return verify(got, port, id)
		}
		source.list = func(got context.Context, port int) ([]cdpdiscovery.Target, error) {
			if got != ctx {
				t.Error("page discovery detached or reset the original budget")
			}
			return list(got, port)
		}
		source.position = func(got context.Context, p pin, tab suspendTab, fence func() error) error {
			if got != ctx {
				t.Error("page operation detached or reset the original budget")
			}
			return position(got, p, tab, fence)
		}
	}
	wrap(&restore)
	wrap(&observe)
	if err := restoreSuspendPageScrollsWith(ctx, f.base.plan, f.base.active, f.base.fence, restore, observe); err != nil {
		t.Fatal(err)
	}
}
