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
	containerName := "citadel-" + service

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

// runningPublishedHostPort returns the host port a running container publishes
// (its first published port) and whether the container is running. It shells out
// to `<engine> inspect` with a Go-template that walks NetworkSettings.Ports,
// mirroring the inspect pattern already used in cmd/service.go. On any error or
// a non-running/no-port container it returns (0, false).
func runningPublishedHostPort(engineBin, containerName string) (int, bool) {
	// {{range}} over the port map; for each binding print the HostPort. We take
	// the first non-empty one. The template prints a space-separated list.
	format := `{{range $p, $conf := .NetworkSettings.Ports}}{{range $conf}}{{.HostPort}} {{end}}{{end}}`
	out, err := exec.Command(engineBin, "inspect",
		"--format", format, containerName).Output()
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(out))
	for _, f := range fields {
		if p, convErr := strconv.Atoi(f); convErr == nil && p > 0 {
			return p, true
		}
	}
	return 0, false
}
