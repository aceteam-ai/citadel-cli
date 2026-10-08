// cmd/worker_queues.go
package cmd

import (
	"context"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// workerQueueParams bundles the API-mode boot-time queue resolution inputs
// shared by `citadel work` (runWork, cmd/work.go) and the Control Center's
// worker path (runTUIWorker, cmd/controlcenter.go).
//
// citadel-cli#839: before this, runTUIWorker built its own inline queue list
// (resolveControlCenterInferenceQueues) that started from the cpu-general
// base and added inference queues (#612/#823/#837), but — unlike runWork —
// never fetched the platform-assigned FetchWorkerConfig workQueue and never
// joined the per-org shellQueueName. A node run via the Control Center could
// therefore never receive jobs dispatched to either queue, silently
// diverging from an equivalent `citadel work` node. resolveWorkerQueues is
// the single place both entry points now build this set, so they cannot
// drift apart again.
type workerQueueParams struct {
	// APIBaseURL and Token authenticate the FetchWorkerConfig lookup.
	APIBaseURL string
	Token      string
	// WorkQueue is the caller's already-known queue, if any (runWork's
	// --queue flag). Empty means "resolve it from FetchWorkerConfig".
	WorkQueue string
	// OrgID is the caller's already-known org id, if any. Empty means
	// "resolve it from FetchWorkerConfig too" — mirrors runWork, which only
	// fetches when WorkQueue=="" || OrgID=="".
	OrgID string
	// NodeCaps and Serving feed capabilities.InferenceQueues, exactly as
	// runWork's nodeCaps and nodeIsServingModels(ctx) do.
	NodeCaps *capabilities.NodeCapabilities
	Serving  bool
	// WorkerHeld short-circuits to the cpu-general-only base set: no
	// FetchWorkerConfig call, no shell queue, no inference queues, and no
	// reconciler gap. This models runTUIWorker's workerHeld case, where a
	// dedicated `citadel work` already owns this node's consumption and the
	// Control Center must not compete for it (the competing-consumer
	// incident referenced elsewhere in cmd/controlcenter.go) — subscribing
	// to anything here would be pure waste at best and a step back toward
	// that split at worst. runWork always passes false (it has no
	// workerHeld concept of its own).
	WorkerHeld bool
	// DebugFn receives fetch/debug tracing. runWork wires Debug; runTUIWorker
	// wires its activity log. May be nil.
	DebugFn func(format string, args ...any)
}

// workerQueueResult is resolveWorkerQueues' output: the (possibly
// FetchWorkerConfig-resolved) WorkQueue and OrgID, the final boot-time queue
// list, and the residual inference-queue gap for InferenceQueueReconciler
// (see missingQueues' doc comment for why that gap can be nil).
type workerQueueResult struct {
	WorkQueue string
	OrgID     string
	Queues    []string
	Missing   []string
}

// resolveWorkerQueues resolves the API-mode boot-time Redis Streams queue
// set: cpu-general base, the FetchWorkerConfig-assigned workQueue, the
// per-org shellQueue, and inference queues -- in that order, matching
// runWork's pre-#839 inline logic exactly. See workerQueueParams for the
// shared contract and citadel-cli#839 for why runWork and runTUIWorker must
// produce an identical set for the same inputs.
//
// Callers must NOT write the returned OrgID back onto their own org-identity
// state unless that call site already did so before #839: runWork's original
// inline block always did (deviceConfig.OrgID = workerCfg.OrgID), so its
// call site preserves that write-back exactly. runTUIWorker's pre-#839 code
// never called FetchWorkerConfig at all, so its call site deliberately does
// NOT propagate the resolved OrgID elsewhere -- doing so would let a
// FetchWorkerConfig-resolved org silently outrank the Control Center's
// existing deviceConfig/manifest org-resolution fallback for callers
// downstream of queue resolution (per-node shell stream, telemetry), which
// is a behavior change outside #839's stated scope (queue-set parity only).
func resolveWorkerQueues(ctx context.Context, p workerQueueParams) workerQueueResult {
	debug := p.DebugFn
	if debug == nil {
		debug = func(string, ...any) {}
	}

	if p.WorkerHeld {
		return workerQueueResult{
			WorkQueue: p.WorkQueue,
			OrgID:     p.OrgID,
			Queues:    []string{worker.DefaultCPUQueue},
		}
	}

	workQueue := p.WorkQueue
	orgID := p.OrgID

	// Fetch worker config from API (queue, org) when either is still
	// unknown. This replaces the need for WORKER_QUEUE env vars. Consumer
	// group is resolved elsewhere from node identity.
	if workQueue == "" || orgID == "" {
		tempClient := redisapi.NewClient(redisapi.ClientConfig{
			BaseURL:   p.APIBaseURL,
			Token:     p.Token,
			DebugFunc: debug,
		})
		workerCfg, err := tempClient.FetchWorkerConfig(ctx)
		if err != nil {
			debug("worker-config fetch failed: %v (using defaults)", err)
		} else if workerCfg != nil {
			debug("worker-config: queue=%s, group=%s, org=%s",
				workerCfg.Queue, workerCfg.ConsumerGroup, workerCfg.OrgID)
			if workQueue == "" && workerCfg.Queue != "" {
				workQueue = workerCfg.Queue
			}
			if orgID == "" && workerCfg.OrgID != "" {
				orgID = workerCfg.OrgID
			}
		} else {
			debug("worker-config: endpoint not available, using defaults")
		}
		_ = tempClient.Close()
	}

	// Build queue list: primary queue + per-org shell queue. Ensure a base
	// queue is always present so that appending the shell queue does not
	// suppress the NewAPISource default.
	var queueNames []string
	if workQueue != "" {
		queueNames = append(queueNames, workQueue)
	}
	if orgID != "" {
		shellQueue := shellQueueName(orgID)
		if len(queueNames) == 0 {
			queueNames = []string{worker.DefaultCPUQueue}
		}
		queueNames = append(queueNames, shellQueue)
		debug("shell queue: %s", shellQueue)
	}

	// Guarantee the base queue survives even when neither workQueue nor
	// orgID resolved to anything (FetchWorkerConfig unreachable/empty and no
	// org known -- authkey flow or a stale/incomplete config). Without this,
	// a GPU-capable or already-serving node would fall through to the
	// inference-queues block below with queueNames still nil, and that block
	// would leave it holding ONLY the inference queue (e.g.
	// ["jobs:v1:gpu-general"]) -- non-empty, so worker.NewAPISource's own
	// zero-value default (which only fires when QueueNames is COMPLETELY
	// empty) would never kick in to add cpu-general back, and the node would
	// silently never consume shell/config/general dispatch for its whole
	// process lifetime. This only fires when queueNames is still empty at
	// this point, so it changes nothing for the common case (workQueue or
	// orgID present) -- runWork's resulting set for those cases is
	// byte-identical to before this guard existed.
	if len(queueNames) == 0 {
		queueNames = []string{worker.DefaultCPUQueue}
	}

	// Inference-capable nodes must also consume the GPU inference queues.
	// Additive to whatever worker-config returned. See
	// capabilities.InferenceQueues' doc comment for the GPU/serving rules.
	if infQueues := capabilities.InferenceQueues(p.NodeCaps, p.Serving); len(infQueues) > 0 {
		queueNames = appendUniqueQueues(queueNames, infQueues)
		debug("inference node (serving=%t): also subscribing to inference queues %v", p.Serving, infQueues)
	}

	// missingQueues diffs the "if serving" set against what boot already
	// subscribed, so a GPU node or a node already serving at boot reports no
	// gap (GPUInferenceQueues is unconditional on `serving`); only a fresh
	// CPU-only node with no engine yet has one. See missingQueues' own doc
	// comment for the full reasoning.
	missing := missingQueues(capabilities.InferenceQueues(p.NodeCaps, true), queueNames)

	return workerQueueResult{
		WorkQueue: workQueue,
		OrgID:     orgID,
		Queues:    queueNames,
		Missing:   missing,
	}
}

// resolveDirectRedisQueues is the authority for the Redis Streams queue set a
// direct-Redis worker (`citadel work` with a config redis_url -- the offline /
// self-host path, e.g. the asusrog node) subscribes to. It is the direct-Redis
// analogue of resolveWorkerQueues, extracted from runWork's inline block so the
// rule is pure/string-in-string-out and can be pinned by a test.
//
// The rules, and why each is load-bearing:
//
//   - explicitQueue (`--queue`) is honored VERBATIM as the sole base queue
//     (plus the per-org shell queue below). This is the documented single-queue
//     workaround/back-compat contract; cpu-general is deliberately NOT forced
//     onto it, because an operator who pins `--queue X` means X.
//
//   - Otherwise the base set ALWAYS contains worker.DefaultCPUQueue
//     (jobs:v1:cpu-general) -- the citadel-cli#1159 fix. The coordinator
//     dispatches every unpinned job that has no capability engine to route by
//     and no target_node (e.g. SYNTHESIZE_SPEECH) to jobs:v1:cpu-general (its
//     default_queue, aceteam fabric_dispatch). Before this, the direct-Redis
//     resolver hardcoded only jobs:v1:gpu-general as its base, so a CPU/TTS node
//     silently never consumed cpu-general -- a dark node with a healthy-looking
//     heartbeat. This mirrors the API-mode path (resolveWorkerQueues), which
//     starts every node from the cpu-general base.
//
//   - jobs:v1:gpu-general stays in the set UNCONDITIONALLY (additive to the
//     cpu-general fix, never a replacement). Direct-Redis mode has always joined
//     gpu-general regardless of `serving`, and the Dynamic Inference-Queue
//     Resubscription design (citadel-cli#612) relies on that: a target_node-
//     pinned inference job for this node lands on gpu-general, and a CPU node
//     that later starts an engine needs no boot-time reconciler because it is
//     already subscribed.
//
//   - Per-capability tag queues (os/arch/gpu/engine/... via
//     capabilities.ResolveQueues) and the per-org shell queue are joined as
//     before. cpu:general is intentionally absorbed by the base queue, not
//     emitted as a jobs:v1:tag:cpu:general queue (ResolveQueues skips it).
//
// worker.DefaultCPUQueue is placed first so queueNames[0] -- the RedisSource
// "primary" / backwards-compat ClientConfig.QueueName -- matches API mode's
// cpu-general base. This is cosmetic: RedisSource.nextMulti acks/DLQs against
// each job's own origin queue (SourceQueue), and EnsureConsumerGroups creates a
// group for every queue, so no queue is privileged by being index 0.
func resolveDirectRedisQueues(explicitQueue string, nodeTags []string, manual []capabilities.Capability, orgID string) []string {
	var queueNames []string
	if explicitQueue != "" {
		// Explicit --queue: honor it verbatim (documented workaround / back-compat).
		queueNames = []string{explicitQueue}
	} else {
		allCaps := make([]capabilities.Capability, 0, len(nodeTags)+len(manual))
		for _, tag := range nodeTags {
			category := tag
			if idx := strings.Index(tag, ":"); idx > 0 {
				category = tag[:idx]
			}
			allCaps = append(allCaps, capabilities.Capability{Tag: tag, Category: category})
		}
		allCaps = append(allCaps, manual...)

		// cpu-general base (the fix) + gpu-general (unconditional) + per-tag
		// queues. ResolveQueues always appends its gpu-general base queue even
		// when allCaps is empty, so gpu-general survives for a tag-less node too.
		queueNames = []string{worker.DefaultCPUQueue}
		queueNames = appendUniqueQueues(queueNames, capabilities.ResolveQueues(allCaps, "jobs:v1:gpu-general"))
	}

	// Per-org shell queue, appended for BOTH branches (unchanged behavior: the
	// pre-#1159 code appended it after whichever base it had resolved).
	if orgID != "" {
		queueNames = appendUniqueQueues(queueNames, []string{shellQueueName(orgID)})
	}

	return queueNames
}
