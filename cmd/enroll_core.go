// cmd/enroll_core.go
package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// The non-interactive enrollment core (citadel-cli#1867 C1).
//
// enrollDevice runs the whole device-grant enrollment without a terminal: it
// starts the device flow, hands the user code and verification URL to OnCode,
// polls with a cancellable context, saves the grant to an explicit node config
// dir, resolves the node name without prompting, reclaims a stale registration,
// connects and links. It never prints, prompts or exits; it reports through
// callbacks and returns a typed result or *enrollError.
//
// The device code, the registration key and the device API token never leave
// the core: they are not in the OnCode argument, the result, any callback
// string or any error text.
//
// Only one enrollment runs per process at a time; a concurrent call is refused
// with enrollBusy. The core never mutates the package-level control URL: it
// resolves the URL for this enrollment and passes it to connect.
//
// Cancellation is honored at every step until the link starts: a cancel after
// the save, reclaim or connect returns enrollCanceled, disconnects and removes
// this grant's saved credentials. Once the link has started it runs to
// completion on its own bounded budgets, because cancelling an attempt that may
// already have committed would make the outcome uncertain; its result (success
// or failure) is what enrollDevice returns, even if ctx was cancelled meanwhile.

// enrollStage names the step an enrollment is in.
type enrollStage string

const (
	enrollStageCheck     enrollStage = "check"
	enrollStageAuthorize enrollStage = "authorize"
	enrollStageSave      enrollStage = "save"
	enrollStageConnect   enrollStage = "connect"
	enrollStageLink      enrollStage = "link"
	enrollStageDone      enrollStage = "done"
)

// enrollFailure classifies why an enrollment stopped.
type enrollFailure string

const (
	enrollBusy             enrollFailure = "busy"
	enrollMisconfigured    enrollFailure = "misconfigured"
	enrollAlreadyConnected enrollFailure = "already_connected"
	enrollAlreadyEnrolled  enrollFailure = "already_enrolled"
	enrollReplaceRefused   enrollFailure = "replace_refused"
	enrollUnreachable      enrollFailure = "unreachable"
	enrollCanceled         enrollFailure = "canceled"
	enrollDenied           enrollFailure = "denied"
	enrollExpired          enrollFailure = "expired"
	enrollAuthFailed       enrollFailure = "auth_failed"
	enrollSaveFailed       enrollFailure = "save_failed"
	enrollNodeNameFailed   enrollFailure = "node_name_failed"
	enrollConnectFailed    enrollFailure = "connect_failed"
	enrollLinkFailed       enrollFailure = "link_failed"
)

var (
	// errEnrollAlreadyConnected is the cause of an enrollAlreadyConnected failure.
	errEnrollAlreadyConnected = errors.New("this node is already connected to the mesh")
	// errEnrollAlreadyEnrolled: this machine holds a mesh registration. A new
	// grant would save over its credentials while the network resumed the old
	// registration, and the refused link would leave the node with neither.
	errEnrollAlreadyEnrolled = errors.New("this machine is already enrolled; log out first, or request a new device registration to replace it")
	errEnrollBusy            = errors.New("another enrollment is already running")
)

// enrollFlight makes enrollDevice single-flight per process.
var enrollFlight sync.Mutex

// enrollError is every failure enrollDevice returns. Error() is the cause's
// text, so terminal wrappers print what they always printed.
type enrollError struct {
	Stage enrollStage
	Kind  enrollFailure
	Err   error
}

func (e *enrollError) Error() string { return e.Err.Error() }
func (e *enrollError) Unwrap() error { return e.Err }

// enrollFailureKind reports the failure class of an enrollDevice error.
func enrollFailureKind(err error) (enrollFailure, bool) {
	var ee *enrollError
	if errors.As(err, &ee) {
		return ee.Kind, true
	}
	return "", false
}

// enrollCode is what the user needs to approve this device. It never carries
// the device code.
type enrollCode struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
}

// enrollProgress is reported on every stage change. NodeName is set from the
// connect stage on.
type enrollProgress struct {
	Stage    enrollStage
	NodeName string
}

// enrollResult is the outcome of a completed enrollment: display fields only,
// never the grant, the device code or a credential.
type enrollResult struct {
	NodeName  string
	OrgID     string
	OrgName   string
	UserEmail string
	UserName  string
	// APIMode is true when the grant carried a device API token (the normal
	// case) rather than a legacy Redis URL.
	APIMode bool
}

// enrollOptions configures one enrollment.
type enrollOptions struct {
	// AuthServiceURL is the AceTeam API the grant is requested from, linked
	// against, reclaimed through and whose private CA trust is persisted.
	AuthServiceURL string
	// ConfigDir is the node config dir the grant is saved to and rolled back
	// from. Required: the core never resolves the machine's own. With the
	// production deps it must be nodeConfigDirFn(), because connect fixes the
	// node's session mode from the device config found there.
	ConfigDir string
	// NodeName is an explicit node name; empty resolves one without prompting.
	NodeName string
	// ForceNew requests a fresh device registration and skips reclaim. On a
	// machine that already holds a mesh registration it is required, and the
	// new key is forced over the saved state.
	ForceNew bool
	// NexusFlagChanged reports an explicit --nexus, which keeps precedence
	// over the grant's control URL.
	NexusFlagChanged bool

	// OnCode receives the user code and verification URL once the flow has
	// started. It must not block.
	OnCode func(enrollCode)
	// OnStage receives every stage change. It must not block.
	OnStage func(enrollProgress)
	// OnWarn receives non-fatal warnings (for example an unverified link on
	// an older server). Messages never contain the device code.
	OnWarn func(string)
}

// enrollConnectRequest is one network registration.
type enrollConnectRequest struct {
	NodeName   string
	AuthKey    string
	ControlURL string
	AuthURL    string
	// ForceLogin makes the network use AuthKey even when saved state exists.
	ForceLogin bool
}

// enrollDeps are the side effects of an enrollment, as seams.
type enrollDeps struct {
	isConnected func() bool
	// hasState reports a mesh registration saved on this machine.
	hasState func() bool
	// replacePrecheck refuses replacing a saved registration that another
	// process still holds.
	replacePrecheck   func() error
	checkReachable    func(authURL string) error
	authorize         func(ctx context.Context) (*DeviceAuthResult, error)
	saveDeviceConfig  func(configDir string, token *nexus.TokenResponse) error
	saveRedisURL      func(configDir, redisURL string) error
	nodeName          func(explicit string) (string, error)
	reclaim           func(apiToken, hostname string)
	resolveControlURL func(nexusFlagChanged bool, tokenNexusURL string) string
	connect           func(enrollConnectRequest) error
	link              func(result *DeviceAuthResult, rollback func(*nexus.TokenResponse) error, warn func(string)) error
	disconnect        func() error
}

// defaultEnrollDeps are the production side effects for a caller with no
// terminal. connect still fixes the session mode from the machine's own node
// config dir and other helpers still read the package-level auth URL, so the
// options must name exactly those; anything else is refused.
func defaultEnrollDeps(opts enrollOptions) (enrollDeps, error) {
	if opts.ConfigDir == "" || filepath.Clean(opts.ConfigDir) != filepath.Clean(nodeConfigDirFn()) {
		return enrollDeps{}, &enrollError{Stage: enrollStageCheck, Kind: enrollMisconfigured, Err: errors.New("enrollment config dir must be this node's config dir")}
	}
	if opts.AuthServiceURL != authServiceURL {
		return enrollDeps{}, &enrollError{Stage: enrollStageCheck, Kind: enrollMisconfigured, Err: errors.New("enrollment auth service must be this process's configured auth service")}
	}
	authURL := opts.AuthServiceURL
	return enrollDeps{
		isConnected:     network.IsGlobalConnected,
		hasState:        network.HasState,
		replacePrecheck: grantEnrollmentPrecheck,
		checkReachable:  nexus.CheckAPIReachable,
		authorize: func(ctx context.Context) (*DeviceAuthResult, error) {
			return authorizeDeviceNonInteractive(ctx, nexus.NewDeviceAuthClient(authURL), opts.ForceNew, opts.OnCode)
		},
		saveDeviceConfig: saveDeviceConfigToDir,
		saveRedisURL:     saveRedisURLToDir,
		nodeName:         resolveNodeNameNonInteractive,
		reclaim: func(apiToken, hostname string) {
			reclaimStaleNodeByHostnameWith(authURL, apiToken, hostname, nil)
		},
		resolveControlURL: deviceAuthControlURL,
		connect: func(r enrollConnectRequest) error {
			_, err := registerOnNetworkWith(r.NodeName, r.AuthKey, r.ForceLogin, r.ControlURL, r.AuthURL)
			return err
		},
		link: func(result *DeviceAuthResult, rollback func(*nexus.TokenResponse) error, warn func(string)) error {
			return linkDeviceEnrollmentGatedWith(result, enrollmentNodeID, nexus.NewDeviceAuthClient(authURL), rollback, warn)
		},
		disconnect: network.Disconnect,
	}, nil
}

// defaultDeviceFlowInterval is RFC 8628's default when the server sends none.
const defaultDeviceFlowInterval = 5

// deviceFlowClient is the slice of the device-auth client the core uses.
type deviceFlowClient interface {
	StartFlowContext(ctx context.Context, opts *nexus.StartFlowOptions) (*nexus.DeviceCodeResponse, error)
	PollForTokenContext(ctx context.Context, deviceCode string, interval int) (*nexus.TokenResponse, error)
}

// authorizeDeviceNonInteractive starts the device flow, reports the code
// through onCode and polls until approval, refusal, expiry or cancellation.
func authorizeDeviceNonInteractive(ctx context.Context, client deviceFlowClient, forceNew bool, onCode func(enrollCode)) (*DeviceAuthResult, error) {
	resp, err := client.StartFlowContext(ctx, &nexus.StartFlowOptions{ForceNew: forceNew})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("failed to start device authorization: %w", err)
	}
	if onCode != nil {
		onCode(enrollCode{
			UserCode:                resp.UserCode,
			VerificationURI:         resp.VerificationURI,
			VerificationURIComplete: resp.VerificationURIComplete,
			ExpiresIn:               resp.ExpiresIn,
		})
	}
	interval := resp.Interval
	if interval <= 0 {
		interval = defaultDeviceFlowInterval
	}
	token, err := client.PollForTokenContext(ctx, resp.DeviceCode, interval)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("device authorization failed: %w", err)
	}
	return &DeviceAuthResult{Token: token, DeviceCode: resp.DeviceCode}, nil
}

// enrollAuthkeyPattern matches registration keys in text the core did not
// write (network or control-server errors), whatever key they name.
var enrollAuthkeyPattern = regexp.MustCompile(`tskey-[A-Za-z0-9_-]+`)

// redactEnrollError returns err unchanged when its text holds none of the
// secrets and no registration key; otherwise a plain error with them replaced.
// The unredacted chain is dropped, so no caller can reach the secret through
// errors.As or %+v.
func redactEnrollError(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	redacted := enrollAuthkeyPattern.ReplaceAllString(msg, "[redacted]")
	for _, s := range secrets {
		if s != "" {
			redacted = strings.ReplaceAll(redacted, s, "[redacted]")
		}
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}

// enrollDevice runs one non-interactive enrollment. See the file comment.
//
// Failure handling, by stage:
//   - before approval (check, authorize): nothing was saved;
//   - after the grant is saved and before the link is sent (save, node name,
//     cancellation, connect): the host is disconnected and this enrollment's
//     saved credentials are removed, with any values the save overwrote put
//     back, so the next attempt authenticates again instead of treating a node
//     with an unlinked key as configured;
//   - link: linkDeviceEnrollmentGatedWith decides, with the existing rules
//     (a definite refusal from an enforcing server removes the credentials and
//     restores the prior ones, an uncertain outcome keeps them), and the host
//     is disconnected.
func enrollDevice(ctx context.Context, opts enrollOptions, deps enrollDeps) (*enrollResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !enrollFlight.TryLock() {
		return nil, &enrollError{Stage: enrollStageCheck, Kind: enrollBusy, Err: errEnrollBusy}
	}
	defer enrollFlight.Unlock()

	if opts.ConfigDir == "" {
		return nil, &enrollError{Stage: enrollStageCheck, Kind: enrollMisconfigured, Err: errors.New("enrollment needs an explicit node config dir")}
	}
	nodeName := ""
	var secrets []string
	stage := func(s enrollStage) {
		if opts.OnStage != nil {
			opts.OnStage(enrollProgress{Stage: s, NodeName: nodeName})
		}
	}
	fail := func(s enrollStage, kind enrollFailure, err error) error {
		return &enrollError{Stage: s, Kind: kind, Err: redactEnrollError(err, secrets)}
	}

	stage(enrollStageCheck)
	if deps.isConnected() {
		return nil, fail(enrollStageCheck, enrollAlreadyConnected, errEnrollAlreadyConnected)
	}
	// Replacing a saved registration needs an explicit request, a forced
	// login (tsnet would otherwise resume the old registration) and no other
	// process holding the mesh state.
	replacing := deps.hasState()
	if replacing {
		if !opts.ForceNew {
			return nil, fail(enrollStageCheck, enrollAlreadyEnrolled, errEnrollAlreadyEnrolled)
		}
		if err := deps.replacePrecheck(); err != nil {
			return nil, fail(enrollStageCheck, enrollReplaceRefused, err)
		}
	}
	if err := deps.checkReachable(opts.AuthServiceURL); err != nil {
		return nil, fail(enrollStageCheck, enrollUnreachable, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fail(enrollStageCheck, enrollCanceled, err)
	}

	stage(enrollStageAuthorize)
	auth, err := deps.authorize(ctx)
	if auth != nil {
		secrets = append(secrets, auth.DeviceCode)
		if auth.Token != nil {
			secrets = append(secrets, auth.Token.Authkey, auth.Token.DeviceAPIToken, auth.Token.RedisURL)
		}
	}
	if err != nil {
		return nil, fail(enrollStageAuthorize, classifyAuthorizeFailure(ctx, err), err)
	}
	if auth == nil || auth.Token == nil || auth.Token.Authkey == "" {
		return nil, fail(enrollStageAuthorize, enrollAuthFailed, errors.New("device authorization returned no registration key"))
	}
	// Approved, but nothing is saved yet: a cancel here leaves no trace.
	if err := ctx.Err(); err != nil {
		return nil, fail(enrollStageAuthorize, enrollCanceled, err)
	}
	token := auth.Token

	configFile := filepath.Join(opts.ConfigDir, "config.yaml")
	prior, err := snapshotEnrollmentFields(configFile)
	if err != nil {
		return nil, fail(enrollStageSave, enrollSaveFailed, fmt.Errorf("could not read device config: %w", err))
	}
	rollback := func(t *nexus.TokenResponse) error {
		return rollbackEnrollmentCredentialsRestoring(configFile, t, prior)
	}
	failUnlinked := func(s enrollStage, kind enrollFailure, err error, connected bool) error {
		if connected {
			_ = deps.disconnect()
		}
		if rbErr := rollback(token); rbErr != nil {
			err = fmt.Errorf("%w (removing this enrollment's saved credentials also failed)", err)
		}
		return fail(s, kind, err)
	}

	stage(enrollStageSave)
	switch {
	case token.DeviceAPIToken != "":
		err = deps.saveDeviceConfig(opts.ConfigDir, token)
	case token.RedisURL != "":
		err = deps.saveRedisURL(opts.ConfigDir, token.RedisURL)
	}
	if err != nil {
		return nil, failUnlinked(enrollStageSave, enrollSaveFailed, fmt.Errorf("could not save device config: %w", err), false)
	}

	nodeName, err = deps.nodeName(opts.NodeName)
	if err != nil {
		nodeName = ""
		return nil, failUnlinked(enrollStageSave, enrollNodeNameFailed, err, false)
	}

	// Reclaim removes a predecessor's registration on the server, so a cancel
	// must be seen before it.
	if err := ctx.Err(); err != nil {
		return nil, failUnlinked(enrollStageSave, enrollCanceled, err, false)
	}
	// Reclaim with the grant just approved, never a saved credential.
	reclaimEnrollmentNodesWith(opts.ForceNew, auth, nil, deps.reclaim, nodeName)
	controlURL := deps.resolveControlURL(opts.NexusFlagChanged, token.NexusURL)
	if err := ctx.Err(); err != nil {
		return nil, failUnlinked(enrollStageSave, enrollCanceled, err, false)
	}

	stage(enrollStageConnect)
	if err := deps.connect(enrollConnectRequest{
		NodeName:   nodeName,
		AuthKey:    token.Authkey,
		ControlURL: controlURL,
		AuthURL:    opts.AuthServiceURL,
		ForceLogin: replacing,
	}); err != nil {
		// A failure after the network accepted the key can leave it connected.
		return nil, failUnlinked(enrollStageConnect, enrollConnectFailed, err, true)
	}
	// Last cancellation point: the link has not been sent.
	if err := ctx.Err(); err != nil {
		return nil, failUnlinked(enrollStageConnect, enrollCanceled, err, true)
	}

	// From here the link runs to completion; see the file comment.
	stage(enrollStageLink)
	if err := deps.link(auth, rollback, opts.OnWarn); err != nil {
		_ = deps.disconnect()
		return nil, fail(enrollStageLink, enrollLinkFailed, err)
	}

	stage(enrollStageDone)
	return &enrollResult{
		NodeName:  nodeName,
		OrgID:     token.OrgID,
		OrgName:   token.OrgName,
		UserEmail: token.UserEmail,
		UserName:  token.UserName,
		APIMode:   token.DeviceAPIToken != "",
	}, nil
}

func classifyAuthorizeFailure(ctx context.Context, err error) enrollFailure {
	switch {
	case ctx.Err() != nil, errors.Is(err, context.Canceled):
		return enrollCanceled
	case errors.Is(err, nexus.ErrDeviceAuthDenied):
		return enrollDenied
	case errors.Is(err, nexus.ErrDeviceAuthExpired), errors.Is(err, nexus.ErrDeviceAuthTimeout):
		return enrollExpired
	case nexus.IsNetworkError(err):
		return enrollUnreachable
	default:
		return enrollAuthFailed
	}
}
