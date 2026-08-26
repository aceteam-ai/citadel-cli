// cmd/worker_queues.go
package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// WorkerQueueParams bundles the API-mode boot-time queue resolution inputs
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
type WorkerQueueParams struct {
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
	// Skip short-circuits to the cpu-general-only base set: no
	// FetchWorkerConfig call, no shell queue, no inference queues, and no
	// reconciler gap. This models runTUIWorker's workerHeld case, where a
	// dedicated `citadel work` already owns this node's consumption and the
	// Control Center must not compete for it (the competing-consumer
	// incident referenced elsewhere in cmd/controlcenter.go) — subscribing
	// to anything here would be pure waste at best and a step back toward
	// that split at worst. runWork always passes false.
	Skip bool
	// DebugFn receives fetch/debug tracing. runWork wires Debug; runTUIWorker
	// wires its activity log. May be nil.
	DebugFn func(format string, args ...any)
}

// WorkerQueueResult is resolveWorkerQueues' output: the (possibly
// FetchWorkerConfig-resolved) WorkQueue and OrgID, the final boot-time queue
// list, and the residual inference-queue gap for InferenceQueueReconciler
// (see missingQueues' doc comment for why that gap can be nil).
type WorkerQueueResult struct {
	WorkQueue string
	OrgID     string
	Queues    []string
	Missing   []string
}

// resolveWorkerQueues resolves the API-mode boot-time Redis Streams queue
// set: cpu-general base, the FetchWorkerConfig-assigned workQueue, the
// per-org shellQueue, and inference queues -- in that order, matching
// runWork's pre-#839 inline logic exactly. See WorkerQueueParams for the
// shared contract and citadel-cli#839 for why runWork and runTUIWorker must
// produce an identical set for the same inputs.
func resolveWorkerQueues(ctx context.Context, p WorkerQueueParams) WorkerQueueResult {
	debug := p.DebugFn
	if debug == nil {
		debug = func(string, ...any) {}
	}

	if p.Skip {
		return WorkerQueueResult{
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

	return WorkerQueueResult{
		WorkQueue: workQueue,
		OrgID:     orgID,
		Queues:    queueNames,
		Missing:   missing,
	}
}
