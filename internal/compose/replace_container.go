package compose

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"gopkg.in/yaml.v3"
)

// Exec runs one prepared engine or compose command and returns its stdout and
// stderr separately. Tests substitute a fake runtime; DefaultExec runs it.
type Exec func(cmd *exec.Cmd) (stdout, stderr []byte, err error)

// DefaultExec runs cmd and captures both streams.
func DefaultExec(cmd *exec.Cmd) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// ReplaceSettleDelay is how long ReplaceRunningService waits after `up` before
// checking the replacement is still running, so a container that exits at once
// counts as a failed replacement.
var ReplaceSettleDelay = 3 * time.Second

// ReplaceOutcome is what ReplaceRunningService did to the service.
type ReplaceOutcome int

const (
	// ReplaceUnchanged: compose kept the existing container (same ID).
	ReplaceUnchanged ReplaceOutcome = iota
	// ReplaceReplaced: a new container now runs under the pinned name.
	ReplaceReplaced
	// ReplaceRestored: the replacement failed and the previous container (or,
	// for a same-project container compose had already removed, one started
	// from the restored previous file) runs again. The error says why.
	ReplaceRestored
	// ReplaceFailed: the replacement failed and the previous state could not
	// be fully restored. The error says what is left.
	ReplaceFailed
)

// ReplaceSpec describes one compose service replacement.
type ReplaceSpec struct {
	Runtime catalog.ContainerRuntime
	// Service is the compose service key; ContainerName its pinned
	// container_name.
	Service       string
	ContainerName string
	// ComposeArgs builds the compose arguments before the verb (project and
	// -f/--env-file arguments) from the compose file as it is on disk when
	// called, plus a cleanup for any temporary rewrite. It is called again
	// after RestoreFile for a recovery up.
	ComposeArgs func() ([]string, func(), error)
	// UpArgs follow `up -d` (for example --force-recreate).
	UpArgs []string
	Env    []string
	// Preflight runs before any container is touched (resource and
	// limit-controller checks). Nil skips it.
	Preflight func() error
	// RestoreFile restores the previous compose file after a failed
	// replacement. Nil when the file did not change.
	RestoreFile func() error
	// Exec runs every command; nil means DefaultExec.
	Exec Exec
	// Log receives progress lines; may be nil.
	Log func(format string, args ...any)
}

// legacyProjectLabel is the compose project a pre-#528 `-p citadel-<name>`
// start left on a container.
func legacyProjectLabel(service string) string { return "citadel-" + service }

// BackupContainerName is the name ReplaceRunningService gives a legacy-project
// container while its replacement starts.
func BackupContainerName(containerName string) string { return containerName + "-citadel-prev" }

type containerInfo struct {
	exists           bool
	id, state        string
	project, service string
}

func (c containerInfo) running() bool { return c.exists && c.state == "running" }

func (c containerInfo) legacy(service string) bool {
	return c.exists && c.project == legacyProjectLabel(service) && c.service == service
}

// ours reports a container compose created for this service in a current
// (non-legacy) project.
func (c containerInfo) ours(service string) bool {
	return c.exists && c.service == service && c.project != "" && c.project != legacyProjectLabel(service)
}

const inspectFormat = `{{.Id}}|{{.State.Status}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}`

type replacer struct {
	spec ReplaceSpec
	exec Exec
}

func (r replacer) logf(format string, args ...any) {
	if r.spec.Log != nil {
		r.spec.Log(format, args...)
	}
}

func (r replacer) engine(args ...string) ([]byte, error) {
	cmd := r.spec.Runtime.EngineCommand(args...)
	stdout, stderr, err := r.exec(cmd)
	if err != nil {
		return stdout, fmt.Errorf("%s %s: %v: %s", r.spec.Runtime.EngineBin, args[0], err, strings.TrimSpace(string(stderr)))
	}
	return stdout, nil
}

func (r replacer) compose(args ...string) ([]byte, error) {
	cmd := r.spec.Runtime.ComposeCommand(args...)
	cmd.Env = r.spec.Env
	stdout, stderr, err := r.exec(cmd)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(stdout))
		}
		return stdout, fmt.Errorf("%s compose %s failed: %s", r.spec.Runtime.Bin, composeVerb(args), msg)
	}
	return stdout, nil
}

func composeVerb(args []string) string {
	for _, a := range args {
		switch a {
		case "up", "config", "pull", "build":
			return a
		}
	}
	return "command"
}

func (r replacer) inspect(name string) containerInfo {
	// `container inspect`, never a bare `inspect`, which also matches an image,
	// network or volume of the same name.
	out, err := r.engine("container", "inspect", "--format", inspectFormat, name)
	if err != nil {
		return containerInfo{}
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 4)
	if len(parts) != 4 || parts[0] == "" {
		return containerInfo{}
	}
	clean := func(v string) string {
		if v == "<no value>" {
			return ""
		}
		return v
	}
	return containerInfo{exists: true, id: parts[0], state: parts[1], project: clean(parts[2]), service: clean(parts[3])}
}

// ReplaceRunningService applies the compose file to Service's container so the
// previous container stays recoverable until its replacement is confirmed
// running.
//
//  1. Preflight touches nothing: Spec.Preflight, `compose config` (a file that
//     does not render would fail `up` too), and image presence. A missing image
//     is pulled or built now; a present one is never re-pulled, so a floating
//     tag is not silently upgraded.
//  2. A container under the legacy per-service project (#528) is stopped and
//     renamed to BackupContainerName so `up` can claim the pinned name; compose
//     selects containers by project labels, so it never touches the backup.
//     Only a container whose project and service labels are the legacy ones is
//     renamed; one owned by anything else is refused untouched.
//  3. A same-project container cannot be hidden from compose (labels are
//     immutable), so compose replaces it itself, and the old container cannot
//     be kept: compose v2 creates the new container first but removes the old
//     one before starting the new one, and podman-compose 1.0.6 removes the old
//     one first and can exit 0 when the new one fails to run. A create failure
//     leaves it running; any later failure has already removed it.
//  4. `up -d`, then the container under the pinned name must be ours and still
//     running after ReplaceSettleDelay. Only then is the backup removed.
//  5. On failure: a partial new container is removed, the backup is renamed
//     back and started if it had been running, and the file is restored. For a
//     same-project service that was running, the real recovery is a forced
//     `up` from the restored previous file, checked to be running. A stopped
//     same-project container compose removed is reported as removed
//     (ReplaceFailed), not restored.
//
// A backup left by an earlier run is handled first. With the pinned name free
// it is renamed back. When the pinned name already holds our running
// replacement (an earlier run whose backup removal failed), the backup is
// removed. Any other combination is refused with an actionable error.
//
// Race: the legacy cleanup some other paths still run
// (RemoveLegacyProjectContainers, from the Control Center service actions,
// module update and device-config apply) force-removes every container labelled
// with the legacy project, the renamed backup included. If one runs between the
// rename and the restore, the backup is lost and a failed replacement cannot
// bring it back; the error then reports that the previous state was not
// restored. The worker's serialized job lane and the local start guard do not
// cover the Control Center.
//
// Commands go through Spec.Runtime's EngineCommand/ComposeCommand seam. A crash
// between the rename and the restore leaves the stopped backup behind. A call
// that finds it while the pinned name is free renames it back first, but the
// callers only call this for a running container, so a fresh start does not
// reach that recovery: the backup stays until it is removed by hand or by a
// legacy-project cleanup.
func ReplaceRunningService(spec ReplaceSpec) (ReplaceOutcome, error) {
	r := replacer{spec: spec, exec: spec.Exec}
	if r.exec == nil {
		r.exec = DefaultExec
	}
	backupName := BackupContainerName(spec.ContainerName)

	old := r.inspect(spec.ContainerName)
	if leftover := r.inspect(backupName); leftover.exists {
		switch {
		case !leftover.legacy(spec.Service):
			return ReplaceUnchanged, fmt.Errorf("container %s exists but is not a citadel backup of service %s; remove or rename it before retrying", backupName, spec.Service)
		case !old.exists:
			r.logf("compose: %s: restoring backup %s left by an interrupted replacement", spec.Service, backupName)
			if _, err := r.engine("rename", leftover.id, spec.ContainerName); err != nil {
				return ReplaceUnchanged, err
			}
			old = r.inspect(spec.ContainerName)
		case old.ours(spec.Service) && old.running():
			// An earlier replacement succeeded but could not remove its backup.
			r.logf("compose: %s: removing backup %s left by an earlier replacement; %s is running", spec.Service, backupName, spec.ContainerName)
			if _, err := r.engine("rm", "-f", leftover.id); err != nil {
				return ReplaceUnchanged, fmt.Errorf("remove leftover backup %s (run `%s rm -f %s`): %w", backupName, spec.Runtime.EngineBin, backupName, err)
			}
		default:
			return ReplaceUnchanged, fmt.Errorf("both %s (%s) and an earlier backup %s exist; check which one should run, then remove the other with `%s rm -f <name>`", spec.ContainerName, nonEmpty(old.state, "unknown"), backupName, spec.Runtime.EngineBin)
		}
	}
	if old.exists && !old.legacy(spec.Service) && !old.ours(spec.Service) {
		return ReplaceUnchanged, fmt.Errorf("container %s is not a compose container of service %s; leaving it untouched", spec.ContainerName, spec.Service)
	}

	if err := r.preflight(); err != nil {
		return ReplaceUnchanged, err
	}

	backedUp := false
	if old.legacy(spec.Service) {
		if old.running() {
			if _, err := r.engine("stop", old.id); err != nil {
				return ReplaceUnchanged, err
			}
		}
		if _, err := r.engine("rename", old.id, backupName); err != nil {
			if old.running() {
				_, _ = r.engine("start", old.id)
			}
			return ReplaceUnchanged, err
		}
		backedUp = true
	}

	upErr := r.up(spec.UpArgs...)
	current := r.inspect(spec.ContainerName)
	if upErr == nil && ReplaceSettleDelay > 0 {
		time.Sleep(ReplaceSettleDelay)
		current = r.inspect(spec.ContainerName)
	}
	if upErr == nil && current.ours(spec.Service) && current.running() {
		if backedUp {
			if _, err := r.engine("rm", "-f", old.id); err != nil {
				r.logf("compose: %s: replacement running; could not remove backup %s (the next replacement removes it, or run `%s rm -f %s`): %v", spec.Service, backupName, spec.Runtime.EngineBin, backupName, err)
			}
		}
		if old.exists && current.id == old.id {
			return ReplaceUnchanged, nil
		}
		return ReplaceReplaced, nil
	}
	failure := upErr
	if failure == nil {
		failure = fmt.Errorf("replacement container %s is %s, not running", spec.ContainerName, nonEmpty(current.state, "missing"))
	}
	return r.recover(old, backedUp, failure)
}

func (r replacer) recover(old containerInfo, backedUp bool, failure error) (ReplaceOutcome, error) {
	spec := r.spec
	var problems []error
	current := r.inspect(spec.ContainerName)
	if current.exists && current.id != old.id && current.ours(spec.Service) {
		if _, err := r.engine("rm", "-f", current.id); err != nil {
			problems = append(problems, err)
		}
	}
	if spec.RestoreFile != nil {
		if err := spec.RestoreFile(); err != nil {
			problems = append(problems, fmt.Errorf("restore previous compose: %w", err))
		}
	}
	if backedUp {
		if _, err := r.engine("rename", old.id, spec.ContainerName); err != nil {
			problems = append(problems, err)
		} else if old.running() {
			if _, err := r.engine("start", old.id); err != nil {
				problems = append(problems, err)
			}
		}
	} else if old.running() {
		if now := r.inspect(spec.ContainerName); !now.running() {
			r.logf("compose: %s: replacement failed; starting it again from the previous compose", spec.Service)
			if err := r.up("--force-recreate"); err != nil {
				problems = append(problems, err)
			}
		}
	}
	if old.running() {
		if now := r.inspect(spec.ContainerName); !now.running() {
			problems = append(problems, fmt.Errorf("%s is not running", spec.ContainerName))
		}
	} else if old.exists && !backedUp {
		if now := r.inspect(spec.ContainerName); now.id != old.id {
			// compose already removed the stopped container; nothing was
			// running, so nothing is started, but it is not restored either.
			problems = append(problems, fmt.Errorf("the stopped container %s (%s) was removed by compose and not recreated", spec.ContainerName, old.id))
		}
	}
	if len(problems) > 0 {
		return ReplaceFailed, fmt.Errorf("replace %s: %w; previous state not restored: %v", spec.Service, failure, errors.Join(problems...))
	}
	return ReplaceRestored, fmt.Errorf("replace %s: %w; previous container restored", spec.Service, failure)
}

func (r replacer) up(extra ...string) error {
	args, cleanup, err := r.spec.ComposeArgs()
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = r.compose(append(append(args, "up", "-d"), extra...)...)
	return err
}

func (r replacer) preflight() error {
	if r.spec.Preflight != nil {
		if err := r.spec.Preflight(); err != nil {
			return err
		}
	}
	args, cleanup, err := r.spec.ComposeArgs()
	if err != nil {
		return err
	}
	defer cleanup()
	rendered, err := r.compose(append(append([]string(nil), args...), "config")...)
	if err != nil {
		return err
	}
	var cfg struct {
		Services map[string]struct {
			Image string `yaml:"image"`
			Build any    `yaml:"build"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(rendered, &cfg); err != nil {
		return fmt.Errorf("parse rendered compose for %s: %w", r.spec.Service, err)
	}
	svc, ok := cfg.Services[r.spec.Service]
	if !ok {
		return fmt.Errorf("rendered compose does not declare service %s", r.spec.Service)
	}
	if svc.Image != "" {
		if _, err := r.engine("image", "inspect", svc.Image); err == nil {
			return nil
		}
	}
	verb := "pull"
	if svc.Build != nil {
		verb = "build"
	} else if svc.Image == "" {
		return nil
	}
	r.logf("compose: %s: image missing; running compose %s before touching the running container", r.spec.Service, verb)
	if _, err := r.compose(append(append([]string(nil), args...), verb, r.spec.Service)...); err != nil {
		return err
	}
	if svc.Image != "" {
		if _, err := r.engine("image", "inspect", svc.Image); err != nil {
			return fmt.Errorf("image %s still missing after compose %s", svc.Image, verb)
		}
	}
	return nil
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
