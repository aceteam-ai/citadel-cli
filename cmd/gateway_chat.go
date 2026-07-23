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
