package platform

import (
	"context"
	"encoding/hex"
	"errors"
	"path"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

// These values must come from verified per-launch observations, never a scope
// name lookup during suspend or an untrusted registry record. No runner creates
// this adapter yet: launch-time invocation pinning and recovery remain needed.
// Observing two scopes does not prove launch containment. A future runner must
// separately exclude escaped/app-scope helpers before using this as the proc's
// complete-resource adapter; emptiness of the original scopes alone cannot.
type suspendScopeResource struct {
	scope  procscope.ScopeIdentity
	holder procscope.ProcessIdentity
}

type suspendScopeStop struct {
	resources [2]suspendScopeResource // browser first, then its dedicated display
	reapers   [2]<-chan struct{}
	exact     func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error
	gate      chan struct{}
}

func newSuspendScopeStop(sessionID string, resources [2]suspendScopeResource, reapers [2]<-chan struct{},
	exact func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error) (*suspendScopeStop, error) {
	fail := func() (*suspendScopeStop, error) { return nil, errors.New("browser scope proof unavailable") }
	if !sessionIDPattern.MatchString(sessionID) || exact == nil {
		return fail()
	}
	for i, resource := range resources {
		role := []string{"browser", "xvfb"}[i]
		scope, holder := resource.scope, resource.holder
		invocation, err := hex.DecodeString(scope.InvocationID)
		if reapers[i] == nil || !anyScopeName.MatchString(scope.Name) ||
			!strings.HasPrefix(scope.Name, "citadel-cobrowse-"+sessionID+"-"+role+"-") ||
			err != nil || len(invocation) != 16 || strings.ToLower(scope.InvocationID) != scope.InvocationID ||
			scope.InvocationID == strings.Repeat("0", 32) || scope.UID == 0 ||
			holder.UID != scope.UID || holder.PID <= 0 || holder.StartTicks == 0 || holder.BootID == "" ||
			!strings.HasPrefix(scope.ControlGroup, "/") || path.Clean(scope.ControlGroup) != scope.ControlGroup ||
			strings.ContainsAny(scope.ControlGroup, "\x00\r\n") || path.Base(scope.ControlGroup) != scope.Name {
			return fail()
		}
	}
	browser, display := resources[0], resources[1]
	if browser.scope.UID != display.scope.UID || browser.holder.BootID != display.holder.BootID ||
		browser.holder.PID == display.holder.PID || browser.scope.InvocationID == display.scope.InvocationID ||
		strings.HasPrefix(browser.scope.ControlGroup, display.scope.ControlGroup+"/") ||
		strings.HasPrefix(display.scope.ControlGroup, browser.scope.ControlGroup+"/") {
		return fail()
	}
	return &suspendScopeStop{resources: resources, reapers: reapers, exact: exact, gate: make(chan struct{}, 1)}, nil
}

// stopForSuspend requires exact invocation emptiness and old process-birth
// disappearance from the trusted adapter, then both original reapers. An
// already-reaped root, legacy stop success, or unit-name absence is not proof.
// The trusted operations and fence must honor the finite shared context and
// must not re-enter this gate. Failure remains retryable; no proof is cached.
func (s *suspendScopeStop) stopForSuspend(ctx context.Context, fence func() error) error {
	fail := func() error { return errors.New("could not verify complete browser scope teardown") }
	if s == nil || s.gate == nil || s.exact == nil || fence == nil || ctx == nil || ctx.Err() != nil {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite {
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
	check := func() error {
		if ctx.Err() != nil || fence() != nil || ctx.Err() != nil {
			return fail()
		}
		return nil
	}
	if check() != nil {
		return fail()
	}
	for _, resource := range s.resources {
		if s.exact(ctx, resource.scope, resource.holder) != nil || check() != nil {
			return fail()
		}
	}
	for _, reaper := range s.reapers {
		select {
		case <-reaper:
		case <-ctx.Done():
			return fail()
		}
		if check() != nil {
			return fail()
		}
	}
	return nil
}
