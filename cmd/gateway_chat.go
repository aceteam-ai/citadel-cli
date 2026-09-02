package cmd

import (
	"context"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// gateway_chat.go wires the gateway's model->engine chat router (issue #581,
// node-side complement of aceteam #6236) to this node's live engine discovery.
// Shared by `citadel work --gateway` (cmd/work.go) and `citadel serve`
// (cmd/serve.go) so both gateways expose /v1/chat/completions identically.

// chatListerTTL bounds how long a discovered engine->model map is reused before
// a fresh probe. status.DiscoverLocalEngines runs `docker inspect` + an engine
// HTTP probe (bounded by status.ModelDiscoveryTimeout) per running engine, which
// is too heavy to run on every chat request (it would add seconds of latency
// before the first streamed token on a multi-engine node). A few seconds of
// staleness is acceptable: the only failure modes are a transient 404 on a
// just-loaded model or a transient 502 on a just-unloaded one, both of which
// self-correct on the next refresh.
const chatListerTTL = 5 * time.Second

// newLocalChatLister builds a gateway.ChatModelLister backed by
// status.DiscoverLocalEngines with a short TTL cache. The returned closure is
// safe for concurrent use (the gateway calls it from request goroutines).
func newLocalChatLister() gateway.ChatModelLister {
	var (
		mu     sync.Mutex
		cached []gateway.ChatUpstream
		expiry time.Time
	)
	return func() []gateway.ChatUpstream {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Now().Before(expiry) {
			return cached
		}
		// Bound the probe a touch above ModelDiscoveryTimeout so a slow engine
		// never stalls a chat request indefinitely.
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		engines := status.DiscoverLocalEngines(ctx)
		out := make([]gateway.ChatUpstream, 0, len(engines))
		for _, e := range engines {
			out = append(out, gateway.ChatUpstream{Engine: e.Name, Port: e.Port, Models: e.Models})
		}
		cached = out
		expiry = time.Now().Add(chatListerTTL)
		return out
	}
}

// installedListerTTL is longer than chatListerTTL: the installed-but-stopped
// fallback (citadel-cli#686) only runs on a running-engine MISS, and rerunning
// a full status.Collector.Collect() (docker stats, nvidia-smi, ...) on every
// miss would be needless cost for a set that only changes when an engine's
// compose file is materialized or removed -- something that happens on a
// deploy, not on a chat request.
const installedListerTTL = 30 * time.Second

// installedListerCollectTimeout bounds how long a cache-miss will wait on
// status.Collector.Collect() (docker stats + nvidia-smi across every
// service -- documented elsewhere in this codebase as multi-second on a busy
// node, and, unlike newLocalChatLister's DiscoverLocalEngines probe above,
// Collect() takes no context to bound it directly). A hung/slow collection
// must not hold an HTTP chat request open indefinitely; this caps the wait
// well under the gateway's own http.Server WriteTimeout (120s).
const installedListerCollectTimeout = 8 * time.Second

// newInstalledModelLister builds a gateway.ChatModelLister reporting
// installed-but-STOPPED serving engines and the model each would serve once
// swapped in (citadel-cli#686 -- the "one node answers the same question two
// ways" fix). configDir must be the same manifest/services directory
// hotswapConfigDir already gates the heartbeat's installed-engine advertising
// on ("" disables the fallback: no manifest to enumerate, mirroring
// hotswapConfigDir's own gate). Reuses status.Collector's existing hotswap
// logic (internal/status/hotswap.go, applyModelHotswap/collectInstalledEngines)
// rather than re-implementing "is this engine installed" here, so the
// gateway's fallback can never drift from what the heartbeat already
// advertises as swap-in candidates -- including the #683 serveability
// preflight (image/weights present, disk headroom), which this filters on via
// ServiceInfo.SwapBlocked so a candidate the swap would immediately fail for
// is never offered to EnsureResident in the first place.
func newInstalledModelLister(configDir string) gateway.ChatModelLister {
	if configDir == "" {
		return func() []gateway.ChatUpstream { return nil }
	}
	var (
		mu     sync.Mutex
		cached []gateway.ChatUpstream
		expiry time.Time
	)
	return func() []gateway.ChatUpstream {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Now().Before(expiry) {
			return cached
		}
		// Collect() has no context param, so the bound is enforced by racing
		// it against a timer rather than passing a deadline in. The goroutine
		// is never killed on timeout (Collect() has no cancellation hook
		// either), but resultCh is buffered so it cannot leak blocked on send
		// -- it simply finishes and is garbage collected once nobody is
		// listening.
		type collectResult struct {
			st  *status.NodeStatus
			err error
		}
		resultCh := make(chan collectResult, 1)
		go func() {
			collector := status.NewCollector(status.CollectorConfig{
				ConfigDir:    configDir,
				ModelHotswap: true,
			})
			st, err := collector.Collect()
			resultCh <- collectResult{st, err}
		}()

		var st *status.NodeStatus
		var err error
		select {
		case r := <-resultCh:
			st, err = r.st, r.err
		case <-time.After(installedListerCollectTimeout):
			// Timed out: serve whatever was last cached (even though its TTL
			// has expired -- stale-but-real beats nothing) rather than block
			// the request further. Deliberately do NOT overwrite cached/expiry
			// here, so the NEXT call retries a fresh Collect() rather than
			// pinning an empty/stale result for a full TTL window.
			if cached != nil {
				return cached
			}
			return []gateway.ChatUpstream{}
		}

		out := []gateway.ChatUpstream{}
		if err == nil && st != nil {
			for _, svc := range st.Services {
				// applyModelHotswap marks every installed-but-stopped engine
				// Resident=false (running engines it marks true; everything
				// else -- non-serving services -- leaves nil), so this is
				// exactly the "swap-in candidate" set the heartbeat advertises.
				if svc.Resident == nil || *svc.Resident {
					continue
				}
				if svc.SwapBlocked || svc.Port <= 0 || len(svc.Models) == 0 {
					continue
				}
				out = append(out, gateway.ChatUpstream{Engine: svc.Name, Port: svc.Port, Models: svc.Models})
			}
		}
		cached = out
		expiry = time.Now().Add(installedListerTTL)
		return out
	}
}
