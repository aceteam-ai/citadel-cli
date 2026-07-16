// internal/worker/instance_handler.go
//
// INSTANCE_* job handlers (aceteam#5963): EC2-style instance provisioning as a
// fabric capability. The platform dispatches org-scoped INSTANCE_PROVISION /
// INSTANCE_START / INSTANCE_STOP / INSTANCE_DESTROY / INSTANCE_STATUS jobs to
// nodes advertising the `hypervisor:proxmox` capability tag; this handler
// drives the local hypervisor through an injected provider (the live one wraps
// internal/proxmox.Provisioner).
//
// # Queue gating
//
// Instance jobs mutate the hypervisor, so they are honored only when they
// arrive on a queue the PLATFORM controls targeting: the hypervisor capability
// queue (jobs:v1:tag:hypervisor:...) or this node's per-node stream. The
// shared org shell pool is refused (fail closed), mirroring MODULE_SET /
// AGENT_UPDATE's privilege gating.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/proxmox"
)

// hypervisorQueuePrefix matches capability queues that route hypervisor work
// (e.g. jobs:v1:tag:hypervisor:proxmox, plus any WORKER_QUEUE_SUFFIX variant).
const hypervisorQueuePrefix = "jobs:v1:tag:hypervisor:"

// InstanceProvider is the hypervisor-side surface the handler drives. The live
// implementation is *proxmox.Provisioner; tests inject a fake.
type InstanceProvider interface {
	Provision(ctx context.Context, req proxmox.ProvisionRequest) (*proxmox.ProvisionResult, error)
	Start(ctx context.Context, ref proxmox.InstanceRef) error
	Stop(ctx context.Context, ref proxmox.InstanceRef) error
	Destroy(ctx context.Context, ref proxmox.InstanceRef, instanceID string) error
	Status(ctx context.Context, ref proxmox.InstanceRef) (*proxmox.InstanceStatus, error)
}

// InstanceHandlerConfig configures an InstanceHandler.
type InstanceHandlerConfig struct {
	// Provider lazily builds the hypervisor provider. Loading is deferred so a
	// node whose proxmox.json appears/changes after startup picks it up, and so
	// unconfigured nodes fail jobs with a clear message instead of at boot.
	Provider func() (InstanceProvider, error)
	// Log reports progress. Nil is a no-op.
	Log func(format string, args ...any)
}

// InstanceHandler processes INSTANCE_* jobs.
type InstanceHandler struct {
	cfg InstanceHandlerConfig

	// provisionAttempted dedupes INSTANCE_PROVISION by instance_id. The runner
	// Nacks failed jobs (redelivered up to MaxAttempts on this same per-node
	// stream), but provisioning is NOT idempotent: a redelivered attempt after
	// a partial failure could clone a second VM the platform never hears about
	// (its waiter already consumed the first error event). First attempt wins;
	// redeliveries fail terminally with a pointer to inspect hypervisor state.
	mu                 sync.Mutex
	provisionAttempted map[string]bool
}

// NewInstanceHandler constructs an INSTANCE_* handler.
func NewInstanceHandler(cfg InstanceHandlerConfig) *InstanceHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &InstanceHandler{cfg: cfg, provisionAttempted: map[string]bool{}}
}

// CanHandle reports whether this handler processes the given job type.
func (h *InstanceHandler) CanHandle(jobType string) bool {
	switch jobType {
	case JobTypeInstanceProvision, JobTypeInstanceStart, JobTypeInstanceStop,
		JobTypeInstanceDestroy, JobTypeInstanceStatus:
		return true
	}
	return false
}

// instancePayload is the wire payload shared by the INSTANCE_* job family.
// Provision uses the identity/sizing fields; lifecycle ops use vmid/pve_node.
type instancePayload struct {
	InstanceID        string   `json:"instance_id"`
	Name              string   `json:"name"`
	InstanceType      string   `json:"instance_type"`
	OrgID             string   `json:"org_id"`
	AuthKey           string   `json:"authkey"`
	LoginServer       string   `json:"login_server"`
	SSHAuthorizedKeys []string `json:"ssh_authorized_keys"`
	VMID              int      `json:"vmid"`
	PVENode           string   `json:"pve_node"`
}

// Execute dispatches one INSTANCE_* job to the provider.
func (h *InstanceHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	if !isInstanceQueue(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"%s refused: must arrive on a hypervisor capability queue or the per-node stream, got source queue %q",
			job.Type, job.SourceQueue)), nil
	}
	if h.cfg.Provider == nil {
		return h.failure(fmt.Errorf("%s handler is misconfigured: no provider factory", job.Type)), nil
	}

	p, err := parseInstancePayload(job.Payload)
	if err != nil {
		return h.failure(fmt.Errorf("%s: %w", job.Type, err)), nil
	}

	provider, err := h.cfg.Provider()
	if err != nil {
		// Not configured / disabled is terminal for this node: retrying the same
		// job here cannot help, and failing fast lets the platform reschedule.
		return h.failure(fmt.Errorf("%s: hypervisor provider unavailable: %w", job.Type, err)), nil
	}

	switch job.Type {
	case JobTypeInstanceProvision:
		return h.provision(ctx, provider, p)
	case JobTypeInstanceStart, JobTypeInstanceStop, JobTypeInstanceDestroy, JobTypeInstanceStatus:
		return h.lifecycle(ctx, provider, job.Type, p)
	default:
		return h.failure(fmt.Errorf("unhandled instance job type %q", job.Type)), nil
	}
}

func (h *InstanceHandler) provision(ctx context.Context, provider InstanceProvider, p instancePayload) (*JobResult, error) {
	if p.InstanceID != "" {
		h.mu.Lock()
		attempted := h.provisionAttempted[p.InstanceID]
		h.provisionAttempted[p.InstanceID] = true
		h.mu.Unlock()
		if attempted {
			return h.failure(fmt.Errorf(
				"INSTANCE_PROVISION refused: instance %s was already attempted on this node "+
					"(likely a redelivered failed job); inspect the hypervisor before re-provisioning",
				p.InstanceID)), nil
		}
	}
	h.cfg.Log("INSTANCE_PROVISION: instance=%s org=%s type=%s", p.InstanceID, p.OrgID, p.InstanceType)
	res, err := provider.Provision(ctx, proxmox.ProvisionRequest{
		InstanceID:        p.InstanceID,
		Name:              p.Name,
		InstanceType:      p.InstanceType,
		OrgID:             p.OrgID,
		AuthKey:           p.AuthKey,
		LoginServer:       p.LoginServer,
		SSHAuthorizedKeys: p.SSHAuthorizedKeys,
	})
	if err != nil {
		// Provisioning is NOT idempotent (a retry would clone a second VM), so
		// fail terminally and let the platform decide whether to re-dispatch.
		return h.failure(fmt.Errorf("INSTANCE_PROVISION: %w", err)), nil
	}
	return &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{
			"instance_id": p.InstanceID,
			"vmid":        res.VMID,
			"pve_node":    res.PVENode,
			"name":        res.Name,
			"cores":       res.Cores,
			"memory_mb":   res.MemoryMB,
			"disk_gb":     res.DiskGB,
			"pool":        res.Pool,
		},
	}, nil
}

func (h *InstanceHandler) lifecycle(ctx context.Context, provider InstanceProvider, jobType string, p instancePayload) (*JobResult, error) {
	if p.VMID <= 0 {
		return h.failure(fmt.Errorf("%s: vmid is required", jobType)), nil
	}
	ref := proxmox.InstanceRef{VMID: p.VMID, PVENode: p.PVENode}
	h.cfg.Log("%s: instance=%s vmid=%d", jobType, p.InstanceID, p.VMID)

	out := map[string]any{"instance_id": p.InstanceID, "vmid": p.VMID}
	var err error
	switch jobType {
	case JobTypeInstanceStart:
		err = provider.Start(ctx, ref)
		out["state"] = "running"
	case JobTypeInstanceStop:
		err = provider.Stop(ctx, ref)
		out["state"] = "stopped"
	case JobTypeInstanceDestroy:
		err = provider.Destroy(ctx, ref, p.InstanceID)
		out["state"] = "destroyed"
	case JobTypeInstanceStatus:
		var st *proxmox.InstanceStatus
		st, err = provider.Status(ctx, ref)
		if err == nil {
			out["state"] = st.Status
			out["uptime_seconds"] = st.UptimeSeconds
			out["cpus"] = st.CPUs
			out["max_mem_bytes"] = st.MaxMemBytes
		}
	}
	if err != nil {
		// Lifecycle ops are idempotent on the PVE side; a transient API error is
		// worth a retry (DLQ-bounded by the runner's MaxAttempts).
		return h.retry(fmt.Errorf("%s: %w", jobType, err)), nil
	}
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

// isInstanceQueue reports whether a source queue is allowed to carry
// INSTANCE_* jobs: the hypervisor capability queue or the per-node stream.
func isInstanceQueue(sourceQueue string) bool {
	return strings.HasPrefix(sourceQueue, hypervisorQueuePrefix) || isPerNodeStream(sourceQueue)
}

// parseInstancePayload decodes the job payload into the typed instance payload.
func parseInstancePayload(payload map[string]any) (instancePayload, error) {
	var p instancePayload
	if payload == nil {
		return p, fmt.Errorf("empty payload")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return p, fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("decode instance payload: %w", err)
	}
	return p, nil
}

func (h *InstanceHandler) failure(err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Error:  err,
		Output: map[string]any{"error": err.Error()},
	}
}

func (h *InstanceHandler) retry(err error) *JobResult {
	return &JobResult{
		Status: JobStatusRetry,
		Error:  err,
		Output: map[string]any{"error": err.Error()},
	}
}

// Ensure InstanceHandler implements JobHandler.
var _ JobHandler = (*InstanceHandler)(nil)
