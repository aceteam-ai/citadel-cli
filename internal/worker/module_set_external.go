package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/externalengine"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// ExternalModuleConfig keeps the external action completely outside managed
// module operations. All lifecycle edges are injected for hermetic tests.
type ExternalModuleConfig struct {
	NodeID, OrgID, Dir string
	Ops                reconcile.ModuleOps
	ManagedVLLMRunning func() bool
	Load               func(string) (*externalengine.Config, error)
	Save               func(string, externalengine.Config) error
	Restore            func(string, *externalengine.Config) error
	Probe              func(context.Context, externalengine.Endpoint, string) error
	Snapshot           func() error
	BeginDrain         func() (release func())
	ActiveJobs         func() int
	Restart            func() error
	IdleTimeout        time.Duration
	Log                func(string, ...any)
	mu                 sync.Mutex
}

func (h *ModuleSetHandler) executeExternal(ctx context.Context, job *Job) *JobResult {
	cfg := h.cfg.External
	if cfg == nil {
		return h.failure(fmt.Errorf("external engine lifecycle is unavailable"))
	}
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	if cfg.Load == nil {
		cfg.Load = externalengine.LoadPersisted
	}
	if cfg.Save == nil {
		cfg.Save = externalengine.Save
	}
	if cfg.Restore == nil {
		cfg.Restore = externalengine.Restore
	}
	if cfg.Probe == nil {
		cfg.Probe = externalengine.Probe
	}
	if cfg.Restart == nil {
		cfg.Restart = update.RestartProcess
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Second
	}

	// Current is called before persistence so this process keeps its original
	// route even if another constructor has not yet initialized inference.
	if cfg.Snapshot == nil {
		cfg.Snapshot = func() error { _, err := externalengine.Current(); return err }
	}
	if err := cfg.Snapshot(); err != nil {
		// Snapshot is still valuable when startup state is invalid: Current has
		// cached that failure, so this process cannot route through a newly
		// written record before the successful re-exec. Continue so a newer
		// desired revision can repair a stale interface binding.
		cfg.Log("current external engine config is unavailable; continuing recovery: %v", err)
	}
	if job == nil || job.Payload == nil {
		return h.failure(fmt.Errorf("missing job payload"))
	}
	source, ok := job.Payload["source"].(string)
	if !ok || source != "vllm" {
		return h.failure(fmt.Errorf("external action requires source vllm"))
	}
	status, _ := job.Payload["desired_status"].(string)
	if status != "adopt_external" && status != "detach_external" {
		return h.failure(fmt.Errorf("unsupported external action"))
	}
	target, ok := job.Payload["target_node"].(string)
	if !ok || target == "" || target != cfg.NodeID || target[0] == '0' || strings.Trim(target, "0123456789") != "" {
		return h.failure(fmt.Errorf("external action requires the current canonical numeric target_node"))
	}
	if cfg.OrgID == "" || job.SourceQueue != fmt.Sprintf("jobs:v1:shell:org_%s:node:%s", cfg.OrgID, cfg.NodeID) {
		return h.failure(fmt.Errorf("external action requires the exact owner node stream"))
	}
	var envelope struct {
		Version   int    `json:"version"`
		Host      string `json:"host"`
		Port      int    `json:"host_port"`
		Model     string `json:"model"`
		Revision  string `json:"revision"`
		RequestID string `json:"request_id"`
	}
	raw, err := json.Marshal(job.Payload["external_engine"])
	if err != nil || string(raw) == "null" {
		return h.failure(fmt.Errorf("missing external_engine envelope"))
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return h.failure(fmt.Errorf("invalid external_engine envelope: %w", err))
	}
	mode := "adopted"
	if status == "detach_external" {
		mode = "detached"
	}
	previous, err := cfg.Load(cfg.Dir)
	if err != nil {
		return h.failure(fmt.Errorf("load external engine config: %w", err))
	}
	candidate := externalengine.Config{Version: envelope.Version, Mode: mode, Endpoint: externalengine.Endpoint{Host: envelope.Host, Port: envelope.Port}, Model: envelope.Model, Revision: envelope.Revision, RequestID: envelope.RequestID, NodeID: target}
	if candidate.Endpoint.Host == "" && mode == "adopted" {
		candidate.Endpoint.Host = "localhost"
	}
	if mode == "detached" && previous != nil {
		candidate.Endpoint, candidate.Model = previous.Endpoint, previous.Model
	}
	candidate, err = externalengine.Validate(candidate, mode == "adopted")
	if err != nil {
		return h.failure(err)
	}
	if previous != nil {
		oldRev, _ := externalengine.ValidateRevision(previous.Revision)
		newRev, _ := externalengine.ValidateRevision(candidate.Revision)
		switch newRev.Cmp(oldRev) {
		case -1:
			return h.failure(fmt.Errorf("stale external engine revision"))
		case 0:
			if previous.RequestID == candidate.RequestID && previous.ConfigRef == candidate.ConfigRef {
				return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"status": "already_applied", "revision": candidate.Revision, "config_ref": candidate.ConfigRef}}
			}
			return h.failure(fmt.Errorf("conflicting external engine revision"))
		}
	}
	if cfg.Ops == nil {
		return h.failure(fmt.Errorf("module ownership check unavailable"))
	}
	installed, err := cfg.Ops.ListInstalled(ctx)
	if err != nil {
		return h.retry(fmt.Errorf("inspect managed vllm ownership: %w", err))
	}
	for _, module := range installed {
		if module.Name == "vllm" || module.Source == "vllm" {
			return h.failure(fmt.Errorf("managed vllm is installed; external action refused"))
		}
	}
	if cfg.ManagedVLLMRunning != nil && cfg.ManagedVLLMRunning() {
		return h.failure(fmt.Errorf("Citadel-managed vllm container is running; external action refused"))
	}
	if mode == "adopted" {
		if err := cfg.Probe(ctx, candidate.Endpoint, candidate.Model); err != nil {
			return h.failure(fmt.Errorf("external vllm probe: %w", err))
		}
	}
	if err := cfg.Save(cfg.Dir, candidate); err != nil {
		return h.retry(fmt.Errorf("persist external engine config: %w", err))
	}
	if cfg.BeginDrain != nil && cfg.ActiveJobs != nil && cfg.Restart != nil {
		releaseDrain := cfg.BeginDrain()
		go cfg.restartAfterAck(candidate.Revision, previous, releaseDrain)
	}
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"status": "applied_pending_reload", "revision": candidate.Revision, "config_ref": candidate.ConfigRef,
		"desired_status": status, "converged": false,
	}}
}

func (cfg *ExternalModuleConfig) restartAfterAck(revision string, previous *externalengine.Config, releaseDrain func()) {
	deadline := time.Now().Add(cfg.IdleTimeout)
	for cfg.ActiveJobs() != 0 {
		if time.Now().After(deadline) {
			cfg.Log("external engine revision %s pending reload: drain timed out", revision)
			cfg.rollbackAfterFailedRestart(revision, previous)
			if releaseDrain != nil {
				releaseDrain()
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := cfg.Restart(); err != nil {
		cfg.Log("external engine revision %s pending reload: reexec failed: %v", revision, err)
		cfg.rollbackAfterFailedRestart(revision, previous)
		if releaseDrain != nil {
			releaseDrain()
		}
	}
}

func (cfg *ExternalModuleConfig) rollbackAfterFailedRestart(revision string, previous *externalengine.Config) {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	current, err := cfg.Load(cfg.Dir)
	if err != nil || current == nil || current.Revision != revision {
		return
	}
	if err := cfg.Restore(cfg.Dir, previous); err != nil {
		cfg.Log("external engine revision %s rollback failed: %v", revision, err)
	}
}
