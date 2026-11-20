package platform

import (
	"context"
	"errors"
	"time"
)

const suspendJobProofLimit = 5 * time.Second

// One privately owned job handle, shared by legacy stop and the suspend-only
// positive proof. Ordinary stop retains its old cached best-effort result.
// Its success is never upgraded into proof that the entire job was emptied.
type suspendJobStop struct {
	gate                  chan struct{}
	exited                <-chan struct{}
	legacy, terminate     func() error
	active                func() (uint32, error)
	close                 func() error
	closed, emptyVerified bool
	legacyErr             error
}

func newSuspendJobStop(exited <-chan struct{}, legacy, terminate func() error,
	active func() (uint32, error), closeJob func() error) *suspendJobStop {
	return &suspendJobStop{gate: make(chan struct{}, 1), exited: exited, legacy: legacy,
		terminate: terminate, active: active, close: closeJob}
}

func (s *suspendJobStop) ordinaryStop() error {
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	if !s.closed {
		s.legacyErr = s.legacy()
		s.closed = true
	}
	return s.legacyErr
}

// stopForSuspend never substitutes root exit, a closed handle or a successful
// legacy stop for a positive zero-process query on this exact owned job. A
// timed-out/failed proof leaves legacy cleanup available and may be retried.
// No manager/action invokes this method yet. Caller must pin the proc, hold
// lifecycle exclusion and separately handle profile/registry completion.
func (s *suspendJobStop) stopForSuspend(ctx context.Context) error {
	fail := func() error { return errors.New("could not verify complete browser job teardown") }
	if ctx == nil || s.gate == nil || s.exited == nil || s.legacy == nil || s.terminate == nil || s.active == nil || s.close == nil {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite || ctx.Err() != nil {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, suspendJobProofLimit)
	defer cancel()
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return fail()
	}
	defer func() { <-s.gate }()
	if ctx.Err() != nil {
		return fail()
	}
	if s.closed {
		if s.emptyVerified {
			return nil
		}
		return fail()
	}
	if err := s.terminate(); err != nil || ctx.Err() != nil {
		return fail()
	}
	for {
		n, err := s.active()
		if err != nil || ctx.Err() != nil {
			return fail()
		}
		if n == 0 {
			break
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return fail()
		}
	}
	select {
	case <-s.exited:
	case <-ctx.Done():
		return fail()
	}
	if ctx.Err() != nil {
		return fail()
	}
	if err := s.close(); err != nil {
		return fail()
	}
	// The facts are retained even if the acknowledgement deadline expires
	// immediately after close. A fresh-budget retry can reconcile this proof.
	s.closed, s.emptyVerified = true, true
	if ctx.Err() != nil {
		return fail()
	}
	return nil
}
