// Package clockstate reports cached daemon observations, never clock authority.
// Constructors and Snapshot do not execute commands. The caller owns Run's
// goroutine and cancellation; only Run probes, at most once per 60 seconds.
package clockstate

import (
	"context"
	"sync/atomic"
	"time"
)

type State string

const (
	Synced   State = "synced"
	Unsynced State = "unsynced"
	Unknown  State = "unknown"
	CacheTTL       = 60 * time.Second
)

// Provider is a cheap cached observation; implementations must never wait for
// a command. It is not an offset estimate or proof of synchronization.
type Provider interface{ Snapshot() State }

func Normalize(s State) State {
	if s == Synced || s == Unsynced {
		return s
	}
	return Unknown
}

func Boolean(s State) *bool {
	if s = Normalize(s); s == Unknown {
		return nil
	}
	b := s == Synced
	return &b
}

type observation struct {
	state State
	at    time.Time
}

type Monitor struct {
	value   atomic.Pointer[observation]
	running atomic.Bool
	now     func() time.Time
	probe   func(context.Context) State
	// Injected only by package fixtures. Production uses a 60-second ticker.
	ticks <-chan time.Time
}

func New() *Monitor { return &Monitor{now: time.Now, probe: probeClock} }

func (m *Monitor) Snapshot() State {
	if m == nil {
		return Unknown
	}
	o := m.value.Load()
	if o == nil {
		return Unknown
	}
	age := m.now().Sub(o.at) // time.Now retains monotonic age across wall jumps.
	if age < 0 || age >= CacheTTL {
		return Unknown
	}
	return Normalize(o.state)
}

func (m *Monitor) refresh(ctx context.Context) {
	state := Normalize(m.probe(ctx))
	if ctx.Err() == nil {
		m.value.Store(&observation{state: state, at: m.now()})
	}
}

// Run must be explicitly called by the owning lifecycle, not by a getter or
// constructor. Cancellation ends probing; OS uninterruptible reap is not an
// absolute wall-clock guarantee. A second Run cannot create an overlapping loop.
func (m *Monitor) Run(ctx context.Context) {
	if !m.running.CompareAndSwap(false, true) {
		return
	}
	defer m.running.Store(false)
	if ctx.Err() != nil {
		return
	}
	ticks := m.ticks
	if ticks == nil {
		ticker := time.NewTicker(CacheTTL)
		defer ticker.Stop()
		ticks = ticker.C
	}
	m.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				return
			}
			m.refresh(ctx)
		}
	}
}
