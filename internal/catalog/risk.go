package catalog

import "strings"

// Severity ranks a compose risk finding.
type Severity string

const (
	// SeverityCritical marks a directive that grants host-root-equivalent access
	// (container escape). A Critical finding gates the install behind an explicit
	// opt-in (--allow-privileged) that --yes does NOT bypass.
	SeverityCritical Severity = "Critical"
	// SeverityHigh marks a directive that significantly widens the container's
	// access to the host (host namespaces, sensitive bind mounts, device
	// passthrough). High findings are surfaced but do not hard-gate the install.
	SeverityHigh Severity = "High"
)

// ComposeRisk is a single dangerous directive found in a resolved compose file.
type ComposeRisk struct {
	Severity  Severity
	Directive string // the directive name, e.g. "privileged"
	Detail    string // human-readable explanation / matched value
}

// sensitiveHostPaths are host paths whose bind-mount into a container is treated
// as High risk (read/write access to host secrets, sockets, or the whole FS).
var sensitiveHostPaths = []string{"/etc", "/root", "/var/run", "/home", "/usr", "/boot", "/sys", "/proc"}

// ScanComposeRisks scans resolved compose text for dangerous directives and
// returns the findings. It is pure and line/substring based (like
// parseComposeImages) -- intentionally conservative: false positives are
// acceptable, silent misses are not. A nil/empty result means no risks found.
func ScanComposeRisks(compose string) []ComposeRisk {
	var risks []ComposeRisk
	inCapAdd := false // whether we are inside a cap_add: block

	for _, raw := range strings.Split(compose, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)

		// --- Critical: privileged mode ---
		if isPrivilegedTrue(lower) {
			risks = append(risks, ComposeRisk{
				Severity:  SeverityCritical,
				Directive: "privileged: true",
				Detail:    "runs the container in privileged mode (full host device + capability access; container escape to host root)",
			})
		}

		// --- Critical: docker socket bind mount ---
		if strings.Contains(line, "/var/run/docker.sock") || strings.Contains(line, "/run/docker.sock") {
			risks = append(risks, ComposeRisk{
				Severity:  SeverityCritical,
				Directive: "docker.sock mount",
				Detail:    "mounts the Docker socket (control of the Docker daemon = root on the host)",
			})
		}

		// --- cap_add tracking (Critical for ALL / SYS_ADMIN) ---
		// Track whether we are inside a cap_add: block so a bare "- ALL" item is
		// only flagged in that context (avoids matching unrelated "ALL" text).
		if strings.HasPrefix(lower, "cap_add:") {
			inCapAdd = true
			// Inline form: "cap_add: [ALL]" or "cap_add: [SYS_ADMIN]".
			if cap := capFromInline(line); cap != "" {
				if r, ok := capRisk(cap); ok {
					risks = append(risks, r)
				}
			}
			continue
		}
		if inCapAdd {
			if item, ok := listItem(line); ok {
				if r, ok := capRisk(item); ok {
					risks = append(risks, r)
				}
				continue
			}
			// A non-list line ends the cap_add block.
			inCapAdd = false
		}

		// --- High: host namespaces ---
		if r, ok := hostNamespaceRisk(lower); ok {
			risks = append(risks, r)
		}

		// --- High: device passthrough (the devices: key) ---
		// Module compose rarely needs host devices, and passthrough widens host
		// access, so we flag the devices: directive whenever it appears.
		if strings.HasPrefix(lower, "devices:") {
			risks = append(risks, ComposeRisk{
				Severity:  SeverityHigh,
				Directive: "devices",
				Detail:    "passes host devices into the container",
			})
		}

		// --- High: sensitive host bind mount (a volumes list item) ---
		if r, ok := bindMountRisk(line); ok {
			risks = append(risks, r)
		}
	}

	return risks
}

// isPrivilegedTrue reports whether a line sets privileged: true.
func isPrivilegedTrue(lower string) bool {
	if !strings.HasPrefix(lower, "privileged:") {
		return false
	}
	val := strings.TrimSpace(strings.TrimPrefix(lower, "privileged:"))
	return val == "true" || val == "yes"
}

// capFromInline extracts a capability from an inline "cap_add: [SYS_ADMIN]" form.
func capFromInline(line string) string {
	idx := strings.Index(line, "[")
	if idx < 0 {
		return ""
	}
	inner := line[idx+1:]
	inner = strings.TrimSuffix(strings.TrimSpace(inner), "]")
	inner = strings.Trim(inner, "[] ")
	// Take the first element if comma-separated.
	if c := strings.SplitN(inner, ",", 2); len(c) > 0 {
		return strings.Trim(strings.TrimSpace(c[0]), `"'`)
	}
	return ""
}

// listItem returns the value of a YAML list item line ("- VALUE"), if it is one.
func listItem(line string) (string, bool) {
	if !strings.HasPrefix(line, "-") {
		return "", false
	}
	val := strings.TrimSpace(strings.TrimPrefix(line, "-"))
	val = strings.Trim(val, `"'`)
	if val == "" {
		return "", false
	}
	return val, true
}

// capRisk returns a Critical risk if cap is a dangerous Linux capability.
func capRisk(cap string) (ComposeRisk, bool) {
	upper := strings.ToUpper(strings.TrimSpace(cap))
	switch upper {
	case "ALL":
		return ComposeRisk{
			Severity:  SeverityCritical,
			Directive: "cap_add: ALL",
			Detail:    "grants ALL Linux capabilities (effectively privileged; container escape)",
		}, true
	case "SYS_ADMIN":
		return ComposeRisk{
			Severity:  SeverityCritical,
			Directive: "cap_add: SYS_ADMIN",
			Detail:    "grants CAP_SYS_ADMIN (mount, namespace, and many escape primitives)",
		}, true
	}
	return ComposeRisk{}, false
}

// hostNamespaceRisk flags host network/pid/ipc namespace sharing.
func hostNamespaceRisk(lower string) (ComposeRisk, bool) {
	switch {
	case strings.HasPrefix(lower, "network_mode:") && strings.Contains(lower, "host"):
		return ComposeRisk{SeverityHigh, "network_mode: host", "shares the host network namespace (binds host ports, sees host traffic)"}, true
	case strings.HasPrefix(lower, "pid:") && strings.Contains(lower, "host"):
		return ComposeRisk{SeverityHigh, "pid: host", "shares the host PID namespace (sees and can signal host processes)"}, true
	case strings.HasPrefix(lower, "ipc:") && strings.Contains(lower, "host"):
		return ComposeRisk{SeverityHigh, "ipc: host", "shares the host IPC namespace"}, true
	}
	return ComposeRisk{}, false
}

// bindMountRisk flags a volumes list item that bind-mounts a sensitive host path.
// It ignores named volumes (left side is not a path). Returns the finding and
// true if risky.
func bindMountRisk(line string) (ComposeRisk, bool) {
	item, ok := listItem(line)
	if !ok {
		return ComposeRisk{}, false
	}
	// A bind mount is "HOST:CONTAINER[:opts]"; named volumes are "name:CONTAINER".
	parts := strings.SplitN(item, ":", 2)
	if len(parts) < 2 {
		return ComposeRisk{}, false
	}
	host := strings.TrimSpace(parts[0])

	// Only a path-like left side is a host bind mount.
	if !looksLikeHostPath(host) {
		return ComposeRisk{}, false
	}

	if isSensitiveHostPath(host) {
		return ComposeRisk{
			Severity:  SeverityHigh,
			Directive: "volumes (host bind mount)",
			Detail:    "bind-mounts a sensitive host path: " + host,
		}, true
	}
	return ComposeRisk{}, false
}

// looksLikeHostPath reports whether s is a filesystem path (vs a named volume).
func looksLikeHostPath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".")
}

// isSensitiveHostPath reports whether host is the root, a home shortcut, or a
// sensitive system path (boundary-aware: "/etc" matches "/etc" and "/etc/x" but
// not "/etcetera").
func isSensitiveHostPath(host string) bool {
	if host == "/" {
		return true
	}
	if host == "~" || strings.HasPrefix(host, "~/") {
		return true
	}
	for _, p := range sensitiveHostPaths {
		if host == p || strings.HasPrefix(host, p+"/") {
			return true
		}
	}
	return false
}

// criticalRisks returns the subset of risks at Critical severity. Pure, so the
// install gate decision is table-testable.
func criticalRisks(risks []ComposeRisk) []ComposeRisk {
	var out []ComposeRisk
	for _, r := range risks {
		if r.Severity == SeverityCritical {
			out = append(out, r)
		}
	}
	return out
}

// criticalDirectives returns the directive names of the Critical findings, for
// the refusal message.
func criticalDirectives(risks []ComposeRisk) []string {
	var out []string
	for _, r := range criticalRisks(risks) {
		out = append(out, r.Directive)
	}
	return out
}
