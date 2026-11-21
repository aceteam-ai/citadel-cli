package containersession

import (
	"context"
	"errors"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

// Decision is the boot reconciler's verdict for one container declaration.
type Decision struct {
	Name   string
	Wake   bool
	Reason string
}

// Decide is the pure wake rule, from what the engine shows alone (no store):
//
//   - on_boot "resume" (the default, also spelled out) honors the intent below;
//     "asleep" never wakes at boot;
//   - an operator sleep request always wins over everything below;
//   - a running container is left alone;
//   - an exited container with exit code 0 is a harness the operator ended, and
//     only an explicit wake undoes that (the holder exits non-zero whenever it
//     is signalled, so a reboot-killed session never looks clean);
//   - any other stopped container (killed by the reboot, crashed) is woken;
//   - no container but a volume is a deliberate sleep (sleep removes the
//     container), and stays asleep;
//   - neither is a session never woken: the first wake is explicit.
//
// A container carrying our name but no valid record is never touched.
func Decide(d declaredsession.Declaration, obs Observed, volume, sleepRequested bool) Decision {
	dec := Decision{Name: d.Name}
	switch {
	case !d.IsContainer():
		dec.Reason = "not_container_runtime"
	case !d.Enabled:
		dec.Reason = "disabled"
	case d.Runtime.OnBoot == "asleep":
		dec.Reason = "on_boot_asleep"
	case sleepRequested:
		// The operator asked this session to sleep (even if the sleep failed
		// midway and left a non-zero exited container).
		dec.Reason = "sleep_requested"
	case obs.Exists && !obs.HasRecord:
		dec.Reason = "foreign_container"
	case obs.Exists:
		switch obs.State {
		case "running":
			dec.Reason = "already_running"
		case "exited":
			if obs.ExitCode == 0 {
				dec.Reason = "clean_exit"
			} else {
				dec.Wake, dec.Reason = true, "interrupted"
			}
		case "created", "dead":
			dec.Wake, dec.Reason = true, "interrupted"
		default:
			dec.Reason = "transitional_state"
		}
	case volume:
		dec.Reason = "asleep"
	default:
		dec.Reason = "never_started"
	}
	return dec
}

// Outcome is one reconciled session's result.
type Outcome struct {
	Decision Decision
	Err      error
}

// Reconcile applies Decide to every container declaration and wakes the ones it
// selects, one at a time. One session's failure never stops the others; an
// observation failure skips that session (fail-closed: an unreadable engine is
// never read as "nothing there").
func (r *Runner) Reconcile(ctx context.Context, snapshot declaredsession.Snapshot) []Outcome {
	var out []Outcome
	for _, d := range snapshot.Sessions {
		if !d.IsContainer() {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		obs, err := r.Observe(ctx, d.Name)
		var volume bool
		if err == nil && !obs.Exists {
			volume, err = r.VolumeExists(ctx, d.Name)
		}
		if err != nil {
			out = append(out, Outcome{Decision: Decision{Name: d.Name, Reason: "observe_failed"}, Err: err})
			continue
		}
		dec := Decide(d, obs, volume, r.sleepRequested(d.Name))
		o := Outcome{Decision: dec}
		if dec.Wake {
			if _, werr := r.Wake(ctx, d); werr != nil && !errors.Is(werr, ErrAlreadyRunning) {
				o.Err = werr
			}
		}
		out = append(out, o)
	}
	return out
}
