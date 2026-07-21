// Package mesh implements Phase 2 of citadel-chat-and-mesh (issue #576):
// discovering the models served by OTHER citadel nodes on the embedded-tsnet
// mesh and routing OpenAI chat-completion requests to a chosen remote node's
// engine.
//
// Discovery route: each citadel node already publishes its served models on the
// heartbeat and exposes the same payload at GET /status on its mesh VPN listener
// (see internal/status). Node->node traffic on the mesh is DIRECT (the Railway
// SOCKS relay caveat only affects backend->node), so a node can enumerate its
// mesh peers and probe each peer's /status directly. This package aggregates
// those payloads into a fabric-wide model -> (node, engine, port) view.
//
// This layer is deliberately standalone: it does NOT import internal/network (or
// any heavy status deps). Callers inject a Dialer and a PeerLister, which makes
// the aggregation logic pure and unit-testable without a live mesh. cmd wires
// network.Dial and network.GetGlobalPeers into it; #575's chat REPL can reuse
// the same Client/Inventory to add a remote/peer selection mode.
package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultStatusPort is the port a citadel node serves its /status endpoint on
// over the mesh. It mirrors services.GatewayPort (8080): `citadel work --gateway`
// (and the provisioned gateway) bind the status server there and add a tsnet VPN
// listener so peers can reach it. Kept as a local const so this discovery layer
// stays free of the services/network imports and can be unit-tested in isolation.
//
// NOTE: a plain `citadel work` (no --gateway, default --status-port 0) does NOT
// serve /status on the mesh. Discovery therefore only sees peers running the
// gateway/status server; unreachable peers are skipped gracefully. The port is
// configurable (Options.Port) for nodes that bind it elsewhere.
const DefaultStatusPort = 8080

// defaultProbeTimeout bounds a single peer /status probe. Set a touch above the
// node's own 2s local model-discovery deadline so a healthy-but-slightly-slow
// peer is not dropped, while an unreachable peer is skipped quickly.
const defaultProbeTimeout = 4 * time.Second

// defaultConcurrency caps the number of in-flight peer probes so a large fabric
// does not open a probe socket per peer all at once.
const defaultConcurrency = 16

// maxStatusBody caps the /status response we will read from a peer (defensive:
// a peer is same-org but a bounded read avoids a hostile/buggy peer streaming an
// unbounded body).
const maxStatusBody = 4 << 20 // 4 MiB

// Peer is the minimal view of a mesh peer the discovery layer needs. cmd adapts
// network.PeerInfo into this so internal/mesh never imports internal/network.
type Peer struct {
	Hostname string
	IP       string
	Online   bool
}

// Dialer dials addr over the mesh. cmd passes network.Dial (tsnet userspace
// dialer); tests pass a dialer that targets a local httptest server, exercising
// the HTTP path without a real mesh.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// PeerLister enumerates mesh peers. cmd wraps network.GetGlobalPeers.
type PeerLister func(ctx context.Context) ([]Peer, error)

// ServedModel is a single model served by a single engine on a single node.
// The same model served on multiple nodes yields multiple ServedModel entries
// so --node/--model can disambiguate which one to route to.
type ServedModel struct {
	Model    string `json:"model"`
	NodeName string `json:"node_name,omitempty"`
	Hostname string `json:"hostname"`
	IP       string `json:"ip"`
	Engine   string `json:"engine,omitempty"` // vllm | ollama | llamacpp | bonsai | ""
	Port     int    `json:"port"`
}

// NodeInventory is the discovery outcome for one peer: either its served models
// or the reason it could not be probed (unreachable, non-200, decode error).
type NodeInventory struct {
	Hostname  string        `json:"hostname"`
	IP        string        `json:"ip"`
	NodeName  string        `json:"node_name,omitempty"`
	Reachable bool          `json:"reachable"`
	Error     string        `json:"error,omitempty"`
	Models    []ServedModel `json:"models,omitempty"`
}

// Inventory is the fabric-wide aggregation: per-node results plus a flattened,
// sorted list of every served model across all reachable peers.
type Inventory struct {
	Nodes  []NodeInventory `json:"nodes"`
	Models []ServedModel   `json:"models"`
}

// Options tunes discovery.
type Options struct {
	// Port is the peer /status port to probe. Zero means DefaultStatusPort.
	Port int
	// ProbeTimeout bounds a single peer probe. Zero means defaultProbeTimeout.
	ProbeTimeout time.Duration
	// Concurrency caps in-flight probes. Zero means defaultConcurrency.
	Concurrency int
	// IncludeOffline probes peers the mesh reports as offline too (default:
	// online-only, matching "OTHER online nodes").
	IncludeOffline bool
	// SelfIP, when set, excludes this node from discovery (the issue is about
	// OTHER nodes). cmd passes network.GetGlobalIPv4().
	SelfIP string
}

// nodeStatus is the minimal subset of the /status payload (internal/status.
// NodeStatus) the discovery layer decodes. Mirroring the JSON here — rather than
// importing internal/status and its heavy transitive deps (desktop, resmon,
// terminal, ...) — keeps this layer standalone and cheap to test.
type nodeStatus struct {
	Node struct {
		Name        string `json:"name"`
		NetworkIP   string `json:"network_ip"`
		TailscaleIP string `json:"tailscale_ip"`
	} `json:"node"`
	Services []struct {
		Name   string   `json:"name"`
		Type   string   `json:"type"`
		Status string   `json:"status"`
		Port   int      `json:"port"`
		Models []string `json:"models"`
	} `json:"services"`
}

// statusFetchFunc fetches and decodes a peer's /status payload. Abstracted so
// discover() can be unit-tested with a mock fetcher (no HTTP, no mesh).
type statusFetchFunc func(ctx context.Context, ip string) (*nodeStatus, error)

// EngineTypeFromName maps a service name to a serving-engine type
// ("vllm"/"ollama"/"llamacpp"/"bonsai"), or "" when not a known engine. Order
// matters: "ollama" contains "llama" so it is checked before the llama.cpp
// patterns. This replicates internal/status.EngineTypeFromName locally to keep
// this layer standalone (the value is informational for the model->engine view;
// all four engines expose the same /v1/chat/completions routing path).
func EngineTypeFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "vllm"):
		return "vllm"
	case strings.Contains(n, "ollama"):
		return "ollama"
	case strings.Contains(n, "bonsai"):
		return "bonsai"
	case strings.Contains(n, "llamacpp"), strings.Contains(n, "llama.cpp"), strings.Contains(n, "llama-cpp"):
		return "llamacpp"
	}
	return ""
}

// modelsFromStatus extracts routable served models from a peer's /status
// payload. A service is included only when it is running, exposes a host port,
// and reports at least one model — the exact conditions under which chat can be
// routed to it. Each model becomes its own ServedModel entry.
func modelsFromStatus(p Peer, st *nodeStatus) []ServedModel {
	if st == nil {
		return nil
	}
	var out []ServedModel
	for _, svc := range st.Services {
		if !strings.EqualFold(svc.Status, "running") {
			continue
		}
		if svc.Port <= 0 || len(svc.Models) == 0 {
			// No port -> cannot route; no models -> nothing to serve.
			continue
		}
		engine := EngineTypeFromName(svc.Name)
		for _, m := range svc.Models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			out = append(out, ServedModel{
				Model:    m,
				NodeName: st.Node.Name,
				Hostname: p.Hostname,
				IP:       p.IP,
				Engine:   engine,
				Port:     svc.Port,
			})
		}
	}
	return out
}

// Discover enumerates mesh peers via lister, probes each reachable peer's
// /status over the mesh (dialing via dialer), and aggregates the served models
// into a fabric-wide Inventory. Unreachable peers are skipped and recorded with
// their error; a peer probe never fails the whole call. Returns an error only if
// enumerating peers itself fails.
func Discover(ctx context.Context, lister PeerLister, dialer Dialer, opts Options) (*Inventory, error) {
	port := opts.Port
	if port <= 0 {
		port = DefaultStatusPort
	}
	return discover(ctx, lister, httpFetcher(dialer, port), opts)
}

// discover is the testable core: it takes a statusFetchFunc so tests can inject
// a fake fetcher and exercise filtering/aggregation/error-handling with no HTTP.
func discover(ctx context.Context, lister PeerLister, fetch statusFetchFunc, opts Options) (*Inventory, error) {
	peers, err := lister(ctx)
	if err != nil {
		return nil, fmt.Errorf("list mesh peers: %w", err)
	}

	// Filter: routable peers only (has IP), online unless overridden, exclude self.
	var targets []Peer
	for _, p := range peers {
		if p.IP == "" {
			continue
		}
		if !opts.IncludeOffline && !p.Online {
			continue
		}
		if opts.SelfIP != "" && p.IP == opts.SelfIP {
			continue
		}
		targets = append(targets, p)
	}

	conc := opts.Concurrency
	if conc <= 0 {
		conc = defaultConcurrency
	}
	probeTimeout := opts.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultProbeTimeout
	}

	// Pre-sized result slice: each goroutine writes a distinct index, so no
	// mutex is needed for the writes.
	nodes := make([]NodeInventory, len(targets))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for i, p := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p Peer) {
			defer wg.Done()
			defer func() { <-sem }()

			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()

			inv := NodeInventory{Hostname: p.Hostname, IP: p.IP}
			st, ferr := fetch(pctx, p.IP)
			if ferr != nil {
				inv.Reachable = false
				inv.Error = ferr.Error()
			} else {
				inv.Reachable = true
				inv.NodeName = st.Node.Name
				inv.Models = modelsFromStatus(p, st)
			}
			nodes[i] = inv
		}(i, p)
	}
	wg.Wait()

	// Deterministic ordering for stable CLI/JSON output.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Hostname != nodes[j].Hostname {
			return nodes[i].Hostname < nodes[j].Hostname
		}
		return nodes[i].IP < nodes[j].IP
	})

	inv := &Inventory{Nodes: nodes, Models: []ServedModel{}}
	for _, n := range nodes {
		inv.Models = append(inv.Models, n.Models...)
	}
	sortModels(inv.Models)
	return inv, nil
}

// sortModels orders models by (model, node, engine, port) for stable output.
func sortModels(models []ServedModel) {
	sort.Slice(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Hostname != b.Hostname {
			return a.Hostname < b.Hostname
		}
		if a.Engine != b.Engine {
			return a.Engine < b.Engine
		}
		return a.Port < b.Port
	})
}

// httpFetcher builds a statusFetchFunc that GETs http://<ip>:<port>/status over
// the mesh using the injected dialer. Keep-alives are disabled: each peer is
// probed once, so pooling connections to many one-shot peers is pointless and
// risks holding mesh sockets open.
func httpFetcher(dialer Dialer, port int) statusFetchFunc {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           func(ctx context.Context, netw, addr string) (net.Conn, error) { return dialer(ctx, netw, addr) },
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: defaultProbeTimeout,
		},
	}
	return func(ctx context.Context, ip string) (*nodeStatus, error) {
		url := fmt.Sprintf("http://%s/status", net.JoinHostPort(ip, strconv.Itoa(port)))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status endpoint returned %d", resp.StatusCode)
		}
		var st nodeStatus
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatusBody)).Decode(&st); err != nil {
			return nil, fmt.Errorf("decode /status: %w", err)
		}
		return &st, nil
	}
}

// FindModel selects the served model matching the given node (hostname or IP,
// optional) and model name (optional). It returns:
//   - the single match, or
//   - an error listing candidates when the selection is ambiguous, or
//   - an error when nothing matches.
//
// Matching is case-insensitive; model matches on exact id first, then a unique
// substring. node matches hostname or IP (exact, case-insensitive).
func (inv *Inventory) FindModel(node, model string) (ServedModel, error) {
	node = strings.TrimSpace(node)
	model = strings.TrimSpace(model)

	var pool []ServedModel
	for _, m := range inv.Models {
		if node != "" && !strings.EqualFold(m.Hostname, node) && !strings.EqualFold(m.IP, node) {
			continue
		}
		pool = append(pool, m)
	}
	if node != "" && len(pool) == 0 {
		return ServedModel{}, fmt.Errorf("no served models found on node %q (is it online and serving on the mesh?)", node)
	}
	if node == "" {
		pool = inv.Models
	}

	if model != "" {
		// Exact model id match first.
		var matched []ServedModel
		for _, m := range pool {
			if strings.EqualFold(m.Model, model) {
				matched = append(matched, m)
			}
		}
		if len(matched) == 0 {
			// Fall back to unique substring match.
			for _, m := range pool {
				if strings.Contains(strings.ToLower(m.Model), strings.ToLower(model)) {
					matched = append(matched, m)
				}
			}
		}
		pool = matched
	}

	switch len(pool) {
	case 0:
		return ServedModel{}, fmt.Errorf("no served model matches node=%q model=%q", node, model)
	case 1:
		return pool[0], nil
	default:
		return ServedModel{}, fmt.Errorf("ambiguous selection (%d matches); narrow with --node/--model:\n%s",
			len(pool), formatCandidates(pool))
	}
}

func formatCandidates(models []ServedModel) string {
	var b strings.Builder
	for _, m := range models {
		fmt.Fprintf(&b, "  - %s on %s (%s:%d, engine=%s)\n", m.Model, m.Hostname, m.IP, m.Port, engineOrDash(m.Engine))
	}
	return b.String()
}

func engineOrDash(e string) string {
	if e == "" {
		return "-"
	}
	return e
}
