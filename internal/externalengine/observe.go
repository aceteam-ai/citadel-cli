package externalengine

import (
	"context"
	"time"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ObservedModule reports the explicit external record without claiming managed
// module ownership. A fresh exact-model probe is required for HEALTHY.
func ObservedModule(ctx context.Context) *fabricpb.ActualModule {
	c, err := Current()
	if err != nil || c == nil {
		return nil
	}
	return observedModule(ctx, *c, Probe)
}

func observedModule(ctx context.Context, c Config, probe func(context.Context, Endpoint, string) error) *fabricpb.ActualModule {
	m := &fabricpb.ActualModule{
		Source: "vllm", ConfigRef: c.ConfigRef, UpdatedAt: timestamppb.Now(),
	}
	if c.Mode == "detached" {
		m.Status = fabricpb.ModuleStatus_MODULE_STATUS_STOPPED
		m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY
		return m
	}
	m.Status = fabricpb.ModuleStatus_MODULE_STATUS_RUNNING
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := probe(probeCtx, c.Endpoint, c.Model); err != nil {
		m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY
		m.Error = err.Error()
		return m
	}
	m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY
	m.InstalledVersion = c.Model // observed exact model; this is not a package version
	return m
}

// AttachObservedModule refuses to duplicate a managed vllm row.
func AttachObservedModule(ctx context.Context, state *fabricpb.ActualState) {
	if state == nil {
		return
	}
	c, err := Current()
	if err != nil || c == nil || c.NodeID != state.NodeId {
		return
	}
	m := observedModule(ctx, *c, Probe)
	for _, existing := range state.Modules {
		if existing.Source == "vllm" {
			return
		}
	}
	state.Modules = append(state.Modules, m)
}
