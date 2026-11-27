package worker

import (
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

func servicePolicyRefusal(reason string) error {
	return &jobs.ShellRefusal{Reason: reason, Message: "Service operation refused by node policy: " + reason}
}

func newServiceLegacyHandlerAdapter(kind string, handler jobs.JobHandler, opts LegacyHandlerOpts) *LegacyHandlerAdapter {
	wired := func() bool {
		return inventoryQueueID.MatchString(opts.ServiceOrgID) &&
			inventoryQueueID.MatchString(opts.ServiceNodeID) && opts.PermissionsDir != ""
	}
	a := newGatedLegacyHandlerAdapter(kind, handler, func() bool {
		if !wired() {
			return false
		}
		p, err := config.LoadPermissionsStrict(opts.PermissionsDir)
		return err == nil && p.Services
	})
	a.jobGate = func(job *Job) error {
		if job == nil || job.Type != kind || !wired() {
			return servicePolicyRefusal("service_policy_unavailable")
		}
		if (job.Source != "redis" && job.Source != "redis-api") ||
			job.SourceQueue != "jobs:v1:shell:org_"+opts.ServiceOrgID+":node:"+opts.ServiceNodeID {
			return servicePolicyRefusal("service_stream_mismatch")
		}
		// Older service requests carry only the service name. When transport
		// targeting fields are present they must agree with the local identity.
		for _, key := range []string{"node_id", "target_node"} {
			if value, present := job.Payload[key]; present && value != opts.ServiceNodeID {
				return servicePolicyRefusal("service_node_mismatch")
			}
		}
		p, err := config.LoadPermissionsStrict(opts.PermissionsDir)
		if err != nil {
			return servicePolicyRefusal("service_policy_unavailable")
		}
		if !p.Services {
			return servicePolicyRefusal("services_disabled")
		}
		if kind == JobTypeServiceStart {
			requested := false
			if value := job.Payload["host_network"]; value != nil {
				switch v := value.(type) {
				case bool:
					requested = v
				case string:
					if strings.TrimSpace(v) != "" {
						requested, err = strconv.ParseBool(strings.TrimSpace(v))
					}
				default:
					return servicePolicyRefusal("invalid_request")
				}
				if err != nil {
					return servicePolicyRefusal("invalid_request")
				}
			}
			if requested && !p.HostNetwork {
				return servicePolicyRefusal("host_network_disabled")
			}
		}
		return nil
	}
	return a
}
