// Package meetingcapacity prepares the local custody state for #1505 item 1.
// It performs no I/O and is not installed in a production JobSource. Transport
// adapters must first authenticate the node incarnation and prove homogeneous
// claim units, one-shot delivery and resource reconciliation. None of these
// values is a credential, wire protocol or substitute for that proof.
package meetingcapacity

import (
	"errors"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var (
	ErrBinding     = errors.New("meeting capacity binding does not match")
	ErrUnsafeUnit  = errors.New("meeting claim unit is unclassified, mixed or unbounded")
	ErrBusy        = errors.New("meeting capacity is occupied")
	ErrUncertain   = errors.New("meeting custody needs reconciliation")
	ErrReservation = errors.New("meeting reservation does not match")
	ErrObservation = errors.New("meeting read observation is inconsistent")
	ErrHandlerLive = errors.New("actual meeting handler has not exited")
	ErrJob         = errors.New("meeting job identity does not match")
	ErrSerial      = errors.New("meeting reservation serial exhausted")
)

// Binding comes from an authenticated adapter, never a job payload. IDs are
// opaque here and must not be joined into paths or treated as authorization.
type Binding struct {
	Organization string
	Node         string
	Incarnation  string
}

func validID(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && strings.TrimSpace(s) == s &&
		strings.IndexFunc(s, unicode.IsControl) < 0
}

func (b Binding) valid() bool {
	return validID(b.Organization) && validID(b.Node) && validID(b.Incarnation)
}

type Class uint8

const (
	Unclassified Class = iota
	UnrelatedOnly
	MeetingOnly
	Mixed
)

type Path uint8

const (
	APIRead Path = iota + 1
	APIWake
	RedisRead
	RedisReclaim
	RedisWake
	WSInitial
	WSReconnect
	WSSubscription
)

// ClaimUnit is an adapter's already-verified source classification. Declaring
// this struct does not establish that classification on a real mixed queue.
type ClaimUnit struct {
	Binding       Binding
	Class         Class
	Path          Path
	Queue         string
	MaxDeliveries int
}

type Phase uint8

const (
	Quarantined Phase = iota
	Free
	Reserved
	Held
)

// Reservation is local, non-serializable custody. Every intake path shares the
// same Capacity pointer. A read, wake or reconnect cannot mint another permit.
type Reservation struct {
	owner  *Capacity
	serial uint64
}

type pending struct {
	serial  uint64
	path    Path
	queue   string
	job     string
	exited  bool
	settled bool
}

// Capacity must not be copied. Its zero value cannot admit meeting reads.
type Capacity struct {
	mu       sync.Mutex
	binding  Binding
	phase    Phase
	startup  bool
	poisoned bool
	serial   uint64
	current  *pending
}

type Snapshot struct {
	Phase         Phase
	Serial        uint64
	Path          Path
	Queue         string
	Job           string
	HandlerExited bool
	ReadSettled   bool
	Startup       bool
	Poisoned      bool
}

func New(binding Binding) (*Capacity, error) {
	if !binding.valid() {
		return nil, ErrBinding
	}
	return &Capacity{binding: binding, phase: Quarantined, startup: true}, nil
}

// StartupProof is supplied only after the paired source/resource reconciler
// establishes each fact for this exact incarnation. Restart or elapsed time
// supplies none of it. This kernel has no native resource or server adapter.
type StartupProof struct {
	Binding               Binding
	NoOutstandingIntake   bool
	PriorClaimsReconciled bool
	NoOwnedResources      bool
}

func (c *Capacity) ReconcileStartup(proof StartupProof) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.binding.valid() || proof.Binding != c.binding {
		return ErrBinding
	}
	if !c.startup || c.current != nil || c.poisoned ||
		!proof.NoOutstandingIntake || !proof.PriorClaimsReconciled || !proof.NoOwnedResources {
		return ErrUncertain
	}
	c.startup = false
	c.phase = Free
	return nil
}

// BeforeClaim runs before ownership, pending entries or delivery counts can
// change. A successful unrelated-only decision has a zero Reservation and does
// not consume meeting capacity. A mixed unit is never filtered after claiming.
func (c *Capacity) BeforeClaim(unit ClaimUnit) (Reservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.binding.valid() || unit.Binding != c.binding {
		return Reservation{}, ErrBinding
	}
	if unit.Class == UnrelatedOnly {
		return Reservation{}, nil
	}
	if unit.Class != MeetingOnly || !validID(unit.Queue) || unit.MaxDeliveries != 1 || unit.Path < APIRead || unit.Path > WSSubscription {
		return Reservation{}, ErrUnsafeUnit
	}
	if c.phase == Quarantined {
		return Reservation{}, ErrUncertain
	}
	if c.phase != Free || c.current != nil {
		return Reservation{}, ErrBusy
	}
	if c.serial == ^uint64(0) {
		c.phase = Quarantined
		c.poisoned = true
		return Reservation{}, ErrSerial
	}
	c.serial++
	c.current = &pending{serial: c.serial, path: unit.Path, queue: unit.Queue}
	c.phase = Reserved
	return Reservation{owner: c, serial: c.serial}, nil
}

type ReadOutcome uint8

const (
	ReadUncertain ReadOutcome = iota
	ReadEmpty
	ReadClaimed
)

// ReadObservation describes an authenticated, reconciled source outcome, not
// an HTTP status, a cancelled context, an expired lease or a local timeout.
// CannotClaimAgain requires one-shot settlement of the outstanding operation.
type ReadObservation struct {
	Binding          Binding
	Outcome          ReadOutcome
	Job              string
	CannotClaimAgain bool
}

func (c *Capacity) owns(r Reservation) bool {
	return r.owner == c && r.serial != 0 && c.current != nil && r.serial == c.current.serial
}

func (c *Capacity) ObserveRead(r Reservation, observation ReadObservation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.owns(r) {
		// A claimed delivery after a definite empty/released read contradicts
		// the one-shot source proof. It cannot free or replace current custody.
		if r.owner == c && r.serial != 0 && r.serial <= c.serial && observation.Outcome == ReadClaimed {
			c.poisoned = true
			c.phase = Quarantined
		}
		return ErrReservation
	}
	if observation.Binding != c.binding {
		c.phase = Quarantined
		return ErrBinding
	}
	if c.poisoned {
		return ErrUncertain
	}
	if c.current.job != "" {
		if observation.Outcome == ReadClaimed && observation.Job == c.current.job && observation.CannotClaimAgain {
			c.current.settled = true
			if !c.current.exited {
				c.phase = Held
			}
			return nil // duplicate receipt confers no new read or execution
		}
		if observation.Outcome == ReadUncertain {
			return ErrUncertain // keep the stronger, existing delivery evidence
		}
		c.poisoned = true
		c.phase = Quarantined
		return ErrObservation
	}
	switch observation.Outcome {
	case ReadEmpty:
		if observation.CannotClaimAgain && observation.Job == "" {
			c.current = nil
			c.phase = Free
			return nil
		}
	case ReadClaimed:
		if validID(observation.Job) {
			c.current.job = observation.Job
			if observation.CannotClaimAgain {
				c.current.settled = true
				c.phase = Held
				return nil
			}
		} else {
			// A claimed delivery with no usable identity cannot be erased by
			// a later empty response for just the original outstanding read.
			c.poisoned = true
		}
	case ReadUncertain:
		c.phase = Quarantined
		return ErrUncertain
	}
	c.phase = Quarantined
	return ErrObservation
}

func (c *Capacity) actualJob(r Reservation, job string) error {
	if !c.owns(r) {
		return ErrReservation
	}
	if c.current.job == "" || job != c.current.job {
		return ErrJob
	}
	return nil
}

// HandlerExited is called at the actual executor goroutine's exit, never at a
// deadline wrapper's return or a terminal publish/ACK/NACK. It alone frees nothing.
func (c *Capacity) HandlerExited(r Reservation, job string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.actualJob(r, job); err != nil {
		return err
	}
	c.current.exited = true
	return nil
}

// ConfirmTeardownAfterExit requires fresh confirmation that all owned browser
// and recorder resources are gone after the actual executor exited. Early
// cleanup, resource timeout and unknown teardown cannot become release proof.
func (c *Capacity) ConfirmTeardownAfterExit(r Reservation, job string, confirmed bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.actualJob(r, job); err != nil {
		return err
	}
	if !c.current.exited {
		return ErrHandlerLive
	}
	if !confirmed || !c.current.settled || c.poisoned {
		c.phase = Quarantined
		return ErrUncertain
	}
	c.current = nil
	c.phase = Free
	return nil
}

func (c *Capacity) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Snapshot{Phase: c.phase, Serial: c.serial, Startup: c.startup, Poisoned: c.poisoned}
	if c.current != nil {
		s.Path, s.Queue, s.Job, s.HandlerExited = c.current.path, c.current.queue, c.current.job, c.current.exited
		s.ReadSettled = c.current.settled
	}
	return s
}
