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
	// ReplaceRestored: the replacement failed and the previous container runs
	// again. The error says why.
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
	// Project is the exact compose project that ComposeArgs will use. Never
	// infer ownership from a matching service label alone.
	Project string
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
func (c containerInfo) ours(service, project string) bool {
	return c.exists && c.service == service && project != "" && c.project == project
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

func (r replacer) inspect(name string) (containerInfo, error) {
	// `container inspect`, never a bare `inspect`, which also matches an image,
	// network or volume of the same name.
	cmd := r.spec.Runtime.EngineCommand("container", "inspect", "--format", inspectFormat, name)
	out, stderr, err := r.exec(cmd)
	if err != nil {
		message := strings.ToLower(strings.TrimSpace(string(stderr)))
		if strings.Contains(message, "no such container") || strings.Contains(message, "no such object") {
			return containerInfo{}, nil
		}
		return containerInfo{}, fmt.Errorf("inspect container %s: %w: %s", name, err, strings.TrimSpace(string(stderr)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 4)
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" {
		return containerInfo{}, fmt.Errorf("inspect container %s: malformed response", name)
	}
	clean := func(v string) string {
		if v == "<no value>" {
			return ""
		}
		return v
	}
	return containerInfo{exists: true, id: parts[0], state: parts[1], project: clean(parts[2]), service: clean(parts[3])}, nil
}

// ReplaceRunningService preflights and applies a compose replacement while a
// legacy-project container remains recoverable under a backup name. It refuses
// a running same-project container: immutable compose labels mean compose may
// remove that container before the replacement starts, so a recovery up cannot
// guarantee the old service. Refusal restores the previous managed file.
// A confirmed legacy backup is removed only after the replacement is running.
// RemoveLegacyProjectContainers excludes backups and shares an OS advisory
// lock with this operation. Commands use Spec.Runtime's EngineCommand/ComposeCommand.
func ReplaceRunningService(spec ReplaceSpec) (ReplaceOutcome, error) {
	legacyCleanupMu.Lock()
	defer legacyCleanupMu.Unlock()
	var outcome ReplaceOutcome
	var replaceErr error
	ran := false
	lockErr := withLegacyMutationLock(spec.Runtime.EngineBin, spec.Service, func() error {
		ran = true
		outcome, replaceErr = replaceRunningServiceLocked(spec)
		return nil
	})
	if !ran {
		return (replacer{spec: spec}).abortUnchanged(fmt.Errorf("compose mutation lock unavailable: %w", lockErr))
	}
	return outcome, errors.Join(replaceErr, lockErr)
}

func replaceRunningServiceLocked(spec ReplaceSpec) (ReplaceOutcome, error) {
	r := replacer{spec: spec, exec: spec.Exec}
	if r.exec == nil {
		r.exec = DefaultExec
	}
	if spec.Project == "" {
		return r.abortUnchanged(fmt.Errorf("compose project for %s is not known; refusing replacement", spec.Service))
	}
	backupName := BackupContainerName(spec.ContainerName)

	old, err := r.inspect(spec.ContainerName)
	if err != nil {
		return r.abortUnchanged(err)
	}
	leftover, err := r.inspect(backupName)
	if err != nil {
		return r.abortUnchanged(err)
	}
	if old.exists && !old.legacy(spec.Service) && !old.ours(spec.Service, spec.Project) {
		return r.abortUnchanged(fmt.Errorf("container %s belongs to project %s, not %s; leaving it untouched", spec.ContainerName, old.project, spec.Project))
	}
	if leftover.exists && !leftover.legacy(spec.Service) {
		return r.abortUnchanged(fmt.Errorf("container %s exists but is not a citadel backup of service %s; leaving it untouched", backupName, spec.Service))
	}
	if old.ours(spec.Service, spec.Project) && old.running() && !leftover.exists {
		return r.abortUnchanged(fmt.Errorf("running same-project container %s cannot be kept recoverable while compose recreates it; use an explicit maintenance restart", spec.ContainerName))
	}
	// Resolve image and compose errors before changing a leftover backup or a
	// running legacy container. A failed preflight also rolls back the file.
	if err := r.preflight(); err != nil {
		return r.abortUnchanged(err)
	}
	if leftover.exists {
		switch {
		case !old.exists:
			r.logf("compose: %s: restoring backup %s left by an interrupted replacement", spec.Service, backupName)
			if _, err := r.engine("rename", leftover.id, spec.ContainerName); err != nil {
				return r.abortUnchanged(err)
			}
			old, err = r.inspect(spec.ContainerName)
			if err != nil {
				return ReplaceFailed, err
			}
		case old.ours(spec.Service, spec.Project) && old.running():
			// An earlier replacement succeeded but could not remove its backup.
			r.logf("compose: %s: removing backup %s left by an earlier replacement; %s is running", spec.Service, backupName, spec.ContainerName)
			if _, err := r.engine("rm", "-f", leftover.id); err != nil {
				return r.abortUnchanged(fmt.Errorf("remove leftover backup %s (run `%s rm -f %s`): %w", backupName, spec.Runtime.EngineBin, backupName, err))
			}
			return r.abortUnchanged(fmt.Errorf("running same-project container %s cannot be kept recoverable while compose recreates it; leftover backup was removed", spec.ContainerName))
		default:
			return r.abortUnchanged(fmt.Errorf("both %s (%s) and an earlier backup %s exist; check which one should run, then remove the other with `%s rm -f <name>`", spec.ContainerName, nonEmpty(old.state, "unknown"), backupName, spec.Runtime.EngineBin))
		}
	}

	backedUp := false
	if old.legacy(spec.Service) {
		if old.running() {
			if _, err := r.engine("stop", old.id); err != nil {
				return r.abortUnchanged(err)
			}
		}
		if _, err := r.engine("rename", old.id, backupName); err != nil {
			if old.running() {
				if _, startErr := r.engine("start", old.id); startErr != nil {
					return r.abortFailed(errors.Join(err, fmt.Errorf("restart previous container: %w", startErr)))
				}
				if now, inspectErr := r.inspect(spec.ContainerName); inspectErr != nil || !now.running() {
					return r.abortFailed(errors.Join(err, fmt.Errorf("previous container did not restart: %v", inspectErr)))
				}
			}
			return r.abortUnchanged(err)
		}
		backedUp = true
	}

	upErr := r.up(spec.UpArgs...)
	current, inspectErr := r.inspect(spec.ContainerName)
	if upErr == nil && ReplaceSettleDelay > 0 {
		time.Sleep(ReplaceSettleDelay)
		current, inspectErr = r.inspect(spec.ContainerName)
	}
	if inspectErr != nil {
		return r.abortFailed(errors.Join(upErr, inspectErr, fmt.Errorf("replacement state unknown; backup retained for manual recovery")))
	}
	if upErr == nil && current.ours(spec.Service, spec.Project) && current.running() {
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

func (r replacer) abortUnchanged(failure error) (ReplaceOutcome, error) {
	if r.spec.RestoreFile != nil {
		if err := r.spec.RestoreFile(); err != nil {
			return ReplaceFailed, errors.Join(failure, fmt.Errorf("restore previous compose: %w", err))
		}
	}
	return ReplaceUnchanged, failure
}

func (r replacer) abortFailed(failure error) (ReplaceOutcome, error) {
	if r.spec.RestoreFile != nil {
		if err := r.spec.RestoreFile(); err != nil {
			failure = errors.Join(failure, fmt.Errorf("restore previous compose: %w", err))
		}
	}
	return ReplaceFailed, failure
}

func (r replacer) recover(old containerInfo, backedUp bool, failure error) (ReplaceOutcome, error) {
	spec := r.spec
	var problems []error
	current, inspectErr := r.inspect(spec.ContainerName)
	if inspectErr != nil {
		problems = append(problems, inspectErr)
	}
	if inspectErr == nil && current.exists && current.id != old.id && current.ours(spec.Service, spec.Project) {
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
		if inspectErr != nil {
			problems = append(problems, fmt.Errorf("backup %s retained because the pinned name could not be inspected", BackupContainerName(spec.ContainerName)))
		} else if _, err := r.engine("rename", old.id, spec.ContainerName); err != nil {
			problems = append(problems, err)
		} else if old.running() {
			if _, err := r.engine("start", old.id); err != nil {
				problems = append(problems, err)
			}
		}
	} else if old.running() {
		if now, err := r.inspect(spec.ContainerName); err != nil {
			problems = append(problems, err)
		} else if !now.running() {
			r.logf("compose: %s: replacement failed; starting it again from the previous compose", spec.Service)
			if err := r.up("--force-recreate"); err != nil {
				problems = append(problems, err)
			}
		}
	}
	if old.running() {
		if now, err := r.inspect(spec.ContainerName); err != nil {
			problems = append(problems, err)
		} else if !now.running() {
			problems = append(problems, fmt.Errorf("%s is not running", spec.ContainerName))
		}
	} else if old.exists && !backedUp {
		if now, err := r.inspect(spec.ContainerName); err != nil {
			problems = append(problems, err)
		} else if now.id != old.id {
			// compose already removed the stopped container; nothing was
			// running, so nothing is started, but it is not restored either.
			problems = append(problems, fmt.Errorf("the stopped container %s (%s) was removed by compose and not recreated", spec.ContainerName, old.id))
		}
	}
	if len(problems) > 0 {
		return ReplaceFailed, fmt.Errorf("replace %s: %w; previous state not restored: %v", spec.Service, failure, errors.Join(problems...))
	}
	if !old.exists {
		return ReplaceFailed, fmt.Errorf("replace %s: %w; no previous container existed", spec.Service, failure)
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
