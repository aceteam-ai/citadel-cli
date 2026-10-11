package cmd

import (
	"context"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// Generic sandboxed-module runtime capability for the heartbeat
// (citadel-cli#1288). The availability probe (container runtime present +
// reachable + able to enforce the sandbox cgroup limits) shells out to the
// runtime, so it is run ONCE and cached rather than on every heartbeat
// collection. "Probed at startup" in practice means on the first heartbeat; the
// result does not change for the life of the process. The SAME jobs probe backs
// the node-side dispatch refusal (jobs.EnsureSandboxedModuleAvailable), so the
// advertised capability and the refusal can never disagree.
//
// Limitation (documented, follow-up): because the probe is cached once, a node
// whose container runtime becomes reachable AFTER the first heartbeat keeps
// advertising unavailable until the worker restarts. A TTL re-probe is a
// follow-up; in practice the runtime is up before `citadel work`'s first
// heartbeat (systemd ordering / install.sh), so the window is narrow.
var (
	sandboxCapOnce sync.Once
	sandboxCapVal  *status.SandboxedModuleCapability
)

// sandboxedModuleCapability is the CollectorConfig.SandboxedModules provider.
func sandboxedModuleCapability() *status.SandboxedModuleCapability {
	sandboxCapOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st := jobs.ProbeSandboxedRuntime(ctx)
		sandboxCapVal = &status.SandboxedModuleCapability{
			Available: st.Available,
			Reason:    st.Reason,
		}
	})
	return sandboxCapVal
}
