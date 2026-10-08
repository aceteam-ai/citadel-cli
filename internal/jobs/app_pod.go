// internal/jobs/app_pod.go
//
// Hosted-app pod-per-app runner (CRAM slice A1, aceteam#9672). This EXTENDS the
// BYOC launcher in service_payload.go rather than adding a sixth: it reuses that
// file's validators (validateImageRef, validateRuntime) unchanged and adds the
// pod semantics the app runner needs - one pod per app,
// a node-allocated loopback host port, size-derived cgroup limits, cap-drop=ALL,
// and the aceteam.app* labels the heartbeat and lifecycle verbs recover state
// from.
//
// Every container-engine invocation goes through the catalog.ContainerRuntime
// seam (podman preferred, docker fallback), so there are NO literal
// exec.Command("docker"/"podman") sites here (the citadel-cli#1041 CI guard).
// On podman the app is a real pod ("pod create" + "run --pod"); on docker,
// which has no pods, it is a single labeled container with the identical
// publish, limits, and labels, and pod_id is the container id. STATUS/LOGS/
// DESTROY resolve the container by its aceteam.app label (works on both
// engines); only stop/start branch on the engine.
package jobs

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// AppPodNamePrefix is the pod / container name prefix for hosted-app pods. The
// pod (podman) or container (docker) is "aceapp-<short_code>"; the app
// container inside a podman pod is "aceapp-<short_code>-app". The gateway route
// name is "app-<short_code>" (gateway.AppExposePrefix), a DIFFERENT namespace,
// deliberately: "aceapp-" is the runtime object, "app-" is the reserved gateway
// route the operator expose verbs refuse.
const AppPodNamePrefix = "aceapp-"

// App label keys are the durable record: the runner writes no store, so the
// heartbeat, STATUS, and idempotent redeploy all recover host_port / health_path
// / org / deployment off the container labels (aceteam#9672 §3.3). host_port and
// health_path go BEYOND the DoR's app/org/deployment set precisely so redeploy
// can reuse the existing port and the heartbeat can report the health endpoint
// without a separate store.
const (
	AppLabelShortCode  = "aceteam.app"
	AppLabelOrg        = "aceteam.org"
	AppLabelDeployment = "aceteam.deployment"
	AppLabelHostPort   = "aceteam.app.host_port"
	AppLabelHealthPath = "aceteam.app.health_path"
)

// defaultAppStateMountPath is where the app's durable volume mounts inside the
// container when the payload omits state_mount_path.
const defaultAppStateMountPath = "/data"

// defaultAppHealthPath is the readiness path polled when the payload omits one.
const defaultAppHealthPath = "/"

// appLimits is one size tier's concrete cgroup ceiling. DoR §3.7's table; the
// three controllers (cpu/memory/pids) are exactly what E2's
// PreflightLimitControllers verifies rootless podman can enforce.
type appLimits struct {
	CPUs   string // --cpus value
	Memory string // --memory value
	Pids   int    // --pids-limit value
}

// appSizeLimits maps a size tier to its limits (DoR §3.7). Unknown / empty
// resolves to "small".
var appSizeLimits = map[string]appLimits{
	"nano":   {CPUs: "0.5", Memory: "512m", Pids: 256},
	"small":  {CPUs: "1", Memory: "1g", Pids: 512},
	"medium": {CPUs: "2", Memory: "4g", Pids: 1024},
	"large":  {CPUs: "4", Memory: "8g", Pids: 2048},
}

// AppLimitControllers is the controller set an app pod's limits require, so the
// runner can preflight rootless-podman cgroup delegation (E2) before create.
var AppLimitControllers = []string{"cpu", "memory", "pids"}

// AppSpecInput is the resolved-but-unvalidated app deploy request the cmd layer
// hands ParseAppSpec (converted from worker.AppDeployRequest so internal/jobs
// need not import internal/worker).
type AppSpecInput struct {
	ShortCode       string
	Image           string
	ContainerPort   int
	Env             map[string]string
	HealthPath      string
	Size            string
	Runtime         string
	Org             string
	DeploymentID    string
	StateVolumePath string
	StateMountPath  string
}

// AppSpec is the validated, node-resolved form ready to launch. Pure data.
type AppSpec struct {
	ShortCode     string
	PodName       string
	ContainerName string
	Image         string
	Env           map[string]string
	ContainerPort int
	HostPort      int
	HealthPath    string
	Limits        appLimits
	Runtime       string
	// StateVolume is the -v source. Apps always use the per-app engine-managed
	// NAMED volume (equal to NamedVolume, removed on destroy); a caller-supplied
	// state_volume_path is ignored, so this is never a host bind path.
	StateVolume string
	// NamedVolume is the per-app engine-managed volume (StateVolume equals it), so
	// destroy knows to `volume rm` it.
	NamedVolume    string
	StateMountPath string
	Labels         map[string]string
}

// ParseAppSpec validates an AppSpecInput and resolves it into a launchable
// AppSpec, reusing service_payload.go's validators. hostPort is the
// node-allocated loopback port (services.AllocateAppPodPort). A payload
// state_volume_path is ignored: apps always get an engine-managed per-app named
// volume, so no caller-chosen host path is ever mounted. Pure. (homeDir is
// retained for signature compatibility and is currently unused.)
func ParseAppSpec(in AppSpecInput, hostPort int, homeDir string) (*AppSpec, error) {
	shortCode := strings.TrimSpace(in.ShortCode)
	if shortCode == "" {
		return nil, fmt.Errorf("app spec missing short_code")
	}
	// The short code steers the pod / container / named-volume names and the
	// gateway route name, so a crafted value must never leak into them. Require
	// the same grammar the gateway route name needs: lowercase alphanumerics and
	// single dashes.
	if !isValidJobsAppShortCode(shortCode) {
		return nil, fmt.Errorf("invalid short_code %q (want lowercase alphanumerics and single dashes)", shortCode)
	}
	image := strings.TrimSpace(in.Image)
	if err := validateImageRef(image); err != nil {
		return nil, err
	}
	if in.ContainerPort < 1024 || in.ContainerPort > 65535 {
		return nil, fmt.Errorf("container_port %d out of range (1024-65535)", in.ContainerPort)
	}
	if hostPort < 1 || hostPort > 65535 {
		return nil, fmt.Errorf("host_port %d out of range", hostPort)
	}
	runtime := strings.TrimSpace(in.Runtime)
	if err := validateRuntime(runtime); err != nil {
		return nil, err
	}

	limits, ok := appSizeLimits[strings.ToLower(strings.TrimSpace(in.Size))]
	if !ok {
		limits = appSizeLimits["small"]
	}

	mountPath := strings.TrimSpace(in.StateMountPath)
	if mountPath == "" {
		mountPath = defaultAppStateMountPath
	}
	if !strings.HasPrefix(mountPath, "/") {
		return nil, fmt.Errorf("state_mount_path %q must be an absolute container path", mountPath)
	}
	// The -v argument is "source:dest"; a ':' or ',' in either side would split
	// the mount or add options the caller did not intend.
	if strings.ContainsAny(mountPath, ":,") {
		return nil, fmt.Errorf("state_mount_path %q must not contain ':' or ','", mountPath)
	}

	podName := AppPodNamePrefix + shortCode
	spec := &AppSpec{
		ShortCode:      shortCode,
		PodName:        podName,
		ContainerName:  podName + "-app",
		Image:          image,
		Env:            copyEnv(in.Env),
		ContainerPort:  in.ContainerPort,
		HostPort:       hostPort,
		HealthPath:     resolveHealthPath(in.HealthPath),
		Limits:         limits,
		Runtime:        runtime,
		StateMountPath: mountPath,
	}

	// State volume: an app always gets an engine-managed per-app NAMED VOLUME,
	// removed on destroy. A payload state_volume_path is IGNORED. Honoring a host
	// path allowed a check-then-use race (the value validated at parse time was
	// not the one handed to the engine, and a symlink could be swapped in between)
	// and let the caller pick the host path, while the A4 dispatch gate does not
	// forward the field. A named volume has no host path at all, so it removes the
	// escape surface entirely, avoids a root-owned directory under rootful docker,
	// and is garbage-collected on destroy (no stale state when a short_code is
	// reused).
	spec.NamedVolume = podName + "-data"
	spec.StateVolume = spec.NamedVolume

	// PORT is injected so a base-path-agnostic app listens on the port we publish
	// (DoR §3.2 "port from env"); an explicit payload PORT wins.
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if _, ok := spec.Env["PORT"]; !ok {
		spec.Env["PORT"] = strconv.Itoa(in.ContainerPort)
	}

	spec.Labels = map[string]string{
		AppLabelShortCode:  shortCode,
		AppLabelOrg:        strings.TrimSpace(in.Org),
		AppLabelDeployment: strings.TrimSpace(in.DeploymentID),
		AppLabelHostPort:   strconv.Itoa(hostPort),
		AppLabelHealthPath: spec.HealthPath,
	}
	return spec, nil
}

func resolveHealthPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return defaultAppHealthPath
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

func copyEnv(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// buildAppPodCreateArgs returns the args AFTER "pod create" (podman only):
// name, the loopback publish (the pod owns the netns), and the labels.
func buildAppPodCreateArgs(spec *AppSpec) []string {
	args := []string{"--name", spec.PodName,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", spec.HostPort, spec.ContainerPort)}
	args = append(args, labelArgs(spec.Labels)...)
	return args
}

// buildAppContainerRunArgs returns the args AFTER "run" for the app container.
// podman=true joins the pod (which owns the publish); docker=false publishes on
// the container itself. Limits, cap-drop, labels, volume, env, and image are
// identical on both. The image is placed LAST so it can never be read as a flag.
func buildAppContainerRunArgs(spec *AppSpec, podman bool) []string {
	args := []string{"-d"}
	if podman {
		args = append(args, "--pod", spec.PodName, "--name", spec.ContainerName)
	} else {
		args = append(args, "--name", spec.PodName,
			"-p", fmt.Sprintf("127.0.0.1:%d:%d", spec.HostPort, spec.ContainerPort))
	}
	args = append(args,
		"--restart", "unless-stopped",
		"--cap-drop", "ALL",
		"--cpus", spec.Limits.CPUs,
		"--memory", spec.Limits.Memory,
		"--pids-limit", strconv.Itoa(spec.Limits.Pids),
		"-v", fmt.Sprintf("%s:%s", spec.StateVolume, spec.StateMountPath),
	)
	if spec.Runtime != "" {
		args = append(args, "--runtime="+spec.Runtime)
	}
	// Labels also on the container (docker has no pod to carry them; podman gets
	// them on both), so `<engine> ps --filter label=aceteam.app=` resolves it on
	// either engine.
	args = append(args, labelArgs(spec.Labels)...)
	for _, k := range sortedEnvKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	args = append(args, spec.Image)
	return args
}

// labelArgs renders a label map into deterministic ("--label", "k=v") pairs.
func labelArgs(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, "--label", k+"="+labels[k])
	}
	return out
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Live runner
// ---------------------------------------------------------------------------

// AppPodRunner drives the app pod lifecycle over the container runtime seam.
// execFn is an injectable seam so the argv the runner builds is unit-testable
// without a live engine (the same argv-capture pattern as the WhatsApp bridge
// tests).
type AppPodRunner struct {
	rt      catalog.ContainerRuntime
	execFn  func(*exec.Cmd) ([]byte, error)
	homeDir string
	log     func(format string, args ...any)
}

// NewAppPodRunner constructs a runner over rt. A nil log is a no-op.
func NewAppPodRunner(rt catalog.ContainerRuntime, homeDir string, log func(string, ...any)) *AppPodRunner {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &AppPodRunner{
		rt:      rt,
		execFn:  func(c *exec.Cmd) ([]byte, error) { return c.CombinedOutput() },
		homeDir: homeDir,
		log:     log,
	}
}

// WithExec overrides the exec seam (tests).
func (r *AppPodRunner) WithExec(fn func(*exec.Cmd) ([]byte, error)) *AppPodRunner {
	r.execFn = fn
	return r
}

// HomeDir exposes the runner's resolved home dir so the cmd caller can pass it
// into ParseAppSpec without re-resolving it.
func (r *AppPodRunner) HomeDir() string { return r.homeDir }

func (r *AppPodRunner) podman() bool { return r.rt.EngineBin == "podman" }

// AppPodDeployResult is what Deploy returns to the cmd layer.
type AppPodDeployResult struct {
	PodID       string
	ImageDigest string
}

// AppPodInfo is a resolved container's live facts, recovered from its labels.
type AppPodInfo struct {
	ShortCode string
	State     string // running | stopped | not_found
	HostPort  int
	PodID     string
	Image     string
}

// Deploy creates (or recreates) the app's pod and returns its id and image
// digest. It is idempotent by name: an existing pod/container for the short
// code is removed first, so a redelivered APP_DEPLOY converges rather than
// colliding on the name. Health is NOT waited on here (the caller runs a
// detached probe and the heartbeat carries health); Deploy returns as soon as
// the container is running.
func (r *AppPodRunner) Deploy(ctx context.Context, spec *AppSpec) (*AppPodDeployResult, error) {
	// Refuse fast when rootless podman cannot enforce the size limits (E2 /
	// DoR §3.7 / §6's highest-probability landmine), rather than run unlimited.
	if err := r.rt.PreflightLimitControllers(AppLimitControllers...); err != nil {
		return nil, fmt.Errorf("cannot enforce resource limits for hosted app %q: %w", spec.ShortCode, err)
	}

	if out, err := r.run(r.rt.ImagePull(ctx, spec.Image)); err != nil {
		return nil, fmt.Errorf("pull image %q: %s", spec.Image, trimOut(out, err))
	}

	// Remove any prior pod/container for this short code before recreating.
	r.teardown(ctx, spec.ShortCode, spec.PodName)

	if spec.NamedVolume != "" {
		// Create the durable named volume (ignore "already exists").
		_, _ = r.run(r.rt.Volume(ctx, "create", spec.NamedVolume))
	}

	if r.podman() {
		if out, err := r.run(r.rt.PodCreate(ctx, buildAppPodCreateArgs(spec)...)); err != nil {
			return nil, fmt.Errorf("create pod %q: %s", spec.PodName, trimOut(out, err))
		}
	}
	if out, err := r.run(r.rt.Run(ctx, buildAppContainerRunArgs(spec, r.podman())...)); err != nil {
		// Roll back a podman pod that was created but whose container failed, so a
		// retry is not blocked by a half-built pod.
		if r.podman() {
			r.teardown(ctx, spec.ShortCode, spec.PodName)
		}
		return nil, fmt.Errorf("run app container %q: %s", spec.ContainerName, trimOut(out, err))
	}

	podID := r.resolvePodID(ctx, spec)
	return &AppPodDeployResult{PodID: podID, ImageDigest: r.resolveImageDigest(ctx, spec.Image)}, nil
}

// WaitHealthy polls http://127.0.0.1:<hostPort><healthPath> until it answers
// 2xx/3xx or the deadline elapses. Advisory only - the caller runs it detached
// and logs the outcome; the heartbeat is the durable health signal.
func (r *AppPodRunner) WaitHealthy(ctx context.Context, hostPort int, healthPath string, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", hostPort, resolveHealthPath(healthPath))
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("app %s not healthy within %s", url, timeout)
}

// Stop stops the app's pod/container, releasing memory while leaving the pod
// definition, volumes, and gateway route intact (DoR §3.6). Returns the
// resulting state (stopped, or not_found when nothing exists).
func (r *AppPodRunner) Stop(ctx context.Context, shortCode string) (string, error) {
	if !r.exists(ctx, shortCode) {
		return "not_found", nil
	}
	var cmd *exec.Cmd
	if r.podman() {
		cmd = r.rt.PodStop(ctx, AppPodNamePrefix+shortCode)
	} else {
		cmd = r.rt.EngineCommandContext(ctx, "stop", AppPodNamePrefix+shortCode)
	}
	if out, err := r.run(cmd); err != nil {
		return "", fmt.Errorf("stop app %q: %s", shortCode, trimOut(out, err))
	}
	return "stopped", nil
}

// Start starts a stopped app pod/container.
func (r *AppPodRunner) Start(ctx context.Context, shortCode string) (string, error) {
	if !r.exists(ctx, shortCode) {
		return "not_found", nil
	}
	var cmd *exec.Cmd
	if r.podman() {
		cmd = r.rt.PodStart(ctx, AppPodNamePrefix+shortCode)
	} else {
		cmd = r.rt.EngineCommandContext(ctx, "start", AppPodNamePrefix+shortCode)
	}
	if out, err := r.run(cmd); err != nil {
		return "", fmt.Errorf("start app %q: %s", shortCode, trimOut(out, err))
	}
	return "running", nil
}

// Status reports a single app pod's live state, resolved by label.
func (r *AppPodRunner) Status(ctx context.Context, shortCode string) (*AppPodInfo, error) {
	id, ok := r.containerID(ctx, shortCode, true)
	if !ok {
		return &AppPodInfo{ShortCode: shortCode, State: "not_found"}, nil
	}
	info := &AppPodInfo{ShortCode: shortCode, PodID: id}
	if running, _ := r.containerID(ctx, shortCode, false); running != "" {
		info.State = "running"
	} else {
		info.State = "stopped"
	}
	info.HostPort = r.inspectHostPort(ctx, id)
	info.Image = r.inspectImage(ctx, id)
	return info, nil
}

// Logs tails a running app container's logs.
func (r *AppPodRunner) Logs(ctx context.Context, shortCode string, tail int) (string, error) {
	id, ok := r.containerID(ctx, shortCode, true)
	if !ok {
		return "", fmt.Errorf("no hosted app found for short_code %q", shortCode)
	}
	if tail <= 0 {
		tail = 200
	}
	out, err := r.run(r.rt.EngineCommandContext(ctx, "logs", "--tail", strconv.Itoa(tail), id))
	if err != nil {
		return "", fmt.Errorf("read logs for app %q: %s", shortCode, trimOut(out, err))
	}
	return string(out), nil
}

// Destroy removes the app's pod/container and its named volume. The caller
// Unexposes the gateway route FIRST (route dark before teardown). Idempotent.
func (r *AppPodRunner) Destroy(ctx context.Context, shortCode string) error {
	podName := AppPodNamePrefix + shortCode
	teardownErr := r.teardown(ctx, shortCode, podName)
	// Idempotent AND honest. A "no such container" on an already-gone app is
	// success, but a teardown that left the container in place -- OR an engine we
	// cannot reach to confirm removal -- must NEVER be reported as a successful
	// destroy (the A1 review's masked-failure finding and its engine-down variant).
	_, present, checkErr := r.containerPresence(ctx, shortCode, true)
	if checkErr != nil {
		if teardownErr != nil {
			return fmt.Errorf("app %q teardown failed and removal is unverifiable: %w", shortCode, teardownErr)
		}
		return fmt.Errorf("app %q removal is unverifiable: %w", shortCode, checkErr)
	}
	if present {
		if teardownErr != nil {
			return fmt.Errorf("app %q teardown failed: %w", shortCode, teardownErr)
		}
		return fmt.Errorf("app %q is still present after teardown", shortCode)
	}
	// Container confirmed gone: ONLY now remove the per-app named volume. Doing it
	// after confirmation (not before, best-effort) means a failed volume removal
	// cannot silently leave a data volume behind for a later app that reuses this
	// short code. A not-found is success (already gone, or an older bind-mount app
	// that never had a named volume); any other error is surfaced.
	if out, err := r.run(r.rt.Volume(ctx, "rm", "-f", podName+"-data")); err != nil {
		if !isVolumeNotFoundErr(out, err) {
			return fmt.Errorf("app %q: remove state volume: %s", shortCode, trimOut(out, err))
		}
	}
	return nil
}

// List returns every hosted-app container on this node (running or stopped),
// resolved by the aceteam.app label. It backs the heartbeat Apps entries.
func (r *AppPodRunner) List(ctx context.Context) ([]AppPodInfo, error) {
	running := r.shortCodesByState(ctx, true) // status=running set
	all := r.listShortCodes(ctx)              // running + stopped
	out := make([]AppPodInfo, 0, len(all))
	for _, sc := range all {
		id, ok := r.containerID(ctx, sc, true)
		if !ok {
			continue
		}
		state := "stopped"
		if running[sc] {
			state = "running"
		}
		out = append(out, AppPodInfo{
			ShortCode: sc,
			State:     state,
			PodID:     id,
			HostPort:  r.inspectHostPort(ctx, id),
			Image:     r.inspectImage(ctx, id),
		})
	}
	return out, nil
}

// ExistingHostPort reads the host_port label off an existing app container, so
// a redeploy reuses the same port (stable gateway upstream).
func (r *AppPodRunner) ExistingHostPort(ctx context.Context, shortCode string) (int, bool) {
	id, ok := r.containerID(ctx, shortCode, true)
	if !ok {
		return 0, false
	}
	p := r.inspectHostPort(ctx, id)
	return p, p > 0
}

// UsedAppPorts returns the set of host ports currently claimed by hosted-app
// containers, for the allocator's in-use predicate.
func (r *AppPodRunner) UsedAppPorts(ctx context.Context) map[int]bool {
	used := map[int]bool{}
	for _, sc := range r.listShortCodes(ctx) {
		if id, ok := r.containerID(ctx, sc, true); ok {
			if p := r.inspectHostPort(ctx, id); p > 0 {
				used[p] = true
			}
		}
	}
	return used
}

// isValidJobsAppShortCode reports whether the short code is lowercase
// alphanumerics with single dashes. It mirrors internal/worker's
// isValidAppShortCode; the check is duplicated here because internal/worker
// imports internal/jobs (not the reverse) and the short code steers the
// pod / container / named-volume names and the gateway route in this package.
var jobsAppShortCodeRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func isValidJobsAppShortCode(code string) bool {
	return jobsAppShortCodeRe.MatchString(code)
}

// --- helpers ---------------------------------------------------------------

func (r *AppPodRunner) run(cmd *exec.Cmd) ([]byte, error) { return r.execFn(cmd) }

// teardown force-removes any pod/container for the short code. Best-effort:
// each removal ignores "no such object". On podman we remove the pod (which
// removes its containers); on docker we remove the container.
func (r *AppPodRunner) teardown(ctx context.Context, shortCode, podName string) error {
	if r.podman() {
		_, err := r.run(r.rt.EngineCommandContext(ctx, "pod", "rm", "-f", podName))
		return err
	}
	_, err := r.run(r.rt.EngineCommandContext(ctx, "rm", "-f", podName))
	return err
}

func (r *AppPodRunner) exists(ctx context.Context, shortCode string) bool {
	_, ok := r.containerID(ctx, shortCode, true)
	return ok
}

// containerPresence returns the first container id carrying aceteam.app=<shortCode>
// and DISTINGUISHES "listed successfully but not found" (present=false, err=nil)
// from a listing or engine failure (err!=nil). Destroy relies on that
// distinction: an engine it cannot reach must never be read as "the app is gone".
// includeStopped selects `ps -a` (any state) vs `ps` (running only).
func (r *AppPodRunner) containerPresence(ctx context.Context, shortCode string, includeStopped bool) (string, bool, error) {
	args := []string{"ps"}
	if includeStopped {
		args = append(args, "-a")
	}
	args = append(args, "--filter", "label="+AppLabelShortCode+"="+shortCode, "--format", "{{.ID}}")
	out, err := r.run(r.rt.EngineCommandContext(ctx, args...))
	if err != nil {
		return "", false, fmt.Errorf("list container for app %q: %s", shortCode, trimOut(out, err))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			return id, true, nil
		}
	}
	return "", false, nil
}

// containerID is the error-swallowing read-only wrapper the listing and port
// helpers use, where treating an engine error as "absent" is acceptable.
func (r *AppPodRunner) containerID(ctx context.Context, shortCode string, includeStopped bool) (string, bool) {
	id, ok, _ := r.containerPresence(ctx, shortCode, includeStopped)
	return id, ok
}

// listShortCodes returns every hosted-app short code (running or stopped),
// read off the aceteam.app label.
func (r *AppPodRunner) listShortCodes(ctx context.Context) []string {
	return r.readShortCodes(ctx, r.rt.EngineCommandContext(ctx,
		"ps", "-a", "--filter", "label="+AppLabelShortCode,
		"--format", "{{index .Labels \""+AppLabelShortCode+"\"}}"))
}

// shortCodesByState returns the set of running (or with running=false, any)
// hosted-app short codes.
func (r *AppPodRunner) shortCodesByState(ctx context.Context, runningOnly bool) map[string]bool {
	args := []string{"ps"}
	if !runningOnly {
		args = append(args, "-a")
	}
	args = append(args, "--filter", "label="+AppLabelShortCode,
		"--format", "{{index .Labels \""+AppLabelShortCode+"\"}}")
	set := map[string]bool{}
	for _, sc := range r.readShortCodes(ctx, r.rt.EngineCommandContext(ctx, args...)) {
		set[sc] = true
	}
	return set
}

func (r *AppPodRunner) readShortCodes(_ context.Context, cmd *exec.Cmd) []string {
	out, err := r.run(cmd)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var codes []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sc := strings.TrimSpace(line)
		if sc == "" || seen[sc] {
			continue
		}
		seen[sc] = true
		codes = append(codes, sc)
	}
	return codes
}

func (r *AppPodRunner) inspectHostPort(ctx context.Context, id string) int {
	out, err := r.run(r.rt.EngineCommandContext(ctx, "inspect", "--format",
		"{{index .Config.Labels \""+AppLabelHostPort+"\"}}", id))
	if err != nil {
		return 0
	}
	p, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		return 0
	}
	return p
}

func (r *AppPodRunner) inspectImage(ctx context.Context, id string) string {
	out, err := r.run(r.rt.EngineCommandContext(ctx, "inspect", "--format", "{{.Config.Image}}", id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (r *AppPodRunner) resolvePodID(ctx context.Context, spec *AppSpec) string {
	if r.podman() {
		out, err := r.run(r.rt.EngineCommandContext(ctx, "pod", "inspect", "--format", "{{.ID}}", spec.PodName))
		if err == nil {
			if id := strings.TrimSpace(string(out)); id != "" {
				return id
			}
		}
	}
	// docker (or podman pod inspect failed): the container id is the pod id.
	if id, ok := r.containerID(ctx, spec.ShortCode, true); ok {
		return id
	}
	return ""
}

// resolveImageDigest prefers the registry digest (RepoDigests[0]); falls back
// to the local image Id. Reported to the platform as app_deployments.image_digest.
func (r *AppPodRunner) resolveImageDigest(ctx context.Context, image string) string {
	out, err := r.run(r.rt.EngineCommandContext(ctx, "image", "inspect", "--format",
		"{{if .RepoDigests}}{{index .RepoDigests 0}}{{else}}{{.Id}}{{end}}", image))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// trimOut renders a subprocess failure: the combined output when present, else
// the raw error.
func trimOut(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return err.Error()
	}
	return s
}

// isVolumeNotFoundErr reports whether a `volume rm` failure is only "the volume
// does not exist" (docker: "No such volume"; podman: "no such volume" / "no
// volume with name"), which is a success for an idempotent destroy. Any other
// failure (engine down, permission) is a real error the caller must surface.
func isVolumeNotFoundErr(out []byte, err error) bool {
	if err == nil {
		return true
	}
	s := strings.ToLower(strings.TrimSpace(string(out)) + " " + err.Error())
	return strings.Contains(s, "no such volume") ||
		strings.Contains(s, "no volume with name") ||
		strings.Contains(s, "not found")
}
