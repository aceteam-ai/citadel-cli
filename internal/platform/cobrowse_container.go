// internal/platform/cobrowse_container.go
//
// containerRunner: the browserRunner that runs one co-browse session per
// browser-runtime container (aceteam-ai/citadel-cli#1760, epic #1757).
//
// internal/browserruntime owns the run contract (argv, labels, loopback
// publishes, network name). This file owns the lifecycle the contract leaves to
// the runner: a fresh per-session network, the CDP identity check, the label
// sweep, re-adoption across a restart, and the host-runner fallback.
//
// Every engine call goes through the ContainerEngine seam (satisfied by
// catalog.ContainerRuntime, which platform cannot import).
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/browserruntime"
)

// ContainerEngine is the slice of catalog.ContainerRuntime the runner needs.
type ContainerEngine interface {
	EngineCommandContext(ctx context.Context, args ...string) *exec.Cmd
	IsPodman() bool
	Label() string
}

// ContainerLimits are per-session resource limits; empty values use the
// contract defaults in browserruntime.RunSpec.
type ContainerLimits struct {
	CPUs, Memory, ShmSize string
	// DiskCacheMB bounds the browser's disk cache inside the profile mount.
	DiskCacheMB int
}

// BrowserRuntimeConfig wires the container runner. Image is the digest-pinned
// ref of the release (browserruntime.PinnedImage); empty means no runtime.
type BrowserRuntimeConfig struct {
	Engine ContainerEngine
	Image  string
	// Slots is the shared concurrent-session count from device config; 0 keeps
	// the default. CITADEL_COBROWSE_MAX_SESSIONS still overrides it.
	Slots  int
	Limits ContainerLimits
}

const (
	defaultDiskCacheMB       = 256
	containerSessionMaxSecs  = 12 * 3600
	containerHealthTTL       = 30 * time.Second
	containerEngineCallLimit = 60 * time.Second
	containerPullLimit       = 10 * time.Minute
	containerNamePrefix      = "aceteam-brc-"
)

// containerInternalCDPPort is Chromium's loopback-internal debug port in the
// image (MEETING_CDP_INTERNAL_PORT); socat forwards the published port to it.
const containerInternalCDPPort = 9222

var containerIDRe = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

type engineExecFunc func(ctx context.Context, args ...string) (string, error)

func engineExecFor(e ContainerEngine) engineExecFunc {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := e.EngineCommandContext(ctx, args...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if len(msg) > 300 {
				msg = msg[:300]
			}
			return out.String(), fmt.Errorf("%s: %w: %s", args[0], err, msg)
		}
		return out.String(), nil
	}
}

type containerRunner struct {
	eng      engineExecFunc
	podman   bool
	label    string
	image    string
	limits   ContainerLimits
	uid, gid int

	mu         sync.Mutex
	healthAt   time.Time
	healthErr  error
	imageReady bool
	now        func() time.Time

	// Seams for tests; defaults talk to the published loopback ports.
	startBrowser  func(ctx context.Context, ctrlPort int, id string) error
	chromeRunning func(ctx context.Context, ctrlPort int, id string) bool
	cdpReady      func(ctx context.Context, port int) error
	browserIDAt   func(ctx context.Context, port int) (string, error)
	navigate      func(ctx context.Context, port int, url string) error
	pollEvery     time.Duration
}

func newContainerRunner(cfg BrowserRuntimeConfig) *containerRunner {
	r := &containerRunner{
		eng:    engineExecFor(cfg.Engine),
		podman: cfg.Engine.IsPodman(),
		label:  cfg.Engine.Label(),
		image:  cfg.Image,
		limits: cfg.Limits,
		uid:    os.Getuid(),
		gid:    os.Getgid(),
		now:    time.Now,

		startBrowser:  defaultStartBrowser,
		chromeRunning: defaultChromeRunning,
		cdpReady: func(ctx context.Context, port int) error {
			return waitForCDPReadyContext(ctx, port, cobrowseCDPReadyTimeout())
		},
		browserIDAt: func(ctx context.Context, port int) (string, error) {
			ws, err := fetchCDPBrowserWSContext(ctx, port)
			if err != nil {
				return "", err
			}
			return browserIDFromWS(ws)
		},
		navigate: func(ctx context.Context, port int, url string) error {
			_, err := cdpCommandContext(ctx, port, "Page.navigate", map[string]any{"url": url})
			return err
		},
		pollEvery: 2 * time.Second,
	}
	if r.limits.DiskCacheMB <= 0 {
		r.limits.DiskCacheMB = defaultDiskCacheMB
	}
	return r
}

func browserIDFromWS(ws string) (string, error) {
	const marker = "/devtools/browser/"
	i := strings.LastIndex(ws, marker)
	if i < 0 || i+len(marker) == len(ws) {
		return "", errors.New("endpoint reports no browser id")
	}
	return ws[i+len(marker):], nil
}

func (r *containerRunner) call(parent context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, containerEngineCallLimit)
	defer cancel()
	return r.eng(ctx, args...)
}

// healthy reports whether sessions can run in containers now: a reachable
// engine, a usable uid, and the pinned image present (pulled when allowPull).
// Results are cached for containerHealthTTL.
func (r *containerRunner) healthy(allowPull bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.image == "" {
		return errors.New("no pinned browser-runtime image in this release")
	}
	if r.uid <= 0 || r.gid <= 0 {
		return errors.New("containers need a non-root host user with a numeric uid")
	}
	if !r.healthAt.IsZero() && r.now().Sub(r.healthAt) < containerHealthTTL && (r.healthErr != nil || r.imageReady || !allowPull) {
		return r.healthErr
	}
	err := r.checkLocked(allowPull)
	if !(err == nil && !r.imageReady) { // an unpulled-but-reachable probe is not cached as final
		r.healthAt = r.now()
	}
	r.healthErr = err
	return err
}

func (r *containerRunner) checkLocked(allowPull bool) error {
	ctx := context.Background()
	if _, err := r.call(ctx, "info", "--format", "{{.ID}}"); err != nil {
		return fmt.Errorf("container engine unreachable: %w", err)
	}
	out, err := r.call(ctx, "image", "inspect", "--format", `{{index .Config.Labels "`+browserruntime.ImageLabelContract+`"}}`, r.image)
	if err != nil {
		if !allowPull {
			return nil // present-or-pullable: Launch pulls it
		}
		pctx, cancel := context.WithTimeout(ctx, containerPullLimit)
		defer cancel()
		if _, perr := r.eng(pctx, "pull", r.image); perr != nil {
			return fmt.Errorf("browser-runtime image is not present and could not be pulled: %w", perr)
		}
		out, err = r.call(ctx, "image", "inspect", "--format", `{{index .Config.Labels "`+browserruntime.ImageLabelContract+`"}}`, r.image)
		if err != nil {
			return fmt.Errorf("inspect browser-runtime image: %w", err)
		}
	}
	if got := strings.TrimSpace(out); got != browserruntime.ContractVersion {
		return fmt.Errorf("browser-runtime image contract %q is not %q", got, browserruntime.ContractVersion)
	}
	r.imageReady = true
	return nil
}

func (r *containerRunner) Name() string { return "container" }

func (r *containerRunner) Discover() (BrowserInfo, error) {
	if err := r.healthy(false); err != nil {
		return BrowserInfo{}, fmt.Errorf("%w: %v", ErrNoBrowser, err)
	}
	return BrowserInfo{Path: r.image, Kind: "chromium"}, nil
}

func (r *containerRunner) PrepareBaseDir(string) error { return nil }

func containerName(id string) string { return containerNamePrefix + id }

// Launch runs one session container and returns once CDP answers and its
// identity is verified. Every failure after the network exists removes what was
// created.
func (r *containerRunner) Launch(profileDir, startURL string, lo LaunchOptions) (proc *cobrowseProc, err error) {
	id := filepath.Base(profileDir)
	if !sessionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("container runner: bad session id %q", id)
	}
	if err := r.healthy(true); err != nil {
		return nil, err
	}
	pruneProfileCaches(profileDir, int64(r.limits.DiskCacheMB)*1024*1024)

	spec := browserruntime.RunSpec{
		Image: r.image, Network: browserruntime.NetworkName(id), SessionID: id,
		Purpose: "cobrowse", UID: r.uid, GID: r.gid, ProfileDir: profileDir,
		ShmSize: r.limits.ShmSize, CPUs: r.limits.CPUs, Memory: r.limits.Memory,
	}
	args, err := spec.Args()
	if err != nil {
		return nil, err
	}
	extras := []string{"--name", containerName(id), "--pull=never",
		"-e", "MEETING_DISK_CACHE_MB=" + strconv.Itoa(r.limits.DiskCacheMB)}
	if r.podman {
		extras = append(extras, "--userns=keep-id")
	}
	args = append(append(append([]string{}, args[:2]...), extras...), args[2:]...)

	ctx, cancel := context.WithTimeout(context.Background(), cobrowseCDPReadyTimeout()+containerEngineCallLimit)
	defer cancel()

	// A fresh network for this session only; "already exists" is a failure.
	netArgs := []string{"network", "create", "--driver", "bridge",
		"--label", browserruntime.LabelRuntime + "=1", "--label", browserruntime.LabelSession + "=" + id}
	if r.podman {
		netArgs = append(netArgs, "--opt", "isolate=strict")
	}
	if _, err := r.call(ctx, append(netArgs, spec.Network)...); err != nil {
		return nil, fmt.Errorf("create session network: %w", err)
	}
	cid := ""
	defer func() {
		if err != nil {
			r.removeSession(cid, spec.Network)
		}
	}()
	out, err := r.call(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("start browser-runtime container: %w", err)
	}
	cid = lastLine(out)
	if !containerIDRe.MatchString(cid) {
		cid = ""
		return nil, fmt.Errorf("engine returned an unexpected container id")
	}
	if cid, err = r.fullID(ctx, cid); err != nil {
		return nil, err
	}
	ctrlPort, err := r.publishedPort(ctx, cid, browserruntime.ControlContainerPort)
	if err != nil {
		return nil, err
	}
	cdpPort, err := r.publishedPort(ctx, cid, browserruntime.CDPContainerPort)
	if err != nil {
		return nil, err
	}
	if err = r.checkIsolation(ctx, cid, id); err != nil {
		return nil, err
	}
	if err = r.startBrowser(ctx, ctrlPort, id); err != nil {
		return nil, fmt.Errorf("start browser in container: %w", err)
	}
	if err = r.cdpReady(ctx, cdpPort); err != nil {
		return nil, fmt.Errorf("browser launched but CDP not ready: %w", err)
	}
	browserID, err := r.verifyCDPIdentity(ctx, cid, cdpPort, "")
	if err != nil {
		return nil, err
	}
	if startURL != "" {
		if err = r.navigate(ctx, cdpPort, startURL); err != nil {
			return nil, fmt.Errorf("open start page: %w", err)
		}
	}
	started := r.startedAt(ctx, cid)
	return r.newProc(id, cid, spec.Network, started, ctrlPort, cdpPort, browserID, profileDir), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

type inspectDoc struct {
	ID    string `json:"Id"`
	State struct {
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (r *containerRunner) inspect(ctx context.Context, cid string) (inspectDoc, error) {
	var docs []inspectDoc
	out, err := r.call(ctx, "inspect", "--format", "{{json .}}", cid)
	if err != nil {
		return inspectDoc{}, err
	}
	var d inspectDoc
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &d); jerr != nil {
		if jerr2 := json.Unmarshal([]byte(strings.TrimSpace(out)), &docs); jerr2 != nil || len(docs) != 1 {
			return inspectDoc{}, errors.New("unparseable inspect output")
		}
		d = docs[0]
	}
	return d, nil
}

// fullID resolves a possibly short id to the full container id.
func (r *containerRunner) fullID(ctx context.Context, cid string) (string, error) {
	d, err := r.inspect(ctx, cid)
	if err != nil {
		return "", fmt.Errorf("inspect new container: %w", err)
	}
	if !containerIDRe.MatchString(d.ID) || !strings.HasPrefix(d.ID, cid) {
		return "", errors.New("inspect reported a different container id")
	}
	return d.ID, nil
}

func (r *containerRunner) startedAt(ctx context.Context, cid string) string {
	if d, err := r.inspect(ctx, cid); err == nil {
		return d.State.StartedAt
	}
	return ""
}

func (r *containerRunner) publishedPort(ctx context.Context, cid string, cport int) (int, error) {
	out, err := r.call(ctx, "port", cid, strconv.Itoa(cport)+"/tcp")
	if err != nil {
		return 0, fmt.Errorf("read published port %d: %w", cport, err)
	}
	return browserruntime.ParsePublishedPort(out)
}

// checkIsolation enforces the per-session network duties: the container is on
// that network and no other, and no other container is on it.
func (r *containerRunner) checkIsolation(ctx context.Context, cid, id string) error {
	want := browserruntime.NetworkName(id)
	d, err := r.inspect(ctx, cid)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	if len(d.NetworkSettings.Networks) != 1 {
		return errors.New("container is not attached to exactly its session network")
	}
	if _, ok := d.NetworkSettings.Networks[want]; !ok {
		return errors.New("container is not on its session network")
	}
	out, err := r.call(ctx, "ps", "-a", "-q", "--no-trunc", "--filter", "network="+want)
	if err != nil {
		return fmt.Errorf("list session network members: %w", err)
	}
	for _, line := range strings.Fields(out) {
		if line != cid {
			return errors.New("another container is attached to the session network")
		}
	}
	return nil
}

// verifyCDPIdentity proves the browser answering the published loopback port is
// the one inside our container: the id read through the port must equal the id
// the browser reports to a process running inside the container, and (when
// known) the id recorded at launch.
func (r *containerRunner) verifyCDPIdentity(ctx context.Context, cid string, port int, recorded string) (string, error) {
	hostID, err := r.browserIDAt(ctx, port)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errCDPCheck, err)
	}
	out, err := r.call(ctx, "exec", cid, "python3", "-c",
		"import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:"+
			strconv.Itoa(containerInternalCDPPort)+"/json/version',timeout=5).read().decode())")
	if err != nil {
		return "", fmt.Errorf("%w: in-container probe: %v", errCDPCheck, err)
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); jerr != nil {
		return "", fmt.Errorf("%w: in-container probe unreadable", errCDPCheck)
	}
	inID, err := browserIDFromWS(v.WS)
	if err != nil || inID != hostID {
		return "", errCDPImpostor
	}
	if recorded != "" && recorded != hostID {
		return "", errCDPImpostor
	}
	return hostID, nil
}

func (r *containerRunner) removeSession(cid, network string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerEngineCallLimit)
	defer cancel()
	var errs []error
	if cid != "" {
		if _, err := r.eng(ctx, "rm", "-f", cid); err != nil && !strings.Contains(err.Error(), "No such") {
			errs = append(errs, err)
		}
	}
	if network != "" {
		if _, err := r.eng(ctx, "network", "rm", network); err != nil && !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "No such") {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// newProc builds the cobrowseProc for a verified container and starts the
// watcher that closes exited when the browser or container is gone.
func (r *containerRunner) newProc(id, cid, network, started string, ctrlPort, cdpPort int, browserID, profileDir string) *cobrowseProc {
	exited := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(r.pollEvery)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			gone := !r.chromeRunning(ctx, ctrlPort, id)
			if !gone && n%3 == 0 {
				if d, err := r.inspect(ctx, cid); err == nil && !d.State.Running {
					gone = true
				}
			}
			cancel()
			n++
			if gone {
				return
			}
		}
	}()
	var once sync.Once
	var stopErr error
	return &cobrowseProc{
		debugPort:      cdpPort,
		browserID:      browserID,
		browser:        "chromium",
		exited:         exited,
		containerID:    cid,
		containerNet:   network,
		containerStart: started,
		containerCtrl:  ctrlPort,
		containerMount: profileDir,
		stop: func() error {
			once.Do(func() {
				close(done)
				stopErr = r.removeSession(cid, network)
			})
			return stopErr
		},
	}
}

// ---- meetingd control plane (published loopback port only) ----

var controlHTTPClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func defaultStartBrowser(ctx context.Context, ctrlPort int, id string) error {
	body, _ := json.Marshal(map[string]any{"session_id": id, "max_duration_seconds": containerSessionMaxSecs})
	var last error
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/sessions", ctrlPort), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := controlHTTPClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusCreated {
				return nil
			}
			if resp.StatusCode != http.StatusServiceUnavailable { // 503: still booting
				return fmt.Errorf("control API answered %s", resp.Status)
			}
			last = fmt.Errorf("control API answered %s", resp.Status)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("control API not ready: %v", last)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func defaultChromeRunning(ctx context.Context, ctrlPort int, id string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/sessions/%s", ctrlPort, id), nil)
	resp, err := controlHTTPClient.Do(req)
	if err != nil {
		return true // an unreachable control API alone is not proof; inspect decides
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false
	}
	var v struct {
		Running bool `json:"chrome_running"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&v) != nil {
		return true
	}
	return v.Running
}

// ---- re-adoption ----

// adoptContainer verifies a recorded container session and returns its proc.
// Everything fails closed: container id, start time, labels, running state,
// network isolation, published ports and CDP identity must all still match.
func (r *containerRunner) adoptContainer(rec sessionRecord, profileDir string) (*cobrowseProc, error) {
	if !containerIDRe.MatchString(rec.ContainerID) || len(rec.ContainerID) != 64 {
		return nil, errors.New("record has no full container id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), containerEngineCallLimit)
	defer cancel()
	d, err := r.inspect(ctx, rec.ContainerID)
	if err != nil {
		return nil, fmt.Errorf("container is gone: %w", err)
	}
	if d.ID != rec.ContainerID {
		return nil, errors.New("container id changed")
	}
	if !d.State.Running {
		return nil, errors.New("container is not running")
	}
	if rec.ContainerStart == "" || d.State.StartedAt != rec.ContainerStart {
		return nil, errors.New("container was restarted since the record")
	}
	l := d.Config.Labels
	if l[browserruntime.LabelRuntime] != "1" || l[browserruntime.LabelSession] != rec.ID {
		return nil, errors.New("container labels do not match the session")
	}
	if err := r.checkIsolation(ctx, rec.ContainerID, rec.ID); err != nil {
		return nil, err
	}
	cdpPort, err := r.publishedPort(ctx, rec.ContainerID, browserruntime.CDPContainerPort)
	if err != nil || cdpPort != rec.DebugPort {
		return nil, errors.New("published CDP port changed")
	}
	ctrlPort, err := r.publishedPort(ctx, rec.ContainerID, browserruntime.ControlContainerPort)
	if err != nil || ctrlPort != rec.ControlPort {
		return nil, errors.New("published control port changed")
	}
	if rec.BrowserID == "" {
		return nil, errors.New("no browser id recorded")
	}
	if _, err := r.verifyCDPIdentity(ctx, rec.ContainerID, cdpPort, rec.BrowserID); err != nil {
		return nil, err
	}
	return r.newProc(rec.ID, rec.ContainerID, browserruntime.NetworkName(rec.ID), rec.ContainerStart, ctrlPort, cdpPort, rec.BrowserID, profileDir), nil
}

// discardContainer removes what a record that failed verification names, but
// only the container whose labels still say it is this session's.
func (r *containerRunner) discardContainer(rec sessionRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), containerEngineCallLimit)
	defer cancel()
	cid := ""
	if containerIDRe.MatchString(rec.ContainerID) {
		if d, err := r.inspect(ctx, rec.ContainerID); err == nil && d.ID == rec.ContainerID &&
			d.Config.Labels[browserruntime.LabelRuntime] == "1" && d.Config.Labels[browserruntime.LabelSession] == rec.ID {
			cid = rec.ContainerID
		}
	}
	_ = r.removeSession(cid, browserruntime.NetworkName(rec.ID))
}

// sweepContainers removes browser-runtime containers and networks whose session
// is not live. Only objects carrying citadel's labels are considered.
func (r *containerRunner) sweepContainers(live func(id string) bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*containerEngineCallLimit)
	defer cancel()
	filters, _ := browserruntime.SessionFilterArgs("")
	out, err := r.call(ctx, append([]string{"ps", "-a", "--no-trunc", "--format", `{{.ID}} {{.Label "` + browserruntime.LabelSession + `"}}`}, filters...)...)
	if err != nil {
		log.Printf("[cobrowse] container sweep skipped: %v", err)
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !containerIDRe.MatchString(f[0]) || live(f[1]) {
			continue
		}
		log.Printf("[cobrowse] removing orphaned browser-runtime container for session %s", f[1])
		_ = r.removeSession(f[0], "")
	}
	out, err = r.call(ctx, "network", "ls", "--format", "{{.Name}}",
		"--filter", "label="+browserruntime.LabelRuntime+"=1", "--filter", "name="+browserruntime.NetworkName(""))
	if err != nil {
		return
	}
	for _, name := range strings.Fields(out) {
		id := strings.TrimPrefix(name, browserruntime.NetworkName(""))
		if id == name || !sessionIDPattern.MatchString(id) || live(id) {
			continue
		}
		_ = r.removeSession("", name)
	}
}

// ---- disk cache bound ----

var browserCacheDirs = []string{"Cache", "Code Cache", "GPUCache", "GrShaderCache", "ShaderCache", "DawnCache"}

func dirSize(root string) int64 {
	var n int64
	filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// pruneProfileCaches removes the browser's cache dirs from a profile before
// launch when they exceed max bytes (the browser also gets --disk-cache-size).
func pruneProfileCaches(profileDir string, max int64) {
	var dirs []string
	for _, base := range []string{profileDir, filepath.Join(profileDir, "Default")} {
		for _, n := range browserCacheDirs {
			if fi, err := os.Lstat(filepath.Join(base, n)); err == nil && fi.IsDir() {
				dirs = append(dirs, filepath.Join(base, n))
			}
		}
	}
	var total int64
	for _, d := range dirs {
		total += dirSize(d)
	}
	if total <= max {
		return
	}
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

// browserFilePath maps a host path under the session's profile dir to where the
// browser inside the container sees it. Host runners return it unchanged.
func (p *cobrowseProc) browserFilePath(hostPath string) string {
	if p == nil || p.containerMount == "" {
		return hostPath
	}
	rel, err := filepath.Rel(p.containerMount, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return hostPath
	}
	return browserruntime.ProfileMountPath + "/" + filepath.ToSlash(rel)
}

// ---- runner selection ----

// containerSweeper and containerAdopter are the optional halves of the
// selecting runner the session manager reaches through activeBrowserRunner.
type containerSweeper interface {
	sweepContainers(live func(id string) bool)
}

type containerAdopter interface {
	adoptContainer(rec sessionRecord, dir string) (*cobrowseProc, error)
	discardContainer(rec sessionRecord)
}

// selectingRunner runs the node-wide manager's sessions in containers while the
// runtime is healthy and otherwise on the host runner (Jason D3: capability
// based, the fallback logged once per distinct reason).
type selectingRunner struct {
	host browserRunner

	mu        sync.Mutex
	container *containerRunner
	lastWhy   string
}

func (s *selectingRunner) cr() *containerRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.container
}

func (s *selectingRunner) Name() string {
	if c := s.cr(); c != nil && c.healthy(false) == nil {
		return c.Name()
	}
	return s.host.Name()
}

func (s *selectingRunner) Discover() (BrowserInfo, error) {
	if c := s.cr(); c != nil {
		if info, err := c.Discover(); err == nil {
			return info, nil
		}
	}
	return s.host.Discover()
}

func (s *selectingRunner) PrepareBaseDir(dir string) error { return s.host.PrepareBaseDir(dir) }

func (s *selectingRunner) Launch(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error) {
	if c := s.cr(); c != nil && lo.NodeWide {
		err := c.healthy(true)
		if err == nil {
			return c.Launch(profileDir, startURL, lo)
		}
		s.logFallback(err)
	}
	return s.host.Launch(profileDir, startURL, lo)
}

func (s *selectingRunner) logFallback(err error) {
	why := err.Error()
	s.mu.Lock()
	same := s.lastWhy == why
	s.lastWhy = why
	s.mu.Unlock()
	if !same {
		log.Printf("[cobrowse] container browser runtime unavailable, using the host browser runner: %v", err)
	}
}

func (s *selectingRunner) SweepOrphans(baseDir string) { s.host.SweepOrphans(baseDir) }

func (s *selectingRunner) sweepContainers(live func(string) bool) {
	if c := s.cr(); c != nil {
		c.sweepContainers(live)
	}
}

var errNoContainerRuntime = errors.New("container runtime is not configured")

func (s *selectingRunner) adoptContainer(rec sessionRecord, dir string) (*cobrowseProc, error) {
	c := s.cr()
	if c == nil {
		return nil, errNoContainerRuntime
	}
	return c.adoptContainer(rec, dir)
}

func (s *selectingRunner) discardContainer(rec sessionRecord) {
	if c := s.cr(); c != nil {
		c.discardContainer(rec)
	}
}

// ConfigureBrowserRuntime enables the container runner for the node-wide
// co-browse manager and applies the shared slot count. The worker calls it once
// at startup, before StartManagedCobrowse. A nil Engine or empty Image leaves
// the host runner in charge.
func ConfigureBrowserRuntime(cfg BrowserRuntimeConfig) {
	if cfg.Slots > 0 {
		SetBrowserSlots(cfg.Slots)
	}
	if cfg.Engine == nil || cfg.Image == "" {
		return
	}
	browserSelector.mu.Lock()
	browserSelector.container = newContainerRunner(cfg)
	browserSelector.mu.Unlock()
}

// recoverContainer re-adopts one live container record or expires it. It returns
// the expired record (for retention accounting) or nil on success.
func (m *CobrowseSessionManager) recoverContainer(rec sessionRecord, dir, regDir string, now time.Time) *sessionRecord {
	ad, _ := activeBrowserRunner.(containerAdopter)
	var proc *cobrowseProc
	err := m.verifyContainerRecord(rec, dir, ad)
	if err == nil {
		proc, err = ad.adoptContainer(rec, dir)
	}
	if err != nil {
		log.Printf("[cobrowse] not adopting container session %s: %v", rec.ID, err)
		if ad != nil {
			ad.discardContainer(rec)
		}
		rec.State = RecordExpired
		rec.EndedReason, rec.EndedDetail = endedReasonLost, ""
		if rec.PendingEnd != nil {
			rec.EndedReason, rec.EndedDetail = rec.PendingEnd.Reason, rec.PendingEnd.Detail
		}
		rec.PendingEnd = nil
		rec.EndedAt = now.UTC().Format(time.RFC3339)
		_ = saveRecord(regDir, rec)
		return &rec
	}
	s := &cobrowseSession{
		id: rec.ID, profile: dir, regDir: regDir, state: SessionRunning, proc: proc,
		explicitHandoff: rec.Handoff, generation: rec.Generation + 1, tracking: m.TargetsEnabled(),
	}
	if t, perr := time.Parse(time.RFC3339, rec.StartedAt); perr == nil {
		s.startedAt = t
	}
	sweepProtected.Store(dir, struct{}{})
	m.mu.Lock()
	if _, exists := m.sessions[rec.ID]; !exists {
		m.sessions[rec.ID] = s
	}
	m.mu.Unlock()
	writeSessionPidfile(dir, os.Getpid(), 0, 0, currentProcessStartToken())
	s.persistRecord()
	if !s.tracking {
		s.startChooserWatch(proc.debugPort)
	}
	log.Printf("[cobrowse] re-adopted container session %s (generation %d)", rec.ID, s.generation)
	return nil
}

// verifyContainerRecord is the container counterpart of verifyAdoptable: the
// record must be ours to take over, describe the plain session dir, and the
// container must then pass adoptContainer's checks (see recoverContainer).
func (m *CobrowseSessionManager) verifyContainerRecord(rec sessionRecord, dir string, ad containerAdopter) error {
	if ad == nil {
		return errNoContainerRuntime
	}
	if rec.Version != sessionRecordVersion || rec.ID == "" || rec.ID != filepath.Base(dir) ||
		rec.Profile != dir || filepath.Dir(dir) != filepath.Clean(m.baseDir) {
		return errors.New("record does not match its directory")
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return errors.New("session dir is not a plain directory")
	}
	if rec.Persistent {
		return errors.New("persistent profile sessions are sealed, not adopted")
	}
	if rec.OwnerPID != os.Getpid() || rec.OwnerStart == 0 || rec.OwnerStart != currentOwnerIdentity() {
		if !detachedWorker.Load() {
			return errors.New("record was not written by this process and this is not the managed worker")
		}
		if rec.OwnerUID == nil || *rec.OwnerUID != os.Getuid() {
			return errors.New("record was written by a different OS user")
		}
		if st, alive, err := procIdentity(rec.OwnerPID); err == nil && alive && st == rec.OwnerStart {
			return errors.New("record owner is still running")
		}
		if !withinDetachGrace(rec) {
			return fmt.Errorf("owner has been gone longer than the %s detach grace", detachGrace())
		}
	}
	return nil
}
