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
// Recreate-on-boot policy: the primary upgrade path is a hands-off auto-update
// that re-execs the binary in place (syscall.Exec) WITHOUT tearing down service
// containers, and startManagedServices no-ops on an already-running container.
// Refreshing only the FILE would therefore never reach an auto-updated node's
// running container -- the old host port, command or image would keep running.
// So the sweep hands every file it rewrote to managedComposeRecreatorUnchecked,
// which applies it to a RUNNING container (and only a running one) once. The
// version stamp keeps that from repeating. Operators who prefer to move live
// containers by hand can opt out with
// CITADEL_COMPOSE_NO_RECREATE_ON_UPGRADE=1|true|yes, which downgrades to
// file-refresh + a remediation hint.
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
)

// refreshManagedComposeFiles runs the version-gated compose refresh sweep for
// the given node config directory. It is safe to call on every boot: when the
// binary version is unchanged it is a cheap no-op. Failures are logged and never
// abort startup -- a refresh problem must not stop the node from serving jobs.
func refreshManagedComposeFiles(configDir string) {
	refreshManagedComposeFilesWithSweep(configDir, composerefresh.Sweep, platform.NvidiaGPUCountForCompose)
}

func refreshManagedComposeFilesWithSweep(configDir string, sweep func(composerefresh.Options) (composerefresh.Result, error), gpuCount func() int) {
	if configDir == "" {
		return
	}
	servicesDir := filepath.Join(configDir, "services")

	var recreator composerefresh.PortRecreator
	if recreateOnUpgradeEnabled() {
		// The sweep owns reservation.lock; avoid a nested lock in its callback.
		recreator = managedComposeRecreatorUnchecked
	}
	var res composerefresh.Result
	err := withLocalServiceMutationLock(configDir, func() error {
		// A sweep may replace a CPU-looking compose with a GPU template before
		// force-recreate. Defer the entire rewrite while training is held.
		if err := finetunesafety.RequireAbsent(configDir); err != nil {
			return err
		}
		embedded, missingOnly := services.ComposeRefreshPlan(runtime.GOOS, gpuCount)
		// Unknown GPU evidence withholds existing CPU/GPU files from refresh,
		// but a missing file still needs the default embedded template. This
		// runs before the version fast path so the same binary retries absence.
		if _, err := composerefresh.MaterializeMissing(servicesDir, missingOnly); err != nil {
			return err
		}
		var sweepErr error
		res, sweepErr = sweep(composerefresh.Options{
			ServicesDir:           servicesDir,
			Version:               Version,
			Embedded:              embedded,
			PortManaged:           services.ServiceHostPorts,
			KnownHistoricalHashes: services.KnownComposeHashes,
			Recreator:             recreator,
			Log:                   func(format string, args ...any) { Log(format, args...) },
		})
		return sweepErr
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
		Log("compose-refresh: applied refreshed composes to running containers: %s", strings.Join(res.Recreated, ", "))
	}
}

// recreateOnUpgradeEnabled reports whether a refreshed managed template is
// applied to its running container automatically. It is ON by default; see the
// file header. composerefresh.RecreateOnUpgradeDisabled owns the opt-out.
func recreateOnUpgradeEnabled() bool {
	return !composerefresh.RecreateOnUpgradeDisabled()
}

// enginePortRecreator is managedComposeRecreatorUnchecked behind the local
// service start guard, for callers that do not already hold reservation.lock.
func enginePortRecreator(service, composePath string, wantHostPort int) (bool, error) {
	configDir := filepath.Dir(filepath.Dir(composePath))
	var recreated bool
	err := withLocalServiceStartGuard(configDir, service, func() error {
		var recreateErr error
		recreated, recreateErr = managedComposeRecreatorUnchecked(service, composePath, wantHostPort, nil)
		return recreateErr
	})
	return recreated, err
}

// managedComposeDeps are the runtime seams the refresh recreate uses, so tests
// drive it with fakes instead of a container engine.
type managedComposeDeps struct {
	runtime        func() catalog.ContainerRuntime
	containerState func(engineBin, containerName string) (string, bool)
	publishedPort  func(engineBin, containerName string) (int, bool)
	serviceFor     func(configDir, service string) Service
	// exec runs every command of a container replacement
	// (compose.ReplaceRunningService).
	exec compose.Exec
}

var managedCompose = managedComposeDeps{
	runtime:        catalog.SelectContainerRuntime,
	containerState: containerStateFor,
	publishedPort:  runningPublishedHostPort,
	serviceFor:     manifestServiceAt,
	exec:           compose.DefaultExec,
}

// replaceManagedService applies composePath to serviceName's container through
// compose.ReplaceRunningService, so a previously running container stays
// recoverable until its replacement runs. Every start-path recreate uses it:
// the boot sweep, startService's running branch and the all-services restart.
// restore puts back the previous compose file if the replacement fails.
func replaceManagedService(rt catalog.ContainerRuntime, serviceName, composePath string, upArgs, env []string, restore func() error) (compose.ReplaceOutcome, error) {
	containerName := "citadel-" + serviceName
	if isEmbeddedService(serviceName) {
		containerName = embeddedContainerName(serviceName)
	}
	return compose.ReplaceRunningService(compose.ReplaceSpec{
		Runtime:       rt,
		Service:       serviceName,
		ContainerName: containerName,
		ComposeArgs: func() ([]string, func(), error) {
			return prepareComposeFileArgs(serviceName, composePath, rt.EngineBin)
		},
		UpArgs:      upArgs,
		Env:         env,
		Preflight:   func() error { return composeLimitsPreflight(rt, serviceName, composePath) },
		RestoreFile: restore,
		Exec:        managedCompose.exec,
		Log:         func(format string, args ...any) { Log(format, args...) },
	})
}

// composeLimitsPreflight refuses a start whose declared cpu/memory/pids limits
// the runtime cannot enforce, before any container is touched.
func composeLimitsPreflight(rt catalog.ContainerRuntime, serviceName, composePath string) error {
	required, err := compose.RequiredLimitControllersFromFiles(composePath, sandboxOverridePathFor(composePath))
	if err != nil {
		return fmt.Errorf("cannot inspect %s resource limits: %w", serviceName, err)
	}
	if err := rt.PreflightLimitControllers(required...); err != nil {
		return fmt.Errorf("cannot start %s: %w", serviceName, err)
	}
	return nil
}

// containerStateFor returns a container's State.Status and whether it exists.
func containerStateFor(engineBin, containerName string) (string, bool) {
	out, err := exec.Command(engineBin, "inspect", "--format", "{{.State.Status}}", containerName).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// manifestServiceAt reads one service entry from the manifest in configDir
// without consulting the global node pointer. A missing entry is the zero
// Service.
func manifestServiceAt(configDir, service string) Service {
	manifest, err := readManifestAt(configDir)
	if err != nil {
		return Service{}
	}
	for _, s := range manifest.Services {
		if s.Name == service {
			return s
		}
	}
	return Service{}
}

// managedComposeRecreatorUnchecked applies a compose file the refresh sweep just
// rewrote to that service's RUNNING container. It is the sweep's
// composerefresh.PortRecreator, so it runs at most once per template change.
//
//   - A stopped, exited or absent container is left alone: the next start uses
//     the new file. A service marked `desired_status: stopped` is never touched.
//   - When the container's published host port differs from wantHostPort (a
//     port-managed service whose publish moved), it is force-recreated.
//   - Otherwise `compose up -d` lets compose recreate it only if its config
//     hash changed, so a comment-only template change does not restart a live
//     engine. On Podman the up is forced, because podman-compose does not
//     compare config hashes.
//   - replaceManagedService keeps the old container recoverable; restore (the
//     sweep's rollback of the file) runs before any recovery up.
//
// It reports true only when a new container replaced the old one. The caller
// holds reservation.lock (the sweep does).
func managedComposeRecreatorUnchecked(service, composePath string, wantHostPort int, restore func() error) (bool, error) {
	configDir := filepath.Dir(filepath.Dir(composePath))
	svc := managedCompose.serviceFor(configDir, service)
	if serviceStartDisabled(svc) {
		return false, nil
	}
	rt := managedCompose.runtime()
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
	if state, exists := managedCompose.containerState(rt.EngineBin, containerName); !exists || state != "running" {
		return false, nil
	}
	var upArgs []string
	if wantHostPort > 0 {
		if current, ok := managedCompose.publishedPort(rt.EngineBin, containerName); ok && current != wantHostPort {
			Log("compose-refresh: %s: host port moved %d -> %d; force-recreating", service, current, wantHostPort)
			upArgs = []string{"--force-recreate"}
		}
	}
	if upArgs == nil && rt.EngineBin == "podman" {
		upArgs = []string{"--force-recreate"}
	}
	Log("compose-refresh: %s: applying the refreshed compose to the running container", service)
	outcome, err := replaceManagedService(rt, service, composePath, upArgs, composeEnvForServiceValues(service, svc), restore)
	if err != nil {
		return false, err
	}
	return outcome == compose.ReplaceReplaced, nil
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
// 0.0.0.0 by aceteam-ai/aceteam#9523 (PR #1025) and its follow-up sweep
// aceteam-ai/citadel-cli#1060 (extraction/diffusers/transcribe). These
// engines have no auth of their own, so a running container left bound to all
// interfaces after an auto-update is the unauthenticated-LLM-on-the-LAN exposure
// #1025 closed for fresh materializations but could not reach on already-running
// containers (aceteam-ai/citadel-cli#1030). It is deliberately NARROWER than
// services.ServiceMap membership (kokoro/omnivoice publish loopback via a literal
// prefix, not a driftable 0.0.0.0 default). TestLoopbackDriftEnginesPublishLoopback
// pins this set against the actual compose templates so it cannot silently
// disagree with what #1025/#1060 edited.
var loopbackDriftEngines = map[string]struct{}{
	"vllm":          {},
	"llamacpp":      {},
	"bonsai":        {},
	"unlimited-ocr": {},
	"sglang":        {},
	"extraction":    {},
	"diffusers":     {},
	"transcribe":    {},
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
// content publishes every host port on a loopback interface, resolving the
// #1023 bind hatch (${CITADEL_<SVC>_BIND:-127.0.0.1}) against env -- the SAME
// env the recreate `up` will inject (from the manifest bind:), so the loop
// guard and the recreate can never disagree. It delegates to the single
// authority services.ComposePublishesLoopbackOnly. Returns false when there is
// no host publish at all, or when any host publish resolves to a non-loopback
// interface.
//
// This is the loop guard for the drift recreate: it must resolve the ON-DISK
// materialized template exactly as the recreate will, so that after a one-time
// recreate the running container matches and the next boot sees no drift. A
// file an operator has hand-edited to publish on 0.0.0.0 (hash-preserved by
// composerefresh.Sweep) -- or a service the operator set `bind: all` on -- is
// therefore NOT recreated: an explicit operator decision, not drift to heal.
func composePublishesLoopbackOnly(content string, env map[string]string) bool {
	loopbackOnly, hasPublish := services.ComposePublishesLoopbackOnly(content, env)
	return hasPublish && loopbackOnly
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
// restart loop. Kept pure (content + bindings + bind env passed in) so it is
// unit-testable without a live container runtime.
//
// env is the #1023 CITADEL_<SVC>_BIND map the recreate would inject (from the
// manifest bind:). A service the operator set `bind: all` on resolves the
// on-disk template to 0.0.0.0, so composePublishesLoopbackOnly returns false
// and this returns false too -- the recreate that would bring it up on 0.0.0.0
// (matching the running wildcard binding) must NOT fire, or it would loop every
// boot.
func engineBindDriftRequiresRecreate(service, composeContent string, bindings []hostBinding, env map[string]string) bool {
	if _, ok := loopbackDriftEngines[service]; !ok {
		return false
	}
	if !composePublishesLoopbackOnly(composeContent, env) {
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
// ORIGINAL materialized compose path (not a GPU-stripped temp copy). bindEnv is
// the #1023 CITADEL_<SVC>_BIND map startService resolved from the manifest bind:
// -- the SAME env the recreate `up` uses -- so an operator `bind: all` service
// is never drift-recreated into a boot loop.
func shouldRecreateForEngineBindDrift(engineBin, service, containerName, composeFilePath string, bindEnv map[string]string) bool {
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
	return engineBindDriftRequiresRecreate(service, string(content), bindings, bindEnv)
}
