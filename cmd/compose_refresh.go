// cmd/compose_refresh.go
//
// Boot-time re-materialization of citadel-owned embedded compose files (#426).
//
// On `citadel work` startup, BEFORE managed services are started, we run a
// version-gated sweep that rewrites the on-disk copies of the embedded service
// composes (services.ServiceMap) when the running binary differs from the one
// that last materialized them. This is what carries template changes -- the
// #405/#410 host-port fix, image tags, healthchecks, GPU stanzas -- to nodes
// that were provisioned by an older binary and would otherwise keep stale,
// hardcoded-port composes forever.
//
// Recreate-on-boot policy (see PR for rationale): the primary upgrade path is a
// hands-off auto-update that re-execs the binary in place (syscall.Exec) WITHOUT
// tearing down service containers, so the prior container keeps running on its
// old host port and startManagedServices no-ops on it (it's already running).
// Refreshing only the FILE would therefore never self-heal an auto-updated node
// -- exactly the population #426 targets. So auto force-recreate on a *port
// change* is the DEFAULT: it fires only when a managed service is running AND
// its published host port actually differs from the new template (i.e. the
// broken/colliding state, not healthy inference). Operators who prefer to move
// live containers by hand can opt out with
// CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE=1|true|yes, which downgrades to
// file-refresh + a remediation hint.
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/services"
)

// refreshManagedComposeFiles runs the version-gated compose refresh sweep for
// the given node config directory. It is safe to call on every boot: when the
// binary version is unchanged it is a cheap no-op. Failures are logged and never
// abort startup -- a refresh problem must not stop the node from serving jobs.
func refreshManagedComposeFiles(configDir string) {
	if configDir == "" {
		return
	}
	servicesDir := filepath.Join(configDir, "services")

	var recreator composerefresh.PortRecreator
	if recreateOnUpgradeEnabled() {
		recreator = enginePortRecreator
	}

	res, err := composerefresh.Sweep(composerefresh.Options{
		ServicesDir:           servicesDir,
		Version:               Version,
		Embedded:              services.ServiceMap,
		PortManaged:           services.ServiceHostPorts,
		KnownHistoricalHashes: services.KnownComposeHashes,
		Recreator:             recreator,
		Log:                   func(format string, args ...any) { Log(format, args...) },
	})
	if err != nil {
		Log("compose-refresh: sweep error: %v", err)
		return
	}
	if res.Skipped {
		Debug("compose-refresh: binary version unchanged (%s); no sweep", Version)
		return
	}
	if len(res.Refreshed) > 0 {
		Log("compose-refresh: refreshed citadel-owned composes: %s", strings.Join(res.Refreshed, ", "))
	}
	if len(res.Preserved) > 0 {
		Log("compose-refresh: preserved hand-edited composes: %s", strings.Join(res.Preserved, ", "))
	}
	if len(res.Recreated) > 0 {
		Log("compose-refresh: force-recreated (host port moved): %s", strings.Join(res.Recreated, ", "))
	}
}

// recreateOnUpgradeEnabled reports whether boot-time auto force-recreate of a
// port-moved service is active. It is ON by default because the primary upgrade
// path (auto-update re-exec) leaves prior containers running on their old host
// port, so file-refresh alone would never self-heal the collision #426 targets.
// Operators can opt out with CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE.
func recreateOnUpgradeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE"))) {
	case "1", "true", "yes", "on":
		return false
	default:
		return true
	}
}

// enginePortRecreator inspects the running container's published host port and,
// if it differs from wantHostPort, force-recreates the service from composePath
// with the citadel host-port env injected. Returns (recreated, err). When the
// container is not running or already publishes the wanted port, it is left
// untouched (recreated=false).
func enginePortRecreator(service, composePath string, wantHostPort int) (bool, error) {
	rt := catalog.SelectContainerRuntime()
	// citadel#860: this sweep only ever runs from citadel work's boot path
	// (refreshManagedComposeFiles, cmd/work.go), which refuses --node-dir/
	// CITADEL_NODE_DIR outright before reaching here -- so this is a
	// defensive mirror, not a live gap today. Kept consistent with
	// containerIsRunning/embeddedContainerName so it can't silently disagree
	// with what ensureComposeFile just materialized if that refusal is ever
	// narrowed.
	containerName := "citadel-" + service
	if isEmbeddedService(service) {
		containerName = embeddedContainerName(service)
	}

	current, running := runningPublishedHostPort(rt.EngineBin, containerName)
	if !running {
		// Nothing running from the old file; the refreshed file will be used on
		// the next start. Don't disturb anything.
		return false, nil
	}
	if current == wantHostPort {
		return false, nil
	}

	Log("compose-refresh: %s: host port moved %d -> %d; force-recreating", service, current, wantHostPort)
	composeArgs := composeFileArgs(composePath, composePath)
	composeArgs = append(composeArgs, "up", "-d", "--force-recreate")
	args := rt.ComposeArgs(composeArgs...)
	cmd := exec.Command(rt.Bin, args...)
	cmd.Env = composeEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return true, nil
}

// hostBinding is one published host binding of a running container: the host
// interface it is bound to (HostIp, e.g. "127.0.0.1", "0.0.0.0", "::", or "")
// and the host port. It mirrors a single entry of docker/podman's
// NetworkSettings.Ports value list.
type hostBinding struct {
	HostIP   string
	HostPort int
}

// runningPublishedHostBindings returns every host binding a running container
// publishes and whether the container is running. It shells out to
// `<engine> inspect` with a Go-template that walks NetworkSettings.Ports,
// mirroring the inspect pattern already used in cmd/service.go. On any error or
// a non-running/no-binding container it returns (nil, false).
//
// A bare (all-interfaces) publish emits TWO bindings on docker -- one HostIp
// "0.0.0.0" and one "::" -- and an empty HostIp on podman, so callers that care
// about the bind must examine every binding, not just the first.
func runningPublishedHostBindings(engineBin, containerName string) ([]hostBinding, bool) {
	// {{range}} over the port map; for each binding print "HostIp|HostPort".
	// Neither field can contain a '|', so it is a safe separator (an IPv6 HostIp
	// like "::" carries colons, which is why we do not split on ':'). The
	// template prints a space-separated list.
	format := `{{range $p, $conf := .NetworkSettings.Ports}}{{range $conf}}{{.HostIp}}|{{.HostPort}} {{end}}{{end}}`
	out, err := exec.Command(engineBin, "inspect",
		"--format", format, containerName).Output()
	if err != nil {
		return nil, false
	}
	bindings := parseHostBindings(string(out))
	if len(bindings) == 0 {
		return nil, false
	}
	return bindings, true
}

// parseHostBindings parses the space-separated "HostIp|HostPort" tokens emitted
// by runningPublishedHostBindings' inspect template. It is a pure function so
// the parse is unit-testable without a live container runtime. Tokens with a
// non-positive or unparseable port are skipped.
func parseHostBindings(raw string) []hostBinding {
	var bindings []hostBinding
	for _, tok := range strings.Fields(raw) {
		parts := strings.SplitN(tok, "|", 2)
		if len(parts) != 2 {
			continue
		}
		port, convErr := strconv.Atoi(parts[1])
		if convErr != nil || port <= 0 {
			continue
		}
		bindings = append(bindings, hostBinding{HostIP: parts[0], HostPort: port})
	}
	return bindings
}

// runningPublishedHostPort returns the host port a running container publishes
// (its first published port) and whether the container is running. On any error
// or a non-running/no-port container it returns (0, false).
func runningPublishedHostPort(engineBin, containerName string) (int, bool) {
	bindings, running := runningPublishedHostBindings(engineBin, containerName)
	if !running {
		return 0, false
	}
	return bindings[0].HostPort, true
}

// loopbackDriftEngines is the set of services.ServiceMap engines whose embedded
// compose templates were changed to publish on 127.0.0.1 (loopback) instead of
// 0.0.0.0 by aceteam-ai/aceteam#9523 (PR #1025). These engines have no auth of
// their own, so a running container left bound to all interfaces after an
// auto-update is the unauthenticated-LLM-on-the-LAN exposure #1025 closed for
// fresh materializations but could not reach on already-running containers
// (aceteam-ai/citadel-cli#1030). It is deliberately NARROWER than
// services.ServiceMap membership: extraction/diffusers/transcribe/lmstudio are
// still intended to publish on all interfaces in v1 (their loopback move is
// tracked in aceteam-ai/citadel-cli#1023), so they must NOT be recreated here.
// TestLoopbackDriftEnginesPublishLoopback pins this set against the actual
// compose templates so it cannot silently disagree with what #1025 edited.
var loopbackDriftEngines = map[string]struct{}{
	"vllm":          {},
	"llamacpp":      {},
	"bonsai":        {},
	"unlimited-ocr": {},
	"sglang":        {},
}

// isWildcardHostIP reports whether a published HostIp means "all interfaces".
// docker uses "0.0.0.0" (IPv4) and "::" (IPv6); podman reports an empty HostIp
// for a wildcard publish. Loopback ("127.0.0.1", "::1") and any explicit
// non-wildcard IP are NOT wildcards.
func isWildcardHostIP(ip string) bool {
	switch strings.TrimSpace(ip) {
	case "", "0.0.0.0", "::":
		return true
	default:
		return false
	}
}

// composePublishesLoopbackOnly reports whether a materialized compose file's
// content publishes every host port on the loopback interface (a "127.0.0.1:"
// prefix on each `ports:` host-publish entry). It mirrors
// services.embed_test.go's TestServiceMapBindSweep parse deliberately -- a
// minimal `services[].ports` YAML read, NOT the broader portspec parser
// aceteam-ai/citadel-cli#1023 introduces. Returns false when there is no host
// publish at all, or when any host publish is not loopback-prefixed.
//
// This is the loop guard for the drift recreate: it must read the ON-DISK
// materialized template, so that after a one-time recreate the running
// container matches the file and the next boot sees no drift. A file an
// operator has hand-edited to publish on 0.0.0.0 (hash-preserved by
// composerefresh.Sweep) therefore is NOT recreated -- that is an explicit
// operator decision, not drift to heal.
func composePublishesLoopbackOnly(content string) bool {
	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return false
	}
	hasHostPublish := false
	for _, svc := range doc.Services {
		for _, spec := range svc.Ports {
			if !strings.Contains(spec, ":") {
				continue // container-port-only, no host publish
			}
			hasHostPublish = true
			if !strings.HasPrefix(spec, "127.0.0.1:") {
				return false
			}
		}
	}
	return hasHostPublish
}

// engineBindDriftRequiresRecreate is the pure decision behind the one-time
// loopback-bind drift remediation (aceteam-ai/citadel-cli#1030). It returns
// true only when ALL of:
//   - service is one of the #1025 loopbackDriftEngines (scope);
//   - the materialized composeContent publishes loopback-only (loop guard: the
//     recreate applies this file, so recreating a non-loopback file would
//     restart every boot forever);
//   - the running container has at least one wildcard (all-interfaces) binding.
//
// A container already on loopback (or an explicit non-wildcard IP) is left
// alone, so the remediation fires at most once per node and never becomes a
// restart loop. Kept pure (content + bindings passed in) so it is unit-testable
// without a live container runtime.
func engineBindDriftRequiresRecreate(service, composeContent string, bindings []hostBinding) bool {
	if _, ok := loopbackDriftEngines[service]; !ok {
		return false
	}
	if !composePublishesLoopbackOnly(composeContent) {
		return false
	}
	for _, b := range bindings {
		if isWildcardHostIP(b.HostIP) {
			return true
		}
	}
	return false
}

// shouldRecreateForEngineBindDrift decides whether startService's already-running
// pre-flight must force-recreate a running engine container to apply the #1025
// loopback publish. It layers the runtime signals (recreate opt-out, live
// inspect, materialized-file read) on top of the pure
// engineBindDriftRequiresRecreate decision, and short-circuits BEFORE the extra
// docker inspect for any service outside loopbackDriftEngines so the common
// already-running healthy service pays nothing. composeFilePath must be the
// ORIGINAL materialized compose path (not a GPU-stripped temp copy).
func shouldRecreateForEngineBindDrift(engineBin, service, containerName, composeFilePath string) bool {
	if !recreateOnUpgradeEnabled() {
		return false
	}
	if _, ok := loopbackDriftEngines[service]; !ok {
		return false
	}
	content, err := os.ReadFile(composeFilePath)
	if err != nil {
		return false
	}
	bindings, running := runningPublishedHostBindings(engineBin, containerName)
	if !running {
		return false
	}
	return engineBindDriftRequiresRecreate(service, string(content), bindings)
}
