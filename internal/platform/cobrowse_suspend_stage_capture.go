package platform

import (
	"context"
	"errors"
	"os"
	"reflect"
	"time"
)

// withStagedThrowawayCapture composes capture and durable intent publication
// without releasing action or registry exclusion between them. The trusted
// caller supplies its manager-incarnation fence and the continuation that will
// perform verified teardown/completion. Neither ordinary teardown nor a legacy
// registry writer may be called from that continuation. Saved-profile handling
// is deliberately excluded; no public action or manager transaction calls this
// boundary yet. An error may leave a referenced intent and is not rollback.
func (s *cobrowseSession) withStagedThrowawayCapture(ctx context.Context, store *suspendIntentStore,
	live sessionRecord, now time.Time, ownerInitiated bool, source suspendPageSource,
	managerFence func() error, use func(sessionRecord) error) error {
	refuse := func() error { return errors.New("browser capture intent transaction incomplete") }
	if s == nil || store == nil || store.root == nil || store.payloads == nil ||
		ctx == nil || ctx.Err() != nil || managerFence == nil || use == nil || now.IsZero() ||
		live.State != RecordLive || live.Suspension != nil || live.Persistent || live.ProfileName != "" {
		return refuse()
	}
	if _, finite := ctx.Deadline(); !finite {
		return refuse()
	}
	return store.withLifecycleExclusive(ctx, func(ctx context.Context) error {
		// The registry gate precedes the session/action locks. A stale expected
		// record must refuse before any page-provider or continuation effect.
		store.mu.Lock()
		current, err := store.read(live.ID)
		store.mu.Unlock()
		if err != nil || !reflect.DeepEqual(current, live) || managerFence() != nil || ctx.Err() != nil {
			return refuse()
		}
		// Pin the captured in-memory incarnation as well as the registry's
		// value identity. The external manager fence is checked around it:
		// action exclusion alone does not stop Stop from removing its slot.
		s.mu.Lock()
		proc := s.proc
		s.mu.Unlock()
		fence := func() error {
			if ctx.Err() != nil || managerFence() != nil {
				return refuse()
			}
			s.mu.Lock()
			valid := proc != nil && proc.exited != nil && s.proc == proc && s.id == live.ID && s.generation == live.Generation &&
				s.ownerUserID == live.OwnerUserID && s.profile == live.Profile && !s.persistent &&
				s.encProfile == nil && s.profileName == "" && s.tracking == live.Tracking && s.explicitHandoff == live.Handoff &&
				checkSuspendState(string(s.state), s.humanDrivingLocked(), ownerInitiated) == nil &&
				proc.browserID == live.BrowserID && proc.debugPort == live.DebugPort &&
				proc.browserPID == live.BrowserPID && proc.xvfbPID == live.XvfbPID &&
				proc.browserScope == live.BrowserScope && proc.xvfbScope == live.XvfbScope && proc.scopeUser == live.ScopeUser &&
				proc.containerID == live.ContainerID && proc.containerStart == live.ContainerStart && proc.containerCtrl == live.ControlPort
			regDir := s.regDir
			if valid {
				select {
				case <-proc.exited:
					valid = false
				default:
				}
			}
			s.mu.Unlock()
			// Directory identity, not path spelling, binds aliases to the
			// same registry exclusion. No profile or page payload is read.
			named, e1 := os.Stat(regDir)
			anchored, e2 := store.root.Stat(".")
			if !valid || e1 != nil || e2 != nil || !os.SameFile(named, anchored) ||
				managerFence() != nil || ctx.Err() != nil {
				return refuse()
			}
			return nil
		}
		if fence() != nil {
			return refuse()
		}
		return s.withSuspendPageCapture(ctx, live.OwnerUserID, ownerInitiated, source, func(manifest suspendManifest) error {
			if fence() != nil {
				return refuse()
			}
			capture, err := prepareSuspendCapture(manifest, false, "", nil)
			if err != nil {
				return refuse()
			}
			defer capture.clear()
			intent, err := store.stage(ctx, live, live.OwnerUserID, capture, now, fence)
			if err != nil || fence() != nil {
				return refuse()
			}
			// The continuation may intentionally stop the captured process;
			// its own pinned proof governs completion, not this live fence.
			// Do not delete an uncertain intent on a continuation error.
			if use(intent) != nil || ctx.Err() != nil {
				return refuse()
			}
			return nil
		})
	})
}
