// internal/services/external_supervisor.go
package services

import (
	"fmt"
	"strings"
)

// This is the "is a native engine's live process owned by an external
// supervisor" decision, split into a pure core (classifyExternalSupervisor)
// that takes the two cgroup file contents as strings and a platform wrapper
// (externalSupervisorUnit, external_supervisor_{linux,other}.go) that reads
// /proc. The split keeps the parsing testable with fabricated /proc content --
// a test must never read /proc of a real pid, and this dev box runs a live node.
//
// Why it exists: the stock ollama install runs as a host systemd service
// (User=ollama, Restart=always). citadel did not start it, cannot signal it
// (wrong user -> EPERM) and cannot make a kill stick (relaunched in seconds), so
// a stop that signals it is a doomed no-op that still reports success. Detecting
// the external supervisor lets stop report guidance instead (see #1084 for the
// docker analogue: an adopted external engine is a clear no-op, not a failure).

// classifyExternalSupervisor reports whether the target process's cgroup places
// it under a systemd *.service unit that is NOT this process's own unit.
//
// The self-comparison is load-bearing: a native engine that CITADEL started is a
// child of the citadel worker and inherits its cgroup, so from inside a
// systemd-run `citadel work` (self == citadel's own unit) the engine's unit
// equals self's unit and is correctly NOT external. Only a DIFFERENT *.service
// counts. A process owned by a *.scope (a login/transient session, i.e. a
// hand-run binary) or by no unit at all is never external -- scopes are not
// restart-supervised, so citadel can stop such a process itself.
func classifyExternalSupervisor(targetCgroup, selfCgroup string) (unit string, external bool) {
	targetUnit := systemdUnitFromCgroup(targetCgroup)
	if targetUnit == "" {
		return "", false
	}
	if targetUnit == systemdUnitFromCgroup(selfCgroup) {
		return "", false
	}
	return targetUnit, true
}

// systemdUnitFromCgroup returns the owning systemd *.service unit for a process
// given its /proc/<pid>/cgroup contents, or "" when the leaf-most owning cgroup
// is a *.scope (a session/transient scope, not a restart-supervised service) or
// there is no unit at all.
func systemdUnitFromCgroup(content string) string {
	path := systemdCgroupPath(content)
	if path == "" {
		return ""
	}
	if unit, isService := leafOwningUnit(path); isService {
		return unit
	}
	return ""
}

// systemdCgroupPath extracts the cgroup path systemd owns. cgroup v2 has a
// single "0::<path>" line (empty controllers); cgroup v1 has one line per
// controller and the authoritative one is the "name=systemd" hierarchy. Either
// form's line is "hierarchy-ID:controllers:path".
func systemdCgroupPath(content string) string {
	var fallback string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		controllers, path := parts[1], parts[2]
		if controllers == "" || strings.Contains(controllers, "name=systemd") {
			return path
		}
		if fallback == "" {
			fallback = path
		}
	}
	return fallback
}

// leafOwningUnit walks a cgroup path from the leaf toward the root and returns
// the first segment that is a systemd unit: a *.service (isService true) or a
// *.scope (isService false). Walking from the leaf -- not "the deepest .service
// anywhere" -- is what keeps a hand-run process under a graphical terminal
// (.../user@1000.service/app.slice/vte-spawn-x.scope) classified by its OWNING
// scope, not misattributed to the user@<uid>.service manager higher up the tree.
// A delegated sub-cgroup (.../ollama.service/subcg) still resolves to its
// service by walking up past the non-unit leaf.
func leafOwningUnit(path string) (unit string, isService bool) {
	segments := strings.Split(path, "/")
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if strings.HasSuffix(seg, ".service") {
			return seg, true
		}
		if strings.HasSuffix(seg, ".scope") {
			return seg, false
		}
	}
	return "", false
}

// ExternallyManagedGuidance is the operator-facing message for a native engine
// that a host systemd unit owns. Both stop call sites (the SERVICE_STOP job and
// the `citadel stop` CLI) use it so the wording cannot drift. citadel
// deliberately does NOT attempt `sudo systemctl stop` unattended: it did not
// install the unit, has no sudo, and stopping it could disrupt dependents.
func ExternallyManagedGuidance(serviceName, unit string) string {
	ctl := strings.TrimSuffix(unit, ".service")
	return fmt.Sprintf(
		"%s is managed outside citadel by host systemd unit %s; citadel will not stop it.\n"+
			"  To stop it: sudo systemctl stop %s   (add: sudo systemctl disable %s to keep it down)",
		serviceName, unit, ctl, ctl)
}
