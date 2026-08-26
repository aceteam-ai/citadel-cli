// internal/servicediag/diagnose.go
package servicediag

import (
	"fmt"
	"strings"
)

const defaultMaxLogLines = 200

// Diagnose wires together container state, log root-cause extraction,
// effective compose command/env, and preflight checks into a single Report.
// insp may be nil (docker unavailable) -- every field it would have fed
// degrades to "unknown"/empty rather than the function erroring or panicking.
//
// This is the reusable core the issue asks for: cmd/service_diagnose.go is
// the only caller today, but any future caller (citadel doctor, citadel
// services) that can produce an Input can call this directly.
func Diagnose(in Input, insp Inspector) Report {
	rep := Report{Service: in.ServiceName}
	rep.Managed, rep.ManagedSource = IsManaged(in.ServiceName, in.ManifestServiceNames)

	maxLines := in.MaxLogLines
	if maxLines <= 0 {
		maxLines = defaultMaxLogLines
	}

	rep.Container = inspectContainer(in.ContainerName, insp)
	rep.Logs = tailLogs(in.ContainerName, maxLines, rep.Container, insp)
	if rep.Logs.Available {
		// Redact BEFORE extracting the root error / hints so both the raw
		// tail (printed verbatim in the LOG TAIL section) and the derived
		// RootError (folded into the synthesized Verdict) are scrubbed --
		// application logs can echo a secret-shaped env value verbatim (an
		// entrypoint echoing its env, a credentialed URL in a stack trace, an
		// auth-failure message embedding the token), with no key name in the
		// log text itself to redact by. See RedactLines/RedactText.
		rep.Logs.Lines = RedactLines(rep.Logs.Lines, in.ResolvedEnv)
		rep.Logs.RootError = ExtractRootError(rep.Logs.Lines)
		rep.Hints = ErrorHints(rep.Logs.Lines)
	}

	rep.Compose = buildComposeInfo(in)

	rep.Checks = requiredVarPreflightChecks(in.ComposeContent, in.ResolvedEnv)
	rep.Checks = append(rep.Checks, VRAMFitCheck(in.FreeVRAMBytes, in.FreeVRAMKnown, in.DeclaredVRAMNeedMB, in.DeclaredVRAMNeedKnown))

	rep.Verdict, rep.NextAction = synthesize(rep)
	return rep
}

// inspectContainer never panics: a nil Inspector or an Inspect error both
// degrade to a ContainerState carrying Error, not a crash.
func inspectContainer(containerName string, insp Inspector) ContainerState {
	if insp == nil {
		return ContainerState{Error: "docker/podman unavailable; container state unknown"}
	}
	cs, err := insp.Inspect(containerName)
	if err != nil {
		return ContainerState{Error: err.Error()}
	}
	return cs
}

// tailLogs only attempts a fetch when the container was actually found --
// there is nothing to tail otherwise, and skipping avoids a guaranteed
// "no such container" error round-trip.
func tailLogs(containerName string, maxLines int, cs ContainerState, insp Inspector) LogTail {
	if insp == nil {
		return LogTail{Error: "docker/podman unavailable; logs unknown"}
	}
	if cs.Error != "" {
		// Container state itself couldn't be determined (daemon unreachable,
		// etc.) -- logs are equally unknown, not "no container".
		return LogTail{Error: "container state unknown: " + cs.Error}
	}
	if !cs.Found {
		return LogTail{}
	}
	lines, err := insp.LogTail(containerName, maxLines)
	if err != nil {
		return LogTail{Error: err.Error()}
	}
	return LogTail{Lines: lines, Available: len(lines) > 0}
}

// requiredVarPreflightChecks converts MissingRequiredVars into
// PreflightChecks, one per distinct ${VAR:?...} guard found in the compose
// content. Nil compose content yields nil checks (nothing to check, not
// "unknown" -- a service whose compose has no guards at all is a fine,
// checkable state).
func requiredVarPreflightChecks(composeContent []byte, env map[string]string) []PreflightCheck {
	if len(composeContent) == 0 {
		return nil
	}
	vars := MissingRequiredVars(string(composeContent), env)
	checks := make([]PreflightCheck, 0, len(vars))
	for _, v := range vars {
		detail := fmt.Sprintf("${%s:?%s}", v.Var, v.Message)
		if v.Verdict == VerdictFail {
			detail = fmt.Sprintf("%s is %s (required: %s)", v.Var, v.Reason, v.Message)
		} else {
			detail = fmt.Sprintf("%s is set (required: %s)", v.Var, v.Message)
		}
		checks = append(checks, PreflightCheck{
			Name:    "required_env:" + v.Var,
			Verdict: v.Verdict,
			Detail:  detail,
		})
	}
	return checks
}

// findCheck returns the check by name, or nil if absent.
func findCheck(checks []PreflightCheck, name string) *PreflightCheck {
	for i := range checks {
		if checks[i].Name == name {
			return &checks[i]
		}
	}
	return nil
}

// hasFailedCheck reports whether any check in checks is VerdictFail.
func hasFailedCheck(checks []PreflightCheck) bool {
	for _, c := range checks {
		if c.Verdict == VerdictFail {
			return true
		}
	}
	return false
}

// synthesize combines every signal in a Report into a single most-likely
// cause + suggested next action. Priority reflects how directly each signal
// explains "the service is down": a failed required-env guard would keep
// compose from ever creating the container, so it outranks a log-derived
// root error; VRAM insufficiency is checked next; the log root error/hints
// are the fallback signal for a container that started and then died; an
// absent container with none of the above is reported honestly as "unknown
// why", not force-fit into one of the other buckets.
//
// A "the container exited because of X" causal claim requires the container
// to actually not be running -- r.Logs.RootError is a heuristic match against
// the log tail (see ExtractRootError) and can just as easily be an old,
// transient, or already-recovered error line (a startup retry, a recovered
// OOM, ...) for a service that is currently healthy and running. Asserting
// "exited" for a running container would directly contradict the CONTAINER
// block's own [RUNNING] status printed right above it, so a running
// container with a matched error line gets an informational verdict instead
// of a causal one.
func synthesize(r Report) (cause, action string) {
	for _, c := range r.Checks {
		if strings.HasPrefix(c.Name, "required_env:") && c.Verdict == VerdictFail {
			return "a required compose variable is not set: " + c.Detail,
				"set the missing variable in the node's environment (or the service's sibling .env file) and retry"
		}
	}

	// Gated on !r.Container.Running for the same reason as the RootError
	// branch below: DeclaredVRAMNeedMB is a coarse "cold start" budget for
	// this engine type (status.EngineVRAMEstimateMB), not a live footprint --
	// it does not know the target service's OWN currently-running instance is
	// part of what's already occupying "free" VRAM. A healthy running
	// service holding most of the GPU's VRAM legitimately fails this check
	// (little free VRAM left), and a top verdict of "insufficient free VRAM
	// to start this service" would be the same false contradiction under a
	// [RUNNING] status line that BLOCK 1 removed from the RootError path.
	// The check itself still shows in PREFLIGHT CHECKS either way.
	vram := findCheck(r.Checks, VRAMFitCheckName)
	if vram != nil && vram.Verdict == VerdictFail && !r.Container.Running {
		cause = "insufficient free VRAM to start this service: " + vram.Detail
		action = "free VRAM by stopping another service (see 'citadel services') or reduce this service's footprint, then retry"
		if r.Logs.RootError != "" {
			cause += "; log tail also shows: " + r.Logs.RootError
		}
		return cause, action
	}

	if r.Logs.RootError != "" && !r.Container.Running {
		cause = "container exited with: " + r.Logs.RootError
		if len(r.Hints) > 0 {
			action = r.Hints[0]
		} else {
			action = "review the log tail above for the full context; no specific pattern matched, so this is a best-effort extraction"
		}
		return cause, action
	}

	if r.Container.Error != "" {
		return "container state could not be determined: " + r.Container.Error,
			"check that docker/podman is installed and the daemon is reachable ('citadel doctor')"
	}

	if !r.Container.Found {
		return "no container found for this service",
			fmt.Sprintf("start it (e.g. 'citadel run --service %s') and re-run diagnose if it fails to come up", r.Service)
	}

	if r.Container.Running {
		if r.Logs.RootError != "" {
			return "service is running; log tail contains an error-looking line: " + r.Logs.RootError,
				"the container has not exited, so this may be a resolved startup retry or otherwise benign -- review the log tail above for context; see 'citadel services' for live usage/idle status"
		}
		// A preflight check (e.g. vram_fit) can fail for a running container
		// without necessarily being wrong -- see the gate above -- but "no
		// obvious problem detected" would be the same false-comfort
		// contradiction under a [FAIL] line in PREFLIGHT CHECKS. Point at it
		// honestly instead of asserting nothing is wrong.
		if hasFailedCheck(r.Checks) {
			return "service is running; a preflight check did not pass (see PREFLIGHT CHECKS above)",
				"the container has not exited, so this may not be an active problem -- review the failed check above for context"
		}
		return "service is running; no obvious problem detected",
			"see 'citadel services' for live usage/idle status"
	}

	return fmt.Sprintf("container is present but not running (status: %s, exit code %d)", r.Container.Status, r.Container.ExitCode),
		"see the log tail above for details, or re-run the service start"
}
