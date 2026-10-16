package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func nodeWorkerControlHandler(opts nodeJobHandlerOpts) *worker.WorkerControlHandler {
	return worker.NewWorkerControlHandler(worker.WorkerControlConfig{
		OrgID: opts.OrgID, NodeID: opts.NodeID, StateDir: opts.PermissionsDir,
		Managed: managedByServiceManager,
		Schedule: func() (func(), func(), error) {
			return worker.PrepareWorkerRestart(func() { processExiter(1) })
		},
		Log: opts.HandlerLog,
	})
}

// Own a separate source, consumer group and runner so normal job admission,
// draining or a saturated execution lane cannot starve restart recovery.
// The caller supplies the persisted local org, never a worker-config fallback.
func startWorkerControl(ctx context.Context, source worker.JobSource, localOrgID string, opts nodeJobHandlerOpts) func() {
	noop := func() {}
	api, ok := source.(*worker.APISource)
	if !ok || localOrgID == "" || opts.NodeID == "" {
		return noop
	}
	control, err := api.WorkerControlSource(ctx, localOrgID, opts.NodeID)
	if err != nil {
		Log("worker control unavailable: %v", err)
		return noop
	}
	if control == nil {
		return noop
	}
	controlCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	opts.OrgID = localOrgID
	runner := worker.NewRunner(control, []worker.JobHandler{nodeWorkerControlHandler(opts)}, worker.RunnerConfig{
		NodeID: opts.NodeID, AgentVersion: Version, MaxConcurrency: 1,
		ActivityFn: opts.LogFn,
	})
	go func() {
		defer close(done)
		if err := connectWithBackoff(controlCtx, control); err != nil {
			return
		}
		enableWebSocketWithRetry(controlCtx, wsConnector{client: control.Client()}, workerWSLog)
		runner.WithStreamWriterFactory(worker.CreateAPIStreamWriterFactory(controlCtx, control))
		if err := runner.Run(controlCtx); err != nil && controlCtx.Err() == nil {
			Log("worker control stopped: %v", err)
		}
	}()
	return func() { cancel(); <-done }
}
