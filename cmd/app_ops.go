// cmd/app_ops.go
//
// cmd-side live adapter for the APP_* worker handlers (CRAM slice A1,
// aceteam#9672). It wires the three edges the worker package must not import:
// the container runtime seam (jobs.AppPodRunner over
// catalog.SelectContainerRuntime), the in-process gateway (the SAME
// liveExposeOps funnel EXPOSE_SET uses, so an app route gets the durable
// exposure record, epoch high-water, and restart restore for free), and the
// node identity key (the AEP app_deploy receipt). It lives here (not in
// internal/worker) for the identical reason liveExposeOps does - it needs
// cmd-level edges the worker handler stays unit-testable without.
package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/services"
)

// appHealthWaitTimeout bounds the detached readiness probe. It is advisory: the
// deploy result returns "starting" immediately and the heartbeat carries health
// from then on, so this only decides how long the fire-and-forget log-only
// probe waits before reporting "not healthy yet".
const appHealthWaitTimeout = 60 * time.Second

var (
	appRunnerOnce sync.Once
	appRunnerRef  *jobs.AppPodRunner
)

// appPodRunner lazily builds the process-wide app pod runner over the selected
// container runtime. SelectContainerRuntime logs its choice once per process;
// the runner is otherwise stateless, so one instance is shared.
func appPodRunner() *jobs.AppPodRunner {
	appRunnerOnce.Do(func() {
		homeDir, _ := os.UserHomeDir()
		appRunnerRef = jobs.NewAppPodRunner(catalog.SelectContainerRuntime(), homeDir, Log)
	})
	return appRunnerRef
}

// liveAppOps implements worker.AppOps against the live container runtime and
// gateway.
type liveAppOps struct{}

// Deploy pulls the image, creates the app pod, wires its gateway route under
// "app-<short_code>", and (when signing is enabled) attaches a signed AEP
// app_deploy receipt. The gateway ref is checked FIRST so a not-yet-ready
// gateway returns a transient error and the job retries, rather than creating a
// pod with no route (DoR §3.3). A redeploy reuses the port already published by
// the existing pod so the gateway upstream stays stable.
func (liveAppOps) Deploy(ctx context.Context, req worker.AppDeployRequest) (*worker.AppDeployResult, error) {
	// Gateway must be running BEFORE we create a pod, or we would strand a
	// routeless pod. Retry (transient) until `citadel work --gateway` / the
	// provisioned gateway is serving.
	if getProvisionedServiceGateway() == nil {
		return nil, fmt.Errorf("%w: no in-process gateway (hosted apps require the node gateway to be running)", worker.ErrAppTransient)
	}

	runner := appPodRunner()

	// Reuse the existing pod's host port on redeploy; otherwise allocate the
	// lowest free port in the hosted-app range. Both the reuse read and the
	// allocation are safe without a lock because APP_DEPLOY runs on the
	// serialized (exec-1) worker lane.
	hostPort, reused := runner.ExistingHostPort(ctx, req.ShortCode)
	if !reused {
		used := runner.UsedAppPorts(ctx)
		alloc, err := services.AllocateAppPodPort(func(p int) bool {
			return used[p] || localPortListening(p)
		})
		if err != nil {
			return nil, err
		}
		hostPort = alloc
	}

	spec, err := jobs.ParseAppSpec(jobs.AppSpecInput{
		ShortCode:       req.ShortCode,
		Image:           req.Image,
		ContainerPort:   req.ContainerPort,
		Env:             req.Env,
		HealthPath:      req.HealthPath,
		Size:            req.Size,
		Runtime:         req.Runtime,
		Org:             req.Org,
		DeploymentID:    req.DeploymentID,
		StateVolumePath: req.StateVolumePath,
		StateMountPath:  req.StateMountPath,
	}, hostPort, runner.HomeDir())
	if err != nil {
		return nil, err
	}

	dep, err := runner.Deploy(ctx, spec)
	if err != nil {
		return nil, err
	}

	// Wire the gateway route through the shared expose funnel. Rotate:false so a
	// redeploy keeps the epoch and any outstanding link token (DoR §3.6).
	exposeRes, err := (liveExposeOps{}).Expose(ctx, worker.ExposeRequest{
		Name:       gateway.AppExposePrefix + req.ShortCode,
		Port:       hostPort,
		Visibility: req.Visibility,
		Creator:    req.Creator,
		TTLSeconds: req.TTLSeconds,
		Rotate:     false,
	})
	if err != nil {
		return nil, fmt.Errorf("wire gateway route for app %q: %w", req.ShortCode, err)
	}

	// Detached, log-only readiness probe. The job returns "starting" now.
	go func() {
		probeCtx, cancel := context.WithTimeout(context.Background(), appHealthWaitTimeout+10*time.Second)
		defer cancel()
		if herr := runner.WaitHealthy(probeCtx, hostPort, spec.HealthPath, appHealthWaitTimeout); herr != nil {
			Log("app %q not healthy yet: %v", req.ShortCode, herr)
		} else {
			Log("app %q is healthy on 127.0.0.1:%d%s", req.ShortCode, hostPort, spec.HealthPath)
		}
	}()

	res := &worker.AppDeployResult{
		Name:        gateway.AppExposePrefix + req.ShortCode,
		ShortCode:   req.ShortCode,
		URL:         exposeRes.URL,
		HostPort:    hostPort,
		PodID:       dep.PodID,
		Image:       req.Image,
		ImageDigest: dep.ImageDigest,
		State:       "starting",
		Visibility:  req.Visibility,
		Epoch:       exposeRes.Epoch,
		Token:       exposeRes.Token,
		ExpiresAt:   exposeRes.ExpiresAt,
		Runtime:     catalog.SelectContainerRuntime().Label(),
		Receipt:     signAppDeployReceipt(req, dep),
	}
	return res, nil
}

func (liveAppOps) Stop(ctx context.Context, shortCode string) (*worker.AppLifecycleResult, error) {
	state, err := appPodRunner().Stop(ctx, shortCode)
	if err != nil {
		return nil, err
	}
	return &worker.AppLifecycleResult{Name: gateway.AppExposePrefix + shortCode, ShortCode: shortCode, State: state}, nil
}

func (liveAppOps) Start(ctx context.Context, shortCode string) (*worker.AppLifecycleResult, error) {
	state, err := appPodRunner().Start(ctx, shortCode)
	if err != nil {
		return nil, err
	}
	return &worker.AppLifecycleResult{Name: gateway.AppExposePrefix + shortCode, ShortCode: shortCode, State: state}, nil
}

func (liveAppOps) Status(ctx context.Context, shortCode string) (*worker.AppStatusResult, error) {
	info, err := appPodRunner().Status(ctx, shortCode)
	if err != nil {
		return nil, err
	}
	return &worker.AppStatusResult{
		Name:      gateway.AppExposePrefix + shortCode,
		ShortCode: shortCode,
		State:     info.State,
		HostPort:  info.HostPort,
		PodID:     info.PodID,
		Image:     info.Image,
	}, nil
}

func (liveAppOps) Logs(ctx context.Context, shortCode string, tail int) (*worker.AppLogsResult, error) {
	logs, err := appPodRunner().Logs(ctx, shortCode, tail)
	if err != nil {
		return nil, err
	}
	return &worker.AppLogsResult{ShortCode: shortCode, Logs: logs}, nil
}

// Destroy tears the gateway route down FIRST (route dark before teardown), then
// removes the pod and its volumes. Unexpose is idempotent, so a route that was
// never wired is a harmless no-op.
func (liveAppOps) Destroy(ctx context.Context, shortCode string) (*worker.AppLifecycleResult, error) {
	if getProvisionedServiceGateway() != nil {
		if _, err := (liveExposeOps{}).Unexpose(ctx, gateway.AppExposePrefix+shortCode); err != nil {
			// A failed route teardown is not fatal to the destroy: the pod is what
			// holds resources, and the durable exposure record is cleaned up on the
			// next Unexpose/restart. Log and proceed.
			Log("app %q: gateway route teardown failed (continuing with pod removal): %v", shortCode, err)
		}
	}
	if err := appPodRunner().Destroy(ctx, shortCode); err != nil {
		return nil, err
	}
	return &worker.AppLifecycleResult{Name: gateway.AppExposePrefix + shortCode, ShortCode: shortCode, State: "destroyed"}, nil
}

// signAppDeployReceipt builds the signed AEP v2 app_deploy receipt when
// CITADEL_SIGN_AEP_RECEIPTS is truthy (the SAME flag the inference path uses; a
// deploy gates on this flag ALONE, with no grounding prerequisite). Fail-open:
// any error yields no receipt (the deploy still succeeds) so a node with no key
// is never blocked from deploying, mirroring applyTrustEngine's contract.
func signAppDeployReceipt(req worker.AppDeployRequest, dep *jobs.AppPodDeployResult) map[string]any {
	if !update.IsTruthy(os.Getenv("CITADEL_SIGN_AEP_RECEIPTS")) {
		return nil
	}
	signer := nodeidentity.Convergent(network.GetNodeConfigDir())
	fabricNodeID := config.LoadDeviceCredsConverged().FabricNodeID
	nodeID, err := aep.ResolveNodeID(signer, fabricNodeID)
	if err != nil {
		Log("app %q: AEP receipt not signed (node id unresolved): %v", req.ShortCode, err)
		return nil
	}
	inputSHA := appReceiptInputSHA(req)
	// output_sha256 preimage is EXACTLY "<image_digest>\n<pod_id>" (a4 verifiers
	// reproduce this byte-for-byte).
	outputSHA := "sha256:" + sha256Hex(dep.ImageDigest+"\n"+dep.PodID)
	receipt, err := aep.BuildSignedAppDeployReceipt(signer, nodeID, req.JobID, inputSHA, outputSHA, time.Now())
	if err != nil {
		Log("app %q: AEP receipt not signed: %v", req.ShortCode, err)
		return nil
	}
	m, err := receipt.ToMap()
	if err != nil {
		Log("app %q: AEP receipt not attached: %v", req.ShortCode, err)
		return nil
	}
	return m
}

// appReceiptInputSHA resolves the receipt's input_sha256. The coordinator's
// canonical-manifest digest (req.ManifestSHA256) is authoritative when present,
// so both sides agree byte-for-byte; absent it, the node hashes a minimal
// canonical stand-in over the fields it launched with (documented as the
// fallback the coordinator does not reproduce, so content_bound is only
// meaningful when manifest_sha256 is supplied).
func appReceiptInputSHA(req worker.AppDeployRequest) string {
	if m := strings.TrimSpace(req.ManifestSHA256); m != "" {
		if strings.HasPrefix(m, "sha256:") {
			return m
		}
		return "sha256:" + m
	}
	return "sha256:" + sha256Hex(req.Image+"\n"+strconv.Itoa(req.ContainerPort))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// refuseReservedAppExposeName rejects an operator expose/unexpose of a reserved
// "app-<short_code>" route (CRAM A1): only the app runner may mint one. It is
// the named guard the /agent/expose control-path closures call
// (cmd/agent_tools.go) so a refactor of those closures cannot silently drop the
// check; the EXPOSE_SET / UNEXPOSE job parsers enforce the same rule
// independently (worker.parseExposeRequest / parseUnexposeRequest). remedy is
// the action-specific hint appended to the error. Returns nil for any
// non-reserved name.
func refuseReservedAppExposeName(name, remedy string) error {
	if gateway.IsAppExposeName(strings.TrimSpace(name)) {
		return fmt.Errorf("exposure name %q uses the reserved %q prefix (hosted-app pod routes); %s", name, gateway.AppExposePrefix, remedy)
	}
	return nil
}

// appPodListings returns this node's hosted-app pods as heartbeat AppInfo
// entries (status.CollectorConfig.AppPods). The entry Name is the app SHORT
// CODE (the stable handle the coordinator's app reconcile keys on), Status is
// running|stopped, and Port is the published host port. It shells a bounded
// `ps` scan once per heartbeat; on a node with no hosted apps it returns nil
// after a single empty scan.
func appPodListings() []status.AppInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := appPodRunner().List(ctx)
	if err != nil || len(infos) == 0 {
		return nil
	}
	out := make([]status.AppInfo, 0, len(infos))
	for _, in := range infos {
		out = append(out, status.AppInfo{
			Name:   in.ShortCode,
			Status: in.State,
			Port:   in.HostPort,
		})
	}
	return out
}
