package containersession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

// Runner drives container sessions through the catalog.ContainerRuntime seam.
// Every field with a default is an injection point so tests need no engine,
// vault, clock-backed randomness or tmpfs.
type Runner struct {
	RT catalog.ContainerRuntime
	// Exec runs one engine command; the default is CombinedOutput.
	Exec  func(*exec.Cmd) ([]byte, error)
	Creds CredentialStore
	// NodeDir is the host node config dir; sessions live under NodeDir/sessions.
	NodeDir string
	// RunBase is a memory-backed directory (e.g. the user's runtime dir). The
	// run dir is RunBase/citadel-sessions/<name>.
	RunBase string
	IsTmpfs func(string) (bool, error)
	// Binary is the node's own executable, mounted into the container.
	Binary   string
	UID, GID int
	Rand     io.Reader
	Log      func(format string, args ...any)
	// ClearStop clears a clean-exit overlay before an explicit wake.
	ClearStop func(ctx context.Context, nodeDir, name string) error
	// WaitReady blocks until the holder in the new incarnation answers.
	WaitReady func(ctx context.Context, name, incarnation string) error
}

func (r *Runner) run(cmd *exec.Cmd) ([]byte, error) {
	if r.Exec != nil {
		return r.Exec(cmd)
	}
	return cmd.CombinedOutput()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func (r *Runner) engine(ctx context.Context, args ...string) ([]byte, error) {
	return r.run(r.RT.EngineCommandContext(ctx, args...))
}

// ViewDir is the host directory bound at /run/session for one session. It holds
// the rendered config and the holder's session dir (socket, state, logs), and
// is the node dir `citadel attach` opens for container sessions.
func (r *Runner) ViewDir(name string) string {
	return filepath.Join(r.NodeDir, "csess", name)
}

func (r *Runner) intentPath(name string) string {
	return filepath.Join(r.NodeDir, "session-intent", name+".asleep")
}

// sleepRequested reports the operator's durable sleep intent. It lives outside
// every mount, so neither the container nor the engine can set or clear it.
func (r *Runner) sleepRequested(name string) bool {
	_, err := os.Lstat(r.intentPath(name))
	return err == nil
}

func (r *Runner) setSleepIntent(name string, on bool) error {
	p := r.intentPath(name)
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, nil, 0o600)
}

// prepareViewDir builds the holder's whole node view on the host as the node
// user (0700), so the engine never creates a root-owned parent for the
// holder's session dir.
func (r *Runner) prepareViewDir(name string) (string, error) {
	view := r.ViewDir(name)
	for _, d := range []string{filepath.Dir(view), view, filepath.Join(view, "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", ErrRunDir
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return "", ErrRunDir
		}
	}
	return view, nil
}

// RunDir resolves and verifies the tmpfs run dir for a session. A base that is
// not provably memory-backed is refused: a credential must never reach a disk.
func (r *Runner) RunDir(name string) (string, error) {
	if declaredsession.ValidateName(name) != nil || !filepath.IsAbs(r.RunBase) || r.IsTmpfs == nil {
		return "", ErrRunDir
	}
	if ok, err := r.IsTmpfs(r.RunBase); err != nil || !ok {
		return "", ErrRunDir
	}
	dir := filepath.Join(r.RunBase, "citadel-sessions", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", ErrRunDir
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (info.Mode().Perm()&0o077 != 0 && !isWindows()) {
		return "", ErrRunDir
	}
	return dir, nil
}

func isWindows() bool { return filepath.Separator == '\\' }

// Observed is the engine-side view of one session container, recovered from its
// labels. Exists=false with no error is a confirmed absence.
type Observed struct {
	Exists    bool
	State     string
	ExitCode  int
	HasRecord bool
	Record    Record
	// Pid is the container's PID 1 as the HOST sees it; attach compares it with
	// the socket peer.
	Pid int
}

func isNotFound(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "no such")
}

// Observe reads one session container by name. An engine failure that is not a
// definite "no such container" is an error, never a silent absence.
func (r *Runner) Observe(ctx context.Context, name string) (Observed, error) {
	if declaredsession.ValidateName(name) != nil {
		return Observed{}, ErrSpec
	}
	out, err := r.engine(ctx, "inspect", "--format",
		"{{.State.Status}}|{{.State.ExitCode}}|{{.State.Pid}}|{{.Config.Image}}|{{json .Config.Labels}}", ContainerPrefix+name)
	if err != nil {
		if isNotFound(out) {
			return Observed{}, nil
		}
		return Observed{}, fmt.Errorf("%w: inspect %s: %s", ErrEngine, name, trim(out, err))
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 5)
	if len(parts) != 5 {
		return Observed{}, fmt.Errorf("%w: unreadable inspect output", ErrEngine)
	}
	code, _ := strconv.Atoi(parts[1])
	pid, _ := strconv.Atoi(parts[2])
	obs := Observed{Exists: true, State: parts[0], ExitCode: code, Pid: pid}
	var labels map[string]string
	if json.Unmarshal([]byte(parts[4]), &labels) == nil {
		// The image label must be the image the engine actually created the
		// container from, so a container cannot claim a record it was not
		// built for. (The revision label cannot be bound: edits change it.)
		if rec, ok := RecordFromLabels(labels); ok && rec.Name == name && rec.Image == parts[3] {
			obs.HasRecord, obs.Record = true, rec
		}
	}
	return obs, nil
}

// VolumeExists reports whether the session volume exists.
func (r *Runner) VolumeExists(ctx context.Context, name string) (bool, error) {
	out, err := r.engine(ctx, "volume", "inspect", ContainerPrefix+name+VolumeSuffix)
	if err == nil {
		return true, nil
	}
	if isNotFound(out) {
		return false, nil
	}
	return false, fmt.Errorf("%w: volume inspect %s: %s", ErrEngine, name, trim(out, err))
}

func trim(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" && err != nil {
		s = err.Error()
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func (r *Runner) incarnation() (string, error) {
	src := r.Rand
	if src == nil {
		src = rand.Reader
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(src, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// settleCredentials re-seals any rotated file credentials from runDir and then
// wipes it. If sealing fails the secrets are left in place (tmpfs) and an error
// returned: losing a rotated refresh token silently is worse than a retry.
func (r *Runner) settleCredentials(spec declaredsession.Declaration, runDir string) error {
	if !HasMaterialized(runDir) {
		return nil
	}
	hc, _ := declaredsession.HarnessConfigFor(spec.Harness)
	if len(spec.Credentials) != 0 {
		old, err := r.Creds.Load(spec.Name)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrReseal, err)
		}
		next, changed, err := ResealBundle(runDir, hc, spec.Credentials, old)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrReseal, err)
		}
		if changed {
			if err := r.Creds.Save(spec.Name, next); err != nil {
				return fmt.Errorf("%w: %v", ErrReseal, err)
			}
		}
	}
	return Wipe(runDir)
}

// sessionLock serializes every wake, sleep and re-seal of one session. The lock
// file lives in the host run dir, which is never mounted into the container.
func (r *Runner) sessionLock(runDir string) (func(), error) {
	return acquireLock(filepath.Join(runDir, "lock"))
}

// Wake creates the session's container (and volume on first wake) and starts
// it. It is not idempotent against a running session (ErrAlreadyRunning), a
// concurrent wake/sleep is refused (ErrBusy), and a failure at any step leaves
// nothing half-started: secrets are wiped and only a container THIS call
// started (by the id its own run returned) is removed. The volume is never
// removed.
func (r *Runner) Wake(ctx context.Context, d declaredsession.Declaration) (rec Record, err error) {
	view := r.ViewDir(d.Name)
	spec, err := NewSpec(d, r.Binary, view, r.UID, r.GID)
	if err != nil {
		return Record{}, err
	}
	if err := r.RT.PreflightLimitControllers(Controllers...); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrEngine, err)
	}
	runDir, err := r.RunDir(d.Name)
	if err != nil {
		return Record{}, err
	}
	release, err := r.sessionLock(runDir)
	if err != nil {
		return Record{}, err
	}
	defer release()
	obs, err := r.Observe(ctx, d.Name)
	if err != nil {
		return Record{}, err
	}
	if obs.Exists && !obs.HasRecord {
		return Record{}, fmt.Errorf("%w: a container named %s exists without a session record", ErrSpec, spec.ContainerName)
	}
	if obs.Exists && obs.State == "running" {
		return obs.Record, ErrAlreadyRunning
	}
	// Only a stopped container may be replaced; a paused or transitional one is
	// somebody's live state and is never removed here.
	if obs.Exists && obs.State != "exited" && obs.State != "created" && obs.State != "dead" {
		return Record{}, fmt.Errorf("%w: container is %s; refusing to replace it", ErrSpec, obs.State)
	}
	// A previous wake that never reached sleep (crashed worker) may have left
	// rotated tokens in the run dir: seal them before anything is replaced.
	if err := r.settleCredentials(d, runDir); err != nil {
		return Record{}, err
	}
	var bundle Bundle
	if len(d.Credentials) != 0 {
		if bundle, err = r.Creds.Load(d.Name); err != nil {
			return Record{}, fmt.Errorf("%w: %v", ErrCredential, err)
		}
		if err := ValidateBundle(d.Credentials, bundle); err != nil {
			return Record{}, err
		}
	}
	if _, err := r.prepareViewDir(d.Name); err != nil {
		return Record{}, err
	}
	if r.ClearStop != nil {
		if err := r.ClearStop(ctx, view, d.Name); err != nil {
			return Record{}, err
		}
	}
	inc, err := r.incarnation()
	if err != nil {
		return Record{}, ErrSpec
	}
	rec = Record{Name: d.Name, Revision: d.Revision, Volume: spec.VolumeName, Harness: d.Harness, Image: spec.Image, Incarnation: inc}

	if err := r.ensureVolume(ctx, spec, rec); err != nil {
		return Record{}, err
	}
	if out, initErr := r.engine(ctx, buildInitArgs(spec, r.RT.IsPodman())...); initErr != nil {
		return Record{}, fmt.Errorf("%w: prepare volume: %s", ErrEngine, trim(out, initErr))
	}
	cfg, err := RenderConfig(d)
	if err != nil {
		return Record{}, ErrSpec
	}
	if err := writePrivate(filepath.Join(view, "sessions.yaml"), cfg); err != nil {
		return Record{}, ErrRunDir
	}
	mat, err := Materialize(runDir, spec.Config, d.Credentials, bundle)
	if err != nil {
		return Record{}, err
	}
	// Past this point the operator has chosen awake.
	if err := r.setSleepIntent(d.Name, false); err != nil {
		_ = Wipe(runDir)
		return Record{}, ErrRunDir
	}
	ownID := ""
	defer func() {
		if err == nil {
			return
		}
		if ownID != "" {
			bg := context.WithoutCancel(ctx)
			_, _ = r.engine(bg, "stop", "-t", strconv.Itoa(StopGraceSeconds), ownID)
			if serr := r.settleCredentials(d, runDir); serr != nil {
				err = errors.Join(err, serr)
			}
			_, _ = r.engine(bg, "rm", "-f", ownID)
		} else {
			_ = Wipe(runDir)
		}
	}()
	if obs.Exists {
		if out, rmErr := r.engine(ctx, "rm", "-f", spec.ContainerName); rmErr != nil && !isNotFound(out) {
			return Record{}, fmt.Errorf("%w: remove stale container: %s", ErrEngine, trim(out, rmErr))
		}
	}
	out, runErr := r.engine(ctx, append([]string{"run"}, buildRunArgs(spec, runInputs{RunDir: runDir, Incarnation: inc, Materialized: mat, Podman: r.RT.IsPodman()}, rec)...)...)
	if runErr != nil {
		return Record{}, fmt.Errorf("%w: run: %s", ErrEngine, trim(out, runErr))
	}
	ownID = lastLine(out)
	if r.WaitReady != nil {
		if err = r.WaitReady(ctx, d.Name, inc); err != nil {
			return Record{}, fmt.Errorf("%w: holder not ready: %v", ErrEngine, err)
		}
	}
	return rec, nil
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (r *Runner) ensureVolume(ctx context.Context, spec *Spec, rec Record) error {
	exists, err := r.VolumeExists(ctx, spec.Name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	vrec := rec
	vrec.Incarnation = ""
	if out, err := r.engine(ctx, buildVolumeCreateArgs(spec, vrec)...); err != nil {
		return fmt.Errorf("%w: volume create: %s", ErrEngine, trim(out, err))
	}
	return nil
}

// Sleep records the operator's sleep intent (so a failed sleep is never undone
// by the boot reconciler), stops the container gracefully, re-seals rotated
// credentials, wipes the run dir and removes the container. The volume stays.
// If re-sealing fails the container stays stopped and the secrets stay on tmpfs
// for a retry.
func (r *Runner) Sleep(ctx context.Context, d declaredsession.Declaration) error {
	if declaredsession.ValidateName(d.Name) != nil {
		return ErrSpec
	}
	runDir, err := r.RunDir(d.Name)
	if err != nil {
		return err
	}
	release, err := r.sessionLock(runDir)
	if err != nil {
		return err
	}
	defer release()
	obs, err := r.Observe(ctx, d.Name)
	if err != nil {
		return err
	}
	if obs.Exists && !obs.HasRecord {
		return fmt.Errorf("%w: %s%s is not a session container", ErrSpec, ContainerPrefix, d.Name)
	}
	if err := r.setSleepIntent(d.Name, true); err != nil {
		return ErrRunDir
	}
	if obs.Exists && obs.State == "running" {
		if out, stopErr := r.engine(ctx, "stop", "-t", strconv.Itoa(StopGraceSeconds), ContainerPrefix+d.Name); stopErr != nil && !isNotFound(out) {
			return fmt.Errorf("%w: stop: %s", ErrEngine, trim(out, stopErr))
		}
	}
	if err := r.settleCredentials(d, runDir); err != nil {
		return err
	}
	if obs.Exists {
		if out, rmErr := r.engine(ctx, "rm", "-f", ContainerPrefix+d.Name); rmErr != nil && !isNotFound(out) {
			return fmt.Errorf("%w: remove: %s", ErrEngine, trim(out, rmErr))
		}
	}
	return nil
}

// ResealOnce captures a token the awake harness rotated into the sealed bundle,
// without waiting for sleep. This is what makes rotation survive a reboot: tmpfs
// does not. It skips (nil) when another wake/sleep holds the session, and never
// wipes anything.
func (r *Runner) ResealOnce(d declaredsession.Declaration) error {
	if len(d.Credentials) == 0 {
		return nil
	}
	runDir, err := r.RunDir(d.Name)
	if err != nil {
		return err
	}
	release, err := r.sessionLock(runDir)
	if errors.Is(err, ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	if !HasMaterialized(runDir) {
		return nil
	}
	hc, _ := declaredsession.HarnessConfigFor(d.Harness)
	old, err := r.Creds.Load(d.Name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReseal, err)
	}
	next, changed, err := ResealBundle(runDir, hc, d.Credentials, old)
	if err != nil || !changed {
		return err
	}
	if err := r.Creds.Save(d.Name, next); err != nil {
		return fmt.Errorf("%w: %v", ErrReseal, err)
	}
	return nil
}
