package containersession

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

func TestDecideTable(t *testing.T) {
	base := fixtureDecl(t)
	disabled := base
	disabled.Enabled = false
	asleepBoot := base
	rt := *base.Runtime
	rt.OnBoot = "asleep"
	asleepBoot.Runtime = &rt
	host := base
	host.Runtime = nil
	rec := Observed{Exists: true, HasRecord: true}
	with := func(state string, code int) Observed {
		o := rec
		o.State, o.ExitCode = state, code
		return o
	}
	cases := []struct {
		name   string
		d      declaredsession.Declaration
		obs    Observed
		volume bool
		wake   bool
		reason string
	}{
		{"host runtime", host, Observed{}, false, false, "not_container_runtime"},
		{"disabled", disabled, with("exited", 137), true, false, "disabled"},
		{"on_boot asleep", asleepBoot, with("exited", 137), true, false, "on_boot_asleep"},
		{"foreign", base, Observed{Exists: true, State: "exited", ExitCode: 1}, true, false, "foreign_container"},
		{"running", base, with("running", 0), true, false, "already_running"},
		{"killed", base, with("exited", 137), true, true, "interrupted"},
		{"holder signalled", base, with("exited", 1), true, true, "interrupted"},
		{"never ran", base, with("created", 0), true, true, "interrupted"},
		{"dead", base, with("dead", 0), true, true, "interrupted"},
		{"clean", base, with("exited", 0), true, false, "clean_exit"},
		{"paused", base, with("paused", 0), true, false, "transitional_state"},
		{"slept", base, Observed{}, true, false, "asleep"},
		{"fresh", base, Observed{}, false, false, "never_started"},
	}
	for _, c := range cases {
		got := Decide(c.d, c.obs, c.volume, false)
		if got.Wake != c.wake || got.Reason != c.reason {
			t.Errorf("%s: got %+v want wake=%v reason=%s", c.name, got, c.wake, c.reason)
		}
	}
}

func TestDecideSleepRequestAndOnBootResume(t *testing.T) {
	d := fixtureDecl(t) // on_boot: resume spelled out
	killed := Observed{Exists: true, HasRecord: true, State: "exited", ExitCode: 137}
	if got := Decide(d, killed, true, true); got.Wake || got.Reason != "sleep_requested" {
		t.Errorf("sleep request = %+v", got)
	}
	if got := Decide(d, killed, true, false); !got.Wake {
		t.Errorf("on_boot resume must honor the interruption: %+v", got)
	}
	// Unset on_boot behaves exactly like "resume".
	d.Runtime = &declaredsession.RuntimeSpec{Kind: "container", Image: d.Runtime.Image}
	if got := Decide(d, killed, true, false); !got.Wake {
		t.Errorf("default on_boot = %+v", got)
	}
}
