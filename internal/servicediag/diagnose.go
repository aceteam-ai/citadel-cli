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

// synthesize combines every signal in a Report into a single most-likely
// cause + suggested next action. Priority reflects how directly each signal
// explains "the service is down": a failed required-env guard would keep
// compose from ever creating the container, so it outranks a log-derived
// root error; VRAM insufficiency is checked next; the log root error/hints
// are the fallback signal for a container that started and then died; an
// absent container with none of the above is reported honestly as "unknown
// why", not force-fit into one of the other buckets.
func synthesize(r Report) (cause, action string) {
	for _, c := range r.Checks {
		if strings.HasPrefix(c.Name, "required_env:") && c.Verdict == VerdictFail {
			return "a required compose variable is not set: " + c.Detail,
				"set the missing variable in the node's environment (or the service's sibling .env file) and retry"
		}
	}

	vram := findCheck(r.Checks, VRAMFitCheckName)
	if vram != nil && vram.Verdict == VerdictFail {
		cause = "insufficient free VRAM to start this service: " + vram.Detail
		action = "free VRAM by stopping another service (see 'citadel services') or reduce this service's footprint, then retry"
		if r.Logs.RootError != "" {
			cause += "; log tail also shows: " + r.Logs.RootError
		}
		return cause, action
	}

	if r.Logs.RootError != "" {
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
		return "service is running; no obvious problem detected",
			"see 'citadel services' for live usage/idle status"
	}

	return fmt.Sprintf("container is present but not running (status: %s, exit code %d)", r.Container.Status, r.Container.ExitCode),
		"see the log tail above for details, or re-run the service start"
}
