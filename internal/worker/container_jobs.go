package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/containerjobs"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/google/uuid"
)

type ContainerJobHandler struct {
	kind    string
	engine  *containerjobs.Engine
	enabled func() bool
}

func (h *ContainerJobHandler) CanHandle(kind string) bool {
	return kind == h.kind && h.engine != nil && h.enabled != nil && h.enabled()
}
func (h *ContainerJobHandler) RefusalFor(kind string) *jobs.ShellRefusal {
	if kind != h.kind || h.CanHandle(kind) {
		return nil
	}
	return &jobs.ShellRefusal{Reason: "container_host_disabled", Message: "typed container hosting requires an enabled, supported container host"}
}
func (h *ContainerJobHandler) DeclaredCapabilities(kind string) (jobs.JobCapabilities, bool) {
	return jobs.JobCapabilities{Lane: jobs.LaneHeavy, Deadline: jobs.DeadlineLong}, kind == h.kind
}
func (h *ContainerJobHandler) Execute(ctx context.Context, job *Job, _ StreamWriter) (*JobResult, error) {
	if !h.CanHandle(job.Type) {
		return nil, &jobs.ShellRefusal{Reason: "container_host_disabled", Message: "typed container hosting is disabled"}
	}
	if !isPerNodeStream(job.SourceQueue) {
		return nil, &jobs.ShellRefusal{Reason: "container_operation_failed", Message: containerjobs.ErrUnavailable.Error()}
	}
	// Keep exact int64 fields: the diagnostic map may already contain float64s.
	// A source without original payload bytes supplies no execution authority.
	raw := job.RawPayload
	if len(raw) == 0 {
		return nil, &jobs.ShellRefusal{Reason: "container_request_invalid", Message: containerjobs.ErrInvalid.Error()}
	}
	request, err := containerjobs.Parse(job.Type, raw)
	if err != nil {
		return nil, &jobs.ShellRefusal{Reason: "container_request_invalid", Message: err.Error()}
	}
	result, err := h.engine.RunDelivery(ctx, job.Type, request, job.ID)
	if err != nil {
		if errors.Is(err, containerjobs.ErrOwnership) || errors.Is(err, containerjobs.ErrNotFound) {
			err = containerjobs.ErrUnavailable
		}
		// Never automatically repeat a create/write/exec/purge after uncertainty,
		// and never reflect native command output, capability URLs or file bytes.
		return nil, &jobs.ShellRefusal{Reason: "container_operation_failed", Message: err.Error()}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil, containerjobs.ErrUnavailable
	}
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

func newContainerJobHandlers(dir, goos string) []JobHandler {
	return containerJobHandlersWithReadiness(dir, goos, containerjobs.NativeSupported())
}

func containerJobHandlersWithReadiness(dir, goos string, supported bool) []JobHandler {
	org := func() string {
		creds := config.LoadDeviceCreds(dir)
		parsed, err := uuid.Parse(creds.OrgID)
		if creds.Token == "" || err != nil || parsed == uuid.Nil || parsed.String() != creds.OrgID {
			return ""
		}
		return creds.OrgID
	}
	enabled := func() bool {
		if !filepath.IsAbs(dir) || goos != "linux" || !supported {
			return false
		}
		p, err := config.LoadPermissionsStrict(dir)
		return err == nil && p.ContainerHost && org() != "" && containerHostLimits(p).Valid() && (p.ContainerRuntime == "" || p.ContainerRuntime == "runsc") && containerjobs.ValidEgressProxyURL(p.ContainerEgressProxyURL)
	}
	policy := func() (containerjobs.HostPolicy, error) {
		p, err := config.LoadPermissionsStrict(dir)
		if err != nil {
			return containerjobs.HostPolicy{}, containerjobs.ErrUnavailable
		}
		return containerjobs.HostPolicy{Runtime: p.ContainerRuntime, EgressProxyURL: p.ContainerEgressProxyURL, Limits: containerHostLimits(p)}, nil
	}
	engine := &containerjobs.Engine{Runtime: containerjobs.NewNativeWithPolicy(containerjobs.HostRoot, policy), Transport: containerjobs.NewHTTPTransport(), Authorize: enabled, EnrolledOrg: org}
	handlers := make([]JobHandler, 0, len(containerjobs.Types))
	for _, kind := range containerjobs.Types {
		handlers = append(handlers, &ContainerJobHandler{kind: kind, engine: engine, enabled: enabled})
	}
	return handlers
}

func containerHostLimits(p *config.Permissions) containerjobs.HostLimits {
	return containerjobs.HostLimits{MaxQuotaBytes: p.ContainerMaxQuotaBytes, MaxVolumes: p.ContainerMaxVolumes, MaxMemoryBytes: p.ContainerMaxMemoryBytes, MaxPids: p.ContainerMaxPids, MaxCPUs: p.ContainerMaxCPUs, MinFreeBytes: p.ContainerMinFreeBytes}
}
