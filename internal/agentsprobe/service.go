// internal/agentsprobe/service.go
//
// Service caches vendor-agent probe results for the S2 pull endpoint
// (aceteam #8993, node_agents_list). A probe is an exec (up to four vendor
// `--version`s, each of which some Node CLIs use to run an update check) plus a
// possible network call, so it is RARE: an async startup probe, an hourly timer,
// and an on-demand ?refresh=1, all serialized behind one singleflight lock. The
// read path (Get) never probes, so no heartbeat/status reader can ever stall on
// one and no read execs a binary.
package agentsprobe

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot is the cached result of one probe run, served by GET /agent/vendor-agents.
type Snapshot struct {
	// ProbedAt is when this snapshot's probe ran (zero before the first probe).
	ProbedAt time.Time `json:"probed_at"`
	// Stale is true before the first probe completes (the worker just booted):
	// the endpoint returns an empty agents list, never a fabricated one.
	Stale bool `json:"stale"`
	// Disabled is true under the CITADEL_AGENTS_PROBE=off kill switch.
	Disabled bool `json:"disabled,omitempty"`
	// Target attributes WHOSE environment was probed (nil before the first probe
	// or when the target was unresolvable).
	Target *TargetInfo `json:"target,omitempty"`
	// ResolveError is set when the target could not be resolved; agents then all
	// report AuthStateUnknown rather than a confident false "no".
	ResolveError string `json:"resolve_error,omitempty"`
	// Agents is the S1 VendorAgent contract, unchanged, so `citadel agents probe
	// --json` and this endpoint agree. Always non-nil (marshals as [], never null).
	Agents []VendorAgent `json:"agents"`
}

// TargetInfo is the attributable {user, uid, home, signal} block from the resolver.
type TargetInfo struct {
	User   string `json:"user"`
	UID    int    `json:"uid"`
	Home   string `json:"home"`
	Signal string `json:"signal"`
}

// ServiceConfig configures a Service. Resolve and Probe are injectable so the
// cadence/singleflight logic is unit-testable with fakes.
type ServiceConfig struct {
	// Disabled short-circuits everything (kill switch): no startup probe, no
	// timer, and Get/Refresh return a disabled snapshot.
	Disabled bool
	// Resolve resolves the target user for each probe (re-read each run so a
	// mid-lifetime identity change is picked up). Required unless Disabled.
	Resolve func() (Target, error)
	// Probe runs the detection. Defaults to the package Probe.
	Probe func(ctx context.Context, opts Options) []VendorAgent
	// ProcessUID is os.Getuid(), used for the privilege-drop decision.
	ProcessUID int
	// MinRefreshGap bounds how often an on-demand (?refresh=1) probe actually
	// runs. Defaults to 60s.
	MinRefreshGap time.Duration
	// Timeout bounds a whole probe run. Defaults to 30s.
	Timeout time.Duration
}

// Service holds the cached snapshot and the singleflight probe guard.
type Service struct {
	cfg  ServiceConfig
	snap atomic.Pointer[Snapshot]

	probeMu   sync.Mutex // singleflight: one probe at a time; also guards lastProbe
	lastProbe time.Time
}

// NewService builds a Service, seeding the snapshot so the endpoint has
// something honest to serve before the first probe (or under the kill switch).
func NewService(cfg ServiceConfig) *Service {
	if cfg.Probe == nil {
		cfg.Probe = Probe
	}
	if cfg.MinRefreshGap <= 0 {
		cfg.MinRefreshGap = 60 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	s := &Service{cfg: cfg}
	if cfg.Disabled {
		s.snap.Store(&Snapshot{Disabled: true, Agents: []VendorAgent{}})
	} else {
		s.snap.Store(&Snapshot{Stale: true, Agents: []VendorAgent{}})
	}
	return s
}

// IsDisabled reports whether the kill switch is set.
func (s *Service) IsDisabled() bool { return s.cfg.Disabled }

// Get returns the current snapshot WITHOUT probing -- the read path the endpoint
// default (and any future heartbeat reader) uses, so a read never execs a binary.
func (s *Service) Get() *Snapshot { return s.snap.Load() }

// Start launches the async startup probe and (when interval > 0) the periodic
// refresh timer. Non-blocking; a no-op when disabled. ctx is the worker root ctx
// so shutdown cancels an in-flight probe.
func (s *Service) Start(ctx context.Context, interval time.Duration) {
	if s.cfg.Disabled {
		return
	}
	go s.Refresh(ctx, true)
	if interval > 0 {
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					s.Refresh(ctx, true)
				}
			}
		}()
	}
}

// Refresh runs a probe and stores the resulting snapshot, then returns it. When
// force is false (the ?refresh=1 path) it SKIPS the probe if one completed within
// MinRefreshGap, returning the cached snapshot instead. probeMu is the
// singleflight guard: a concurrent caller blocks here and, on acquiring the lock,
// sees the just-completed probe within the gap and returns it rather than
// starting a second exec.
func (s *Service) Refresh(ctx context.Context, force bool) *Snapshot {
	if s.cfg.Disabled {
		return s.snap.Load()
	}
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if !force && !s.lastProbe.IsZero() && time.Since(s.lastProbe) < s.cfg.MinRefreshGap {
		return s.snap.Load()
	}
	snap := s.runProbe(ctx)
	s.lastProbe = time.Now()
	s.snap.Store(snap)
	return snap
}

// runProbe resolves the target and runs the probe (or an honest HomeUnknown
// probe when the target is unresolvable). The caller holds probeMu.
func (s *Service) runProbe(ctx context.Context) *Snapshot {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	snap := &Snapshot{ProbedAt: time.Now()}
	target, err := s.cfg.Resolve()
	if err != nil {
		// Unresolvable target: probe with HOME UNKNOWN so every installed vendor
		// reports AuthStateUnknown rather than a confident false "no" against the
		// worker's own /root (#1015). Record the error for attribution.
		snap.ResolveError = err.Error()
		snap.Agents = s.cfg.Probe(cctx, Options{HomeUnknown: true})
		return snap
	}
	snap.Target = &TargetInfo{
		User:   target.Username,
		UID:    target.UID,
		Home:   target.HomeDir,
		Signal: string(target.Signal),
	}
	snap.Agents = s.cfg.Probe(cctx, target.probeOptions(s.cfg.ProcessUID))
	return snap
}
