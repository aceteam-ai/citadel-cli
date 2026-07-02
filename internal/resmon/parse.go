package resmon

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// parseComputeApps parses nvidia-smi `--query-compute-apps=pid,used_memory`
// CSV output (units stripped, so used_memory is in MiB) into a pid → vram-bytes
// map. When two rows share a pid (a multi-GPU process), their VRAM is summed.
// Unparseable rows are skipped. Mirrors the #421 parser so both features report
// VRAM identically.
func parseComputeApps(out string) map[int]uint64 {
	res := map[int]uint64{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		mib, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil || mib < 0 {
			continue
		}
		res[pid] += uint64(mib * (1 << 20))
	}
	return res
}

// parseGPUTotals parses nvidia-smi
// `--query-gpu=memory.used,memory.total,utilization.gpu` CSV (units stripped:
// memory in MiB, util in %). It sums used/total across devices (whole-node
// totals) and returns the max utilization across devices, so a busy GPU is not
// diluted by an idle second card. Returns util=-1 when no row parses.
func parseGPUTotals(out string) (used, total uint64, util float64) {
	util = -1
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		if u, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err == nil && u >= 0 {
			used += uint64(u * (1 << 20))
		}
		if t, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil && t >= 0 {
			total += uint64(t * (1 << 20))
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil && v > util {
			util = v
		}
	}
	return used, total, util
}

// parsePsOutput parses tab-separated `<engine> ps --format {{.ID}}\t{{.Names}}`
// output into a container-id → name map. Both the full (untruncated) id and a
// 12-char short id are indexed, because /proc/<pid>/cgroup may reference either
// form depending on the runtime. Unparseable lines are skipped.
func parsePsOutput(out string) map[string]string {
	res := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		id := strings.TrimSpace(fields[0])
		name := strings.TrimSpace(fields[1])
		if id == "" || name == "" {
			continue
		}
		// A compose/podman container may list several names comma-separated;
		// take the first for the label.
		if i := strings.Index(name, ","); i >= 0 {
			name = strings.TrimSpace(name[:i])
		}
		res[id] = name
		if len(id) >= 12 {
			res[id[:12]] = name
		}
	}
	return res
}

// cgroupIDRe matches a 64-hex (or 12+-hex) container id embedded in a
// /proc/<pid>/cgroup line. Docker/podman cgroup paths take several shapes:
//
//	.../docker-<64hex>.scope
//	.../docker/<64hex>
//	.../libpod-<64hex>.scope
//	.../crio-<64hex>.scope
//
// so we match the longest hex run of >= 12 chars anywhere in the text.
var cgroupIDRe = regexp.MustCompile(`[0-9a-f]{12,64}`)

// containerIDFromCgroup extracts the container id from raw /proc/<pid>/cgroup
// contents, or "" when no id-shaped token is present (a bare host process).
func containerIDFromCgroup(cgroup string) string {
	if cgroup == "" {
		return ""
	}
	// Prefer the longest hex token so a short parent-slice id doesn't shadow the
	// full container id on the same line.
	best := ""
	for _, m := range cgroupIDRe.FindAllString(cgroup, -1) {
		if len(m) > len(best) {
			best = m
		}
	}
	return best
}

// classifyOwner maps a compute pid to its owner given the pid's raw cgroup
// contents, the container-id → name map (from one `ps` call), the pid's comm
// (host process name fallback), and the set of manifest-declared managed service
// names. It is the pure crux of #427's "who owns this GPU process" question and
// is unit-tested directly.
//
// Resolution order:
//  1. cgroup names a container we can resolve → managed if the name starts with
//     "citadel-" OR is a manifest-declared service name, else container:<name>.
//  2. cgroup names an unresolvable container id → container:<short-id> (still a
//     container, we just can't name it).
//  3. no container id in the cgroup → host:<comm>.
//
// managedNames may be nil (server/job paths, which don't read the manifest); the
// "citadel-" prefix is the reliable marker for the compose-deployed default.
func classifyOwner(cgroup string, idToName map[string]string, comm string, managedNames map[string]struct{}) (owner string, kind OwnerKind) {
	id := containerIDFromCgroup(cgroup)
	if id == "" {
		return "host:" + fallbackComm(comm), OwnerHost
	}
	name := lookupContainerName(id, idToName)
	if name == "" {
		// A container we can't name (daemon race, different runtime). It is still
		// NOT citadel-managed, so it stays a reclaimable candidate.
		short := id
		if len(short) > 12 {
			short = short[:12]
		}
		return "container:" + short, OwnerContainer
	}
	if isManagedName(name, managedNames) {
		return "citadel-managed", OwnerCitadelManaged
	}
	return "container:" + name, OwnerContainer
}

// lookupContainerName resolves a cgroup container id against the ps map, trying
// the full id then the 12-char short id.
func lookupContainerName(id string, idToName map[string]string) string {
	if name, ok := idToName[id]; ok {
		return name
	}
	if len(id) >= 12 {
		if name, ok := idToName[id[:12]]; ok {
			return name
		}
	}
	return ""
}

// isManagedName reports whether a container name is citadel-managed: either the
// compose-deployed "citadel-<name>" prefix, or an exact match against a
// manifest-declared service name (managed services may run under a bare name;
// see internal/status.serviceContainerNames, which probes both). The prefix is
// the reliable default when no manifest names are available.
func isManagedName(name string, managedNames map[string]struct{}) bool {
	if strings.HasPrefix(name, "citadel-") {
		return true
	}
	if managedNames != nil {
		if _, ok := managedNames[name]; ok {
			return true
		}
		// Also match the bare form when the container carries the "citadel-"
		// prefix but the manifest lists the bare name (belt and suspenders).
		if _, ok := managedNames[strings.TrimPrefix(name, "citadel-")]; ok {
			return true
		}
	}
	return false
}

// fallbackComm returns the host process comm, or "unknown" when the comm read
// failed, so a host owner never renders as a bare "host:".
func fallbackComm(comm string) string {
	comm = strings.TrimSpace(comm)
	if comm == "" {
		return "unknown"
	}
	return comm
}

// readProcFile reads /proc/<pid>/<name>, returning "" on any error (non-Linux,
// pid exited, permission denied) so callers degrade to host classification.
func readProcFile(pid int, name string) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name))
	if err != nil {
		return ""
	}
	return string(data)
}

// readProcComm reads the trimmed process command name from /proc/<pid>/comm.
func readProcComm(pid int) string {
	return strings.TrimSpace(readProcFile(pid, "comm"))
}

// readProcRSS reads resident set size in bytes from /proc/<pid>/statm. statm
// fields are page counts; field 2 (index 1) is resident pages. Returns 0 on any
// error or unparseable content.
func readProcRSS(pid int) uint64 {
	return parseStatmRSS(readProcFile(pid, "statm"))
}

// pageSize is the memory page size used to convert /proc/<pid>/statm page counts
// to bytes. os.Getpagesize is used at package init so tests remain deterministic
// on any platform.
var pageSize = uint64(os.Getpagesize())

// parseStatmRSS parses the resident-pages field (field 2) of /proc/<pid>/statm
// and converts it to bytes. Split out from readProcRSS so it is unit-testable.
func parseStatmRSS(statm string) uint64 {
	fields := strings.Fields(statm)
	if len(fields) < 2 {
		return 0
	}
	rssPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return rssPages * pageSize
}
