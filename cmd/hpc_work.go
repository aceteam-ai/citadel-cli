package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/hpc"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The HPC worker (aceteam#12715) is a separate entry point, not a set of gates
// inside runWork. It is built from a positive list of parts: the API job
// source on this node's own stream, the API heartbeat, the usage syncer, and
// the handler set below. It never joins the mesh, opens a listening socket,
// starts managed services, reconciles desired state, or updates itself.
// hpc_work_tripwire_test.go fails if one of those entry points is referenced
// from the HPC files.

// hpcNodeIDPattern is the platform's API-only node id shape ("hpc-" + 32 hex).
var hpcNodeIDPattern = regexp.MustCompile(`^hpc-[0-9a-f]{32}$`)

// hpcCredentials is what the HPC worker needs from the job-local enrollment.
type hpcCredentials struct {
	NodeID     string
	OrgID      string
	Token      string
	APIBaseURL string
}

// hpcCredentialsFrom validates the job-local device configuration written by
// HPC enrollment. The platform node id (FabricNodeID) is the identity every
// queue, heartbeat and usage record is keyed on.
func hpcCredentialsFrom(dc *DeviceConfig) (hpcCredentials, error) {
	if dc == nil || strings.TrimSpace(dc.DeviceAPIToken) == "" {
		return hpcCredentials{}, errors.New("this job is not enrolled; run 'citadel hpc enroll' first")
	}
	c := hpcCredentials{
		NodeID:     strings.TrimSpace(dc.FabricNodeID),
		OrgID:      strings.TrimSpace(dc.OrgID),
		Token:      strings.TrimSpace(dc.DeviceAPIToken),
		APIBaseURL: strings.TrimSpace(dc.APIBaseURL),
	}
	if !hpcNodeIDPattern.MatchString(c.NodeID) {
		return hpcCredentials{}, errors.New("job-local enrollment has no valid platform node id")
	}
	if c.OrgID == "" {
		return hpcCredentials{}, errors.New("job-local enrollment has no organization id")
	}
	if c.APIBaseURL == "" {
		return hpcCredentials{}, errors.New("job-local enrollment has no API base URL")
	}
	return c, nil
}

// hpcQueueNames is the complete consume set: this node's own per-node stream.
// A bound HPC credential is refused on every shared queue, so no base, GPU,
// tag or org shell queue may be added (statically or later).
func hpcQueueNames(orgID, nodeID string) []string {
	return []string{nodeQueueName(orgID, nodeID)}
}

// hpcWorkFlagsAllowed lists the `citadel work` local flags the HPC profile
// accepts. Any other changed flag (gateway, terminal, status port, direct
// Redis, queues, egress relay, workspace, ...) names a surface the profile
// turns off, so it is refused instead of silently ignored.
var hpcWorkFlagsAllowed = map[string]bool{
	"poll-ms":         true,
	"max-retries":     true,
	"max-concurrency": true,
}

// hpcRefusedWorkFlags returns the changed local flags the profile refuses.
func hpcRefusedWorkFlags(fs *pflag.FlagSet) []string {
	var refused []string
	fs.Visit(func(f *pflag.Flag) {
		if !hpcWorkFlagsAllowed[f.Name] {
			refused = append(refused, "--"+f.Name)
		}
	})
	sort.Strings(refused)
	return refused
}

// hpcCapabilityPolicy is the HPC profile's handler policy. Its zero value is
// the v1 default: inference and approved templates only. The opt-ins are
// reserved for a future org-admin grant of shell or job-workspace files; they
// are unexported, and no flag, environment variable or config file sets them.
type hpcCapabilityPolicy struct {
	allowShell bool
	allowFiles bool
}

// validate fails closed on any opt-in until its confinement is implemented.
func (p hpcCapabilityPolicy) validate() error {
	if p.allowShell || p.allowFiles {
		return errors.New("shell and files are not available in this version of the hpc profile")
	}
	return nil
}

// hpcTemplateBuiltinPolicy allows only builtins that open no listener. The
// papercraft renderers drive a browser over a loopback debug (CDP) port; on a
// shared compute node any local user can reach a loopback port, and CDP gives
// full browser control.
func hpcTemplateBuiltinPolicy(handler string) string {
	switch handler {
	case audioMixBuiltinName:
		return ""
	case papercraftBuiltinName, papercraftSceneBuiltinName, papercraftSceneV2BuiltinName:
		return "it needs a browser debug listener, which the hpc profile does not allow on a shared compute node"
	default:
		return "it is not on the hpc profile's builtin allowlist"
	}
}

// hpcPermissions is the fixed permission state the HPC worker reports and
// enforces. It never reads permissions.yaml: a live permission change cannot
// widen what this process does.
func hpcPermissions() *config.Permissions {
	return &config.Permissions{}
}

// hpcHandlerOpts carries what the HPC handler set needs.
type hpcHandlerOpts struct {
	Policy       hpcCapabilityPolicy
	WorkspaceDir string
	NodeID       string
	Signer       aep.Signer
	Log          func(format string, args ...any)
}

// buildHPCJobHandlers returns the initial handler slice: llm_inference only,
// without a model swapper (the swapper starts and stops containers).
func buildHPCJobHandlers(o hpcHandlerOpts) ([]worker.JobHandler, error) {
	if err := o.Policy.validate(); err != nil {
		return nil, err
	}
	llm := worker.NewLLMInferenceHandler()
	if o.Signer != nil {
		llm = llm.WithSigner(o.Signer)
	}
	if o.NodeID != "" {
		nodeID := o.NodeID
		llm = llm.WithFabricNodeIDResolver(func() string { return nodeID })
	}
	return []worker.JobHandler{llm}, nil
}

// registerHPCRunnerHandlers adds RUN_JOB_TEMPLATE, which needs the runner's
// node id, confined to the job workspace and the builtin allowlist.
func registerHPCRunnerHandlers(runner *worker.Runner, o hpcHandlerOpts) {
	runner.RegisterHandler(worker.NewRunJobTemplateHandler(worker.RunJobTemplateHandlerConfig{
		Ops:    hpcTemplateRunOps(o.WorkspaceDir, runner.NodeID()),
		NodeID: runner.NodeID(),
		Log:    o.Log,
	}))
}

// hpcTemplateRunOps is the template adapter the HPC worker registers: the job
// workspace is the only staging root and the builtin allowlist applies.
func hpcTemplateRunOps(workspaceDir, nodeID string) liveTemplateRunOps {
	return liveTemplateRunOps{
		workspaceDir:  workspaceDir,
		nodeID:        nodeID,
		builtinPolicy: hpcTemplateBuiltinPolicy,
	}
}

// hpcWorkerDeps are the edges of the HPC worker core. Production fills them
// from the job root; tests inject a fake source and capture the runner.
type hpcWorkerDeps struct {
	Creds          hpcCredentials
	Layout         hpc.Layout
	Source         worker.JobSource
	StreamFactory  func(job *worker.Job) worker.StreamWriter
	Policy         hpcCapabilityPolicy
	MaxConcurrency int
	Signer         aep.Signer
	UsageStore     *usage.Store
	State          *worker.WorkerState
	Log            func(format string, args ...any)
	// OnRunner observes the fully registered runner before Run starts (the
	// heartbeat start and drain wiring hang off it).
	OnRunner func(ctx context.Context, runner *worker.Runner)
}

// newHPCRunner builds the runner and its complete handler set.
func newHPCRunner(d hpcWorkerDeps) (*worker.Runner, error) {
	logf := d.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	opts := hpcHandlerOpts{
		Policy:       d.Policy,
		WorkspaceDir: d.Layout.WorkspaceDir,
		NodeID:       d.Creds.NodeID,
		Signer:       d.Signer,
		Log:          logf,
	}
	handlers, err := buildHPCJobHandlers(opts)
	if err != nil {
		return nil, err
	}
	var jobRecordFn func(record usage.UsageRecord)
	if d.UsageStore != nil {
		store, nodeID := d.UsageStore, d.Creds.NodeID
		jobRecordFn = func(record usage.UsageRecord) {
			record.NodeID = nodeID
			if err := store.Insert(record); err != nil {
				logf("usage insert error: %v", err)
			}
		}
	}
	concurrency := d.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	cfg := worker.RunnerConfig{
		WorkerID:       fmt.Sprintf("citadel-%s", uuid.New().String()[:8]),
		NodeID:         d.Creds.NodeID,
		AgentVersion:   Version,
		Verbose:        true,
		ActivityFn:     func(_ string, msg string) { logf("%s", msg) },
		JobRecordFn:    jobRecordFn,
		MaxConcurrency: concurrency,
		State:          d.State,
	}
	if d.Signer != nil {
		cfg.WorkReceiptSigner = d.Signer
		cfg.WorkReceiptNodeID = d.Creds.NodeID
	}
	runner := worker.NewRunner(d.Source, handlers, cfg)
	if d.StreamFactory != nil {
		runner.WithStreamWriterFactory(d.StreamFactory)
	}
	registerHPCRunnerHandlers(runner, opts)
	return runner, nil
}

// runHPCWorker builds the runner and runs it until ctx ends.
func runHPCWorker(ctx context.Context, d hpcWorkerDeps) error {
	runner, err := newHPCRunner(d)
	if err != nil {
		return err
	}
	if d.OnRunner != nil {
		d.OnRunner(ctx, runner)
	}
	return runner.Run(ctx)
}

// hpcHeartbeatCollector builds the status collector for the HPC heartbeat:
// no config dir (so no installed-engine enumeration), no hotswap, and the
// fixed HPC permission state.
func hpcHeartbeatCollector(nodeID string, runner *atomic.Pointer[worker.Runner], state *worker.WorkerState, transport func() string) *status.Collector {
	providers := newWorkSessionProviders(state, nil, runner, transport)
	return status.NewCollector(status.CollectorConfig{
		NodeName:       nodeID,
		WorkerLiveness: providers.Worker,
		LaneActivity:   providers.Lanes,
		JobTypes: func() []string {
			if rn := runner.Load(); rn != nil {
				return rn.SupportedJobTypes()
			}
			return nil
		},
		PermissionsProvider: hpcPermissions,
	})
}

// runHPCWork is `citadel work` under the HPC profile.
func runHPCWork(cmd *cobra.Command, _ []string) {
	if refused := hpcRefusedWorkFlags(cmd.Flags()); len(refused) > 0 {
		fmt.Fprintf(os.Stderr, "Error: %s not available in the hpc profile (it runs API-only, with no listeners, services, mesh or updates).\n", strings.Join(refused, ", "))
		os.Exit(2)
	}
	layout := currentHPCLayout()
	if err := runHPCWorkProcess(cmd.Context(), layout); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// runHPCWorkProcess wires the production edges and runs the worker.
func runHPCWorkProcess(parent context.Context, layout hpc.Layout) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// One worker per job root. The lock lives under the job's own network
	// state dir, so concurrent allocations never contend with each other.
	lock, err := worklock.Acquire(network.GetStateDir(), Version, Log)
	if err != nil {
		return fmt.Errorf("another citadel worker already serves this job: %w", err)
	}
	defer lock.Release()

	creds, err := hpcCredentialsFrom(getDeviceConfigFromFile())
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			fmt.Println("\n   - Received shutdown signal...")
			cancel()
		case <-ctx.Done():
		}
	}()

	state := worker.NewWorkerState()
	source := worker.NewAPISource(worker.APISourceConfig{
		BaseURL:          creds.APIBaseURL,
		Token:            creds.Token,
		QueueNames:       hpcQueueNames(creds.OrgID, creds.NodeID),
		ConsumerGroup:    workGroup,
		BlockMs:          workPollMs,
		MaxAttempts:      workMaxRetries,
		DebugFunc:        Debug,
		PlatformObserver: state.ObservePlatform,
		LogFn:            func(_ string, msg string) { Log("%s", msg) },
		WebSocketProxy:   http.ProxyFromEnvironment,
	})
	if err := connectWithBackoff(ctx, source); err != nil {
		Log("worker connect aborted: %v", err)
		return nil
	}
	client := source.Client()
	client.SetNodeMeta(creds.NodeID, creds.NodeID)
	enableWebSocketWithRetry(ctx, wsConnector{client: client}, workerWSLog)
	state.SetIdentity(fmt.Sprintf("citadel-hpc-%s", layout.JobID), source.Name(), workGroup, creds.NodeID, creds.OrgID)
	state.SetQueues(source.QueueNames())
	state.SetPerNodeQueue(nodeQueueName(creds.OrgID, creds.NodeID))

	fmt.Println("   - Profile: hpc (API only)")
	fmt.Printf("   - Node: %s\n", creds.NodeID)
	fmt.Printf("   - Job root: %s\n", layout.Root)

	var usageStore *usage.Store
	if store, err := usage.OpenStore(filepath.Join(layout.NodeDir, "usage.db")); err != nil {
		fmt.Fprintf(os.Stderr, "   - Warning: usage tracking disabled: %v\n", err)
	} else {
		usageStore = store
		defer usageStore.Close()
		if publishFn := createUsagePublishFn(true, source, "", ""); publishFn != nil {
			syncer := usage.NewSyncer(usage.SyncerConfig{Store: usageStore, PublishFn: publishFn})
			go func() {
				if err := syncer.Start(ctx); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "   - Warning: usage syncer error: %v\n", err)
				}
			}()
		}
	}

	var nodeRunner atomic.Pointer[worker.Runner]
	collector := hpcHeartbeatCollector(creds.NodeID, &nodeRunner, state, client.PubSubTransport)
	publisher, err := heartbeat.NewAPIPublisher(heartbeat.APIPublisherConfig{
		Client:       client,
		NodeID:       creds.NodeID,
		OrgID:        creds.OrgID,
		AgentVersion: Version,
		DebugFunc:    Debug,
		MarkerDir:    layout.NodeDir,
	}, collector)
	if err != nil {
		return fmt.Errorf("create heartbeat publisher: %w", err)
	}
	publisher.SetPermissionsProvider(func() *heartbeat.PermissionState {
		return permissionsToHeartbeat(hpcPermissions())
	})

	signer := nodeidentity.Convergent(layout.NodeDir)
	err = runHPCWorker(ctx, hpcWorkerDeps{
		Creds:          creds,
		Layout:         layout,
		Source:         source,
		StreamFactory:  worker.CreateAPIStreamWriterFactory(ctx, source),
		MaxConcurrency: workMaxConcurrency,
		Signer:         signer,
		UsageStore:     usageStore,
		State:          state,
		Log:            func(format string, args ...any) { Log(format, args...) },
		OnRunner: func(ctx context.Context, runner *worker.Runner) {
			startStatusPublisherAfterRunner(&nodeRunner, runner, func() {
				go func() {
					if err := publisher.Start(ctx); err != nil && err != context.Canceled {
						fmt.Fprintf(os.Stderr, "   - Warning: heartbeat publisher error: %v\n", err)
					}
				}()
			})
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
