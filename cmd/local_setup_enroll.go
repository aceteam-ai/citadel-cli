// cmd/local_setup_enroll.go
//
// "Connect this PC to AceTeam" on the setup page (citadel-cli#1867 C2).
// setupEnroller runs one enrollDevice at a time in the background and keeps
// only what the page may show: the stage, the user code, the verification URL,
// and on success the org and node names. Every failure is shown as fixed copy
// keyed by its enrollFailure kind (setupEnrollCopy); no error text from the
// enrollment, the server or the network reaches the page. The device code
// never leaves the core: enrollCode has no field for it, and neither does
// localviewer.SetupEnrollState.
//
// Before anything is saved, checkSetupNodeDir requires the node config dir to
// lie inside this user's own folder, and on Windows the machine-wide config dir
// (which another local user could create and use to redirect it) to be
// absent or administrator-only. It is checked again before the save and before
// the network connect.
package cmd

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"gopkg.in/yaml.v3"
)

// Page stages (localviewer.SetupEnrollState.Stage).
const (
	setupEnrollIdle       = "idle"
	setupEnrollStarting   = "starting"
	setupEnrollWaiting    = "waiting"
	setupEnrollConnecting = "connecting"
	setupEnrollConnected  = "connected"
	setupEnrollAlready    = "already"
	setupEnrollCanceling  = "canceling"
	setupEnrollCanceled   = "canceled"
	setupEnrollFailed     = "failed"
)

// The page's own failures, beside the core's enrollFailure kinds.
const (
	// setupEnrollUntrustedLink: the approval link did not point at the AceTeam
	// service this enrollment talks to, or its code differed from the user
	// code, so it was neither opened nor shown and the enrollment was stopped.
	setupEnrollUntrustedLink enrollFailure = "untrusted_link"
	// setupEnrollUntrustedNodeDir: the node config dir this PC resolves to is
	// not inside this user's own folder, or the machine-wide config dir that
	// can redirect it is not administrator-only.
	setupEnrollUntrustedNodeDir enrollFailure = "untrusted_node_dir"
)

// errSetupNodeDirUntrusted is the cause of a setupEnrollUntrustedNodeDir
// failure, including one found by the recheck inside the enrollment.
var errSetupNodeDirUntrusted = errors.New("the node config dir is not inside this user's own folder, or the machine config dir is not administrator-only")

// setupEnrollCopy is the fixed user copy for every failure kind. A kind
// missing here falls back to setupEnrollDefaultCopy;
// TestSetupEnrollCopyCoversEveryFailureKind keeps the map complete.
var setupEnrollCopy = map[enrollFailure]string{
	enrollBusy:             "A connection is already in progress. Wait for it to finish, or press Cancel.",
	enrollMisconfigured:    "Citadel could not start connecting from this page. Run \"citadel enroll\" in a terminal instead.",
	enrollAlreadyConnected: "This PC is already connected to AceTeam, so nothing was changed.",
	enrollAlreadyEnrolled: "This PC already has an AceTeam registration, so nothing was changed. It may be from an earlier setup, " +
		"or from a try that did not finish (for example, if this window was closed while connecting). " +
		"To connect it again, run \"citadel logout\" in a terminal, then press Connect here.",
	enrollReplaceRefused: "Another Citadel program on this PC is using its AceTeam connection. Stop it, then try again.",
	enrollUnreachable:    "Citadel could not reach AceTeam. Check your internet connection, then press Connect to try again.",
	enrollCanceled:       "Canceled. This PC was not connected.",
	enrollDenied:         "The request was denied on the AceTeam page. This PC was not connected.",
	enrollExpired:        "The code expired before it was approved. Press Connect to get a new one.",
	enrollAuthFailed:     "AceTeam did not accept the request. Press Connect to try again.",
	enrollSaveFailed:     "Citadel could not save the connection on this PC. Check that your user folder is not full or read-only, then try again.",
	enrollNodeNameFailed: "Citadel could not choose a name for this PC. Press Connect to try again.",
	enrollConnectFailed: "Citadel could not join this PC to your AceTeam network. Check your internet connection and try again. " +
		"If the next try says this PC already has a registration, run \"citadel logout\" in a terminal first.",
	enrollLinkFailed: "AceTeam could not confirm this PC, so it may not appear in your account. " +
		"Run \"citadel logout\" in a terminal, then press Connect here to try again.",
	setupEnrollUntrustedLink: "AceTeam sent an approval link Citadel does not recognize, so it was not opened and nothing was changed. " +
		"Run \"citadel enroll\" in a terminal instead.",
	setupEnrollUntrustedNodeDir: "This page saves a device key only inside your own user folder, and this PC's Citadel settings point " +
		"somewhere else (for example, Citadel was set up for all users by an administrator), so nothing was changed. " +
		"Run \"citadel enroll\" in a terminal instead.",
}

const setupEnrollDefaultCopy = "The connection did not finish. Press Connect to try again."

// Stage copy.
const (
	setupEnrollStartingCopy   = "Contacting AceTeam..."
	setupEnrollWaitingCopy    = "On the AceTeam page that opened in your browser, enter the code shown here, then approve this PC."
	setupEnrollConnectingCopy = "Approved. Connecting this PC..."
	setupEnrollCancelingCopy  = "Canceling..."
	setupEnrollConnectedCopy  = "Connected. This PC stays registered after you close this window, but AceTeam agents can use it only " +
		"while Citadel's worker runs, and this page does not start the worker yet."
	setupEnrollUnverifiedCopy = "Connected, but AceTeam could not confirm it yet. If this PC does not appear in your account, " +
		"run \"citadel status\" in a terminal."
	setupEnrollLateCancelCopy = "Cancel came too late: the last step had already started, and this PC is now connected. " +
		"To undo it, run \"citadel logout\" in a terminal."
)

// setupEnrollCopyFor returns the fixed copy for a failure kind.
func setupEnrollCopyFor(kind enrollFailure) string {
	if c, ok := setupEnrollCopy[kind]; ok {
		return c
	}
	return setupEnrollDefaultCopy
}

// setupEnrollShutdownWait bounds how long the setup page waits, when it
// closes, for a running enrollment to roll back after being canceled. Windows
// ends a console process about 5s after its window is closed, so this is best
// effort: a rollback still running then is cut off, and the next try reports
// the leftover registration (the enrollAlreadyEnrolled copy says so).
const setupEnrollShutdownWait = 4 * time.Second

// setupEnroller is the setup page's single enrollment.
type setupEnroller struct {
	// base returns the options for one enrollment, resolved at click time.
	base func() enrollOptions
	// run is the enrollment itself (enrollDevice in production).
	run func(ctx context.Context, opts enrollOptions) (*enrollResult, error)
	// open opens the verification URL in the default browser.
	open func(string) error
	// already reports the node and org names of this PC's existing
	// registration, for the already-connected state.
	already func(configDir string) (nodeName, orgName string)
	// check refuses a node config dir this page must not save to
	// (checkSetupNodeDir in production). Nil checks nothing.
	check func(configDir string) error

	mu              sync.Mutex
	state           localviewer.SetupEnrollState
	running         bool
	cancel          context.CancelFunc
	done            chan struct{}
	warned          bool
	untrusted       bool
	cancelRequested bool
}

func newSetupEnroller(base func() enrollOptions, run func(context.Context, enrollOptions) (*enrollResult, error), open func(string) error, already func(string) (string, string)) *setupEnroller {
	return &setupEnroller{base: base, run: run, open: open, already: already, state: localviewer.SetupEnrollState{Stage: setupEnrollIdle}}
}

// State returns a copy of the current state.
func (e *setupEnroller) State() localviewer.SetupEnrollState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// Start begins one enrollment. A start while one runs is refused with the busy
// copy and leaves the running enrollment untouched. A node config dir the page
// must not save to fails the start before anything runs.
func (e *setupEnroller) Start() (localviewer.SetupEnrollState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return e.state, errors.New(setupEnrollCopyFor(enrollBusy))
	}
	opts := e.base()
	if e.check != nil {
		if err := e.check(opts.ConfigDir); err != nil {
			e.state = localviewer.SetupEnrollState{
				Stage: setupEnrollFailed, Failure: string(setupEnrollUntrustedNodeDir),
				Message: setupEnrollCopyFor(setupEnrollUntrustedNodeDir),
			}
			return e.state, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running, e.cancel, e.done = true, cancel, make(chan struct{})
	e.warned, e.untrusted, e.cancelRequested = false, false, false
	e.state = localviewer.SetupEnrollState{Stage: setupEnrollStarting, Message: setupEnrollStartingCopy}

	authURL := opts.AuthServiceURL
	opts.OnCode = func(c enrollCode) { e.onCode(c, authURL, cancel) }
	opts.OnStage = e.onStage
	// Warnings are never shown as text: they come from the link step and may
	// quote the server. Only the fact that one was raised is kept.
	opts.OnWarn = func(string) {
		e.mu.Lock()
		e.warned = true
		e.mu.Unlock()
	}
	go e.runOnce(ctx, opts, e.done)
	return e.state, nil
}

func (e *setupEnroller) onCode(c enrollCode, authURL string, cancel context.CancelFunc) {
	link, ok := setupApprovalLink(c, authURL)
	if !ok {
		e.mu.Lock()
		e.untrusted = true
		e.mu.Unlock()
		cancel()
		return
	}
	e.mu.Lock()
	if e.state.Stage == setupEnrollCanceling {
		// Canceled before the code arrived: do not open the approval page.
		e.mu.Unlock()
		return
	}
	e.state.Stage = setupEnrollWaiting
	e.state.UserCode = c.UserCode
	e.state.VerificationURL = link
	e.state.Message = setupEnrollWaitingCopy
	e.mu.Unlock()
	if e.open != nil {
		// Best effort, outside the lock: the page also shows the link. The
		// link carries no code, so the code is never on a command line.
		_ = e.open(link)
	}
}

func (e *setupEnroller) onStage(p enrollProgress) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Stage == setupEnrollCanceling {
		return
	}
	if p.NodeName != "" {
		e.state.NodeName = p.NodeName
	}
	switch p.Stage {
	case enrollStageSave, enrollStageConnect, enrollStageLink, enrollStageDone:
		e.state.Stage = setupEnrollConnecting
		e.state.UserCode, e.state.VerificationURL = "", ""
		e.state.Message = setupEnrollConnectingCopy
	}
}

func (e *setupEnroller) runOnce(ctx context.Context, opts enrollOptions, done chan struct{}) {
	defer close(done)
	res, err := e.run(ctx, opts)

	var already struct{ node, org string }
	kind, _ := enrollFailureKind(err)
	if err != nil && (kind == enrollAlreadyEnrolled || kind == enrollAlreadyConnected) && e.already != nil {
		already.node, already.org = e.already(opts.ConfigDir)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = false
	e.cancel()
	st := localviewer.SetupEnrollState{}
	switch {
	case err == nil && res != nil:
		st.Stage = setupEnrollConnected
		st.NodeName, st.OrgName = res.NodeName, res.OrgName
		switch {
		case e.cancelRequested:
			st.Message = setupEnrollLateCancelCopy
		case e.warned:
			st.Message = setupEnrollUnverifiedCopy
		default:
			st.Message = setupEnrollConnectedCopy
		}
	case e.untrusted:
		st.Stage, st.Failure = setupEnrollFailed, string(setupEnrollUntrustedLink)
		st.Message = setupEnrollCopyFor(setupEnrollUntrustedLink)
	case errors.Is(err, errSetupNodeDirUntrusted):
		st.Stage, st.Failure = setupEnrollFailed, string(setupEnrollUntrustedNodeDir)
		st.Message = setupEnrollCopyFor(setupEnrollUntrustedNodeDir)
	case err == nil:
		st.Stage, st.Message = setupEnrollFailed, setupEnrollDefaultCopy
	case kind == enrollCanceled:
		st.Stage, st.Failure, st.Message = setupEnrollCanceled, string(kind), setupEnrollCopyFor(kind)
	case kind == enrollAlreadyEnrolled || kind == enrollAlreadyConnected:
		st.Stage, st.Failure, st.Message = setupEnrollAlready, string(kind), setupEnrollCopyFor(kind)
		st.NodeName, st.OrgName = already.node, already.org
	default:
		st.Stage, st.Failure, st.Message = setupEnrollFailed, string(kind), setupEnrollCopyFor(kind)
	}
	e.state = st
}

// Cancel asks the running enrollment to stop. The state reads canceling until
// the enrollment has returned (and rolled back what it saved).
func (e *setupEnroller) Cancel() localviewer.SetupEnrollState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return e.state
	}
	e.cancel()
	e.cancelRequested = true
	e.state.Stage = setupEnrollCanceling
	e.state.UserCode, e.state.VerificationURL = "", ""
	e.state.Message = setupEnrollCancelingCopy
	return e.state
}

// Shutdown cancels a running enrollment and waits up to wait for it to return,
// so closing the setup page still runs the core's rollback when it can.
func (e *setupEnroller) Shutdown(wait time.Duration) {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.cancel()
	e.cancelRequested = true
	done := e.done
	e.mu.Unlock()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// hooks wires the enroller into the setup page.
func (e *setupEnroller) hooks(c *localviewer.SetupConfig) {
	c.EnrollStart = e.Start
	c.EnrollState = e.State
	c.EnrollCancel = e.Cancel
}

// setupApprovalLink returns the approval page to open and show: the bare
// verification URL, without the user code, so the code never appears on a
// process command line or in the browser history; the page shows the code.
// It is false when the user code is implausible, when the URL is not on
// authURL's own host over https (see setupVerificationURL), or when either
// URL carries a code that differs from the user code.
func setupApprovalLink(c enrollCode, authURL string) (string, bool) {
	if !plausibleUserCode(c.UserCode) {
		return "", false
	}
	raw := c.VerificationURI
	if raw == "" {
		raw = c.VerificationURIComplete
	}
	for _, s := range []string{c.VerificationURI, c.VerificationURIComplete} {
		if s == "" {
			continue
		}
		u, err := url.Parse(s)
		if err != nil {
			return "", false
		}
		if code := u.Query().Get("code"); code != "" && code != c.UserCode {
			return "", false
		}
	}
	return setupVerificationURL(raw, authURL)
}

// setupVerificationURL returns raw rebuilt from scheme, host and path only, or
// false when it does not point at authURL's own host over https (http only for
// a loopback test server). On Windows the URL is opened with url.dll, which
// would also run file: and other schemes.
func setupVerificationURL(raw, authURL string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" {
		return "", false
	}
	a, err := url.Parse(authURL)
	if err != nil || a.Host == "" {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, a.Scheme) || !strings.EqualFold(u.Host, a.Host) {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", false
		}
	default:
		return "", false
	}
	clean := url.URL{Scheme: strings.ToLower(u.Scheme), Host: u.Host, Path: u.Path}
	return clean.String(), true
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// plausibleUserCode accepts the short letters, digits and dashes a device-flow
// user code is made of.
func plausibleUserCode(c string) bool {
	if c == "" || len(c) > 32 {
		return false
	}
	for _, r := range c {
		if !(r == '-' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return true
}

// checkSetupNodeDir refuses a node config dir the setup page must not save a
// device key to: one whose machine-wide redirect (machine) is untrusted, or
// one not strictly inside one of homes. Paths are compared after resolving
// symlinks and junctions on their existing part, so a link cannot lead out.
func checkSetupNodeDir(dir string, homes []string, machine func() error) error {
	if machine != nil {
		if err := machine(); err != nil {
			return errSetupNodeDirUntrusted
		}
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return errSetupNodeDirUntrusted
	}
	target := resolveExistingPath(dir)
	for _, h := range homes {
		if h == "" || !filepath.IsAbs(h) {
			continue
		}
		if pathStrictlyWithin(resolveExistingPath(h), target) {
			return nil
		}
	}
	return errSetupNodeDirUntrusted
}

// resolveExistingPath resolves symlinks on the longest existing prefix of p and
// re-appends the rest.
func resolveExistingPath(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// pathStrictlyWithin reports whether p lies below root (not root itself).
// filepath.Rel compares case-insensitively on Windows.
func pathStrictlyWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// setupUserHomes is the folder that belongs to the user running the setup
// page: the home dir from platform.UserHomeDir (the repo's fixture-aware
// resolver). On Windows the default node config dir sits under
// %LOCALAPPDATA%, which is inside the profile, so the home covers it; a
// %LOCALAPPDATA% moved outside the profile is refused rather than trusted from
// an environment variable.
func setupUserHomes() []string {
	h, err := platform.UserHomeDir()
	if err != nil || h == "" {
		return nil
	}
	return []string{h}
}

// productionSetupNodeDirCheck is checkSetupNodeDir for this process's user
// and machine.
func productionSetupNodeDirCheck(dir string) error {
	return checkSetupNodeDir(dir, setupUserHomes(), network.CheckMachineConfigDirTrusted)
}

// setupNetworkCheckSuffix qualifies the doctor's "makes no calls" line on the
// setup page, which also has the Connect to AceTeam button.
const setupNetworkCheckSuffix = " The setup page connects to AceTeam only when you click Connect to AceTeam."

// setupPageChecks adapts the doctor results for the setup page: the Network
// check's "Citadel makes no calls of its own" holds for local mode, and the
// page says plainly that its Connect to AceTeam button is the exception.
// `local doctor` itself is unchanged.
func setupPageChecks(checks []localviewer.SetupCheck) []localviewer.SetupCheck {
	out := make([]localviewer.SetupCheck, len(checks))
	copy(out, checks)
	for i := range out {
		if out[i].What == "Network" && !strings.HasSuffix(out[i].Detail, setupNetworkCheckSuffix) {
			out[i].Detail = strings.TrimRight(out[i].Detail, ". ") + "." + setupNetworkCheckSuffix
		}
	}
	return out
}

// setupAlreadyInfo reads the node and org names of this PC's existing
// registration: the saved node name, and the org from configDir's config.yaml.
// Either may be empty.
func setupAlreadyInfo(configDir string) (nodeName, orgName string) {
	nodeName = getSavedHostname()
	data, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		return nodeName, ""
	}
	var cfg struct {
		OrgName string `yaml:"org_name"`
	}
	if yaml.Unmarshal(data, &cfg) != nil {
		return nodeName, ""
	}
	return nodeName, cfg.OrgName
}

// productionEnroll runs enrollDevice with the production side effects. The
// options must name this node's config dir and this process's auth service,
// which defaultEnrollDeps checks. The node dir check runs again right before
// the save and before the network connect, which resolves the state dir
// itself, so a redirect planted while the user was approving is refused.
func productionEnroll(ctx context.Context, opts enrollOptions) (*enrollResult, error) {
	deps, err := defaultEnrollDeps(opts)
	if err != nil {
		return nil, err
	}
	recheck := func() error {
		if filepath.Clean(nodeConfigDirFn()) != filepath.Clean(opts.ConfigDir) {
			return errSetupNodeDirUntrusted
		}
		return productionSetupNodeDirCheck(opts.ConfigDir)
	}
	guardEnrollDeps(&deps, recheck)
	return enrollDevice(ctx, opts, deps)
}

// guardEnrollDeps runs check before the save and the connect side effects.
func guardEnrollDeps(deps *enrollDeps, check func() error) {
	save, saveRedis, connect := deps.saveDeviceConfig, deps.saveRedisURL, deps.connect
	deps.saveDeviceConfig = func(dir string, t *nexus.TokenResponse) error {
		if err := check(); err != nil {
			return err
		}
		return save(dir, t)
	}
	deps.saveRedisURL = func(dir, redisURL string) error {
		if err := check(); err != nil {
			return err
		}
		return saveRedis(dir, redisURL)
	}
	deps.connect = func(r enrollConnectRequest) error {
		if err := check(); err != nil {
			return err
		}
		return connect(r)
	}
}

// productionEnrollOptions are resolved at click time, never at page start, so
// a test fixture that builds the page never resolves this machine's node dir.
func productionEnrollOptions() enrollOptions {
	return enrollOptions{AuthServiceURL: authServiceURL, ConfigDir: nodeConfigDirFn()}
}
