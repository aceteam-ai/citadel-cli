package declaredsession

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

func (h *holder) beginCleanup(stop bool) {
	if stop {
		h.stopRequested = true
		if h.stopReason == "" {
			h.stopReason = "operator_stop"
		}
	}
	if h.cleaning || h.quarantined {
		return
	}
	h.cleaning = true
	h.nextSpawn = time.Time{}
	h.controller, h.controlPending = "", true
	h.epoch++
	if h.terminal != nil {
		h.terminal.epoch.Store(h.epoch)
	}
	for _, viewer := range h.viewers {
		role := frame("role")
		role.ReadOnly = true
		h.send(viewer, role)
	}
	h.state.Phase = "stopping"
	h.writeState()
	child, terminal, readDone := h.child, h.terminal, h.readDone
	self := toScopeProcess(h.state.Holder)
	go func() {
		result := cleanupResult{noChild: child == nil}
		if terminal != nil {
			terminal.stop()
		}
		if child != nil {
			// The leader is still owned and unreaped. Preserve its birth/PID
			// anchor until group signalling AND scope inspection are complete.
			if child.SignalGroup(15) != nil {
				result.err = errChildIO
			} else {
				grace, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				leaderErr := h.await(grace, child.Exited(), 2*time.Second)
				drainErr := h.await(grace, readDone, 2*time.Second)
				cancel()
				if leaderErr != nil || drainErr != nil {
					// A leader's exit does not prove its group stopped. Retain its
					// WNOWAIT anchor and escalate for TERM-resistant descendants
					// which still hold the slave/output, even if leader.Exited closed.
					if child.SignalGroup(9) != nil {
						result.err = errChildIO
					} else {
						killed, done := context.WithTimeout(context.Background(), 2*time.Second)
						leaderErr = h.await(killed, child.Exited(), 2*time.Second)
						drainErr = h.await(killed, readDone, 2*time.Second)
						done()
						if leaderErr != nil || drainErr != nil {
							result.err = errChildIO
						}
					}
				}
			}
			if result.err == nil {
				result.err = h.scope.CheckMembers(context.Background(), []procscope.ProcessIdentity{self, toScopeProcess(child.Identity())})
			}
			if result.err == nil {
				result.exit, result.err = child.Reap()
				if result.err == nil {
					result.err = child.Close()
				}
			}
		}
		if result.err == nil {
			result.err = h.scope.CheckMembers(context.Background(), []procscope.ProcessIdentity{self})
		}
		// The actor keeps processing output while this routine waits for drain;
		// it never waits for a reader which is blocked on a full output queue.
		h.cleaned <- result
	}()
}

func (h *holder) finishCleanup(result cleanupResult) bool {
	h.cleaning = false
	if result.err != nil {
		h.quarantined = true
		h.state.Phase = "cleanup_unconfirmed"
		h.writeState()
		h.event("stop", "cleanup_unconfirmed", "", "")
		h.finishStopReplies(ErrUnavailable)
		// Keep this incarnation, socket, scope and owner guard alive. It must
		// not respawn, release custody or signal a potentially reaped PID.
		return false
	}
	h.child, h.terminal = nil, nil
	if h.readCancel != nil {
		h.readCancel()
	}
	// readDone is closed only after the final output/error was enqueued. Drain
	// the bounded remaining queue before publishing exit or any completion.
	for len(h.parts) > 0 {
		h.output(<-h.parts)
	}
	if !result.noChild && h.state.ChildGeneration != 0 && !h.readEOF {
		h.storageFailed = true
	}
	h.state.Child = ProcessIdentity{}
	exitReason := "child_exit"
	if result.exit.Code != 0 || result.exit.Signal != 0 {
		exitReason = "child_failure"
	}
	if !result.noChild {
		result.exit.ChildGeneration = h.state.ChildGeneration
		h.state.LastExit = &result.exit
		h.event("exit", exitReason, "", "")
	}
	if h.stopRequested || h.storageFailed {
		h.event("stop", h.stopReason, "", "")
		h.state.Phase = "stopped"
		if h.storageFailed {
			h.state.Phase = "error"
		}
		h.writeState() // after the last synchronous console/event fsync
		h.finishStopReplies(h.finalError())
		return true
	}
	clean := result.exit.Code == 0 && result.exit.Signal == 0
	if clean && h.declaration.Restart.Policy == "on-failure" {
		// Marker first, state second; YAML is never rewritten. The marker is a
		// per-name execution veto even if config changed while the child ran.
		marker := StopMarker{Version: 1, Name: h.state.Name, Incarnation: h.state.Incarnation, DeclarationRevision: h.state.DeclarationRevision, StoppedAt: time.Now().UTC()}
		if h.session.WriteStopMarker(marker) != nil {
			h.storageFailed = true
			h.state.Phase = "error"
			h.writeState()
			return true
		}
		h.state.Phase = "stopped_clean"
		if snapshot, err := h.store.LoadConfig(); err != nil {
			h.state.Phase = "config_conflict"
		} else if current, exists := snapshot.Lookup(h.state.Name); !exists || current.Revision != h.state.DeclarationRevision {
			h.state.Phase = "config_conflict"
		}
		h.writeState()
		return true
	}
	if h.declaration.Restart.Policy == "never" {
		h.state.Phase = "stopped"
		h.writeState()
		return true
	}
	// Failed or always-policy exits retain the same holder/viewers. Configuration
	// is an immutable snapshot; Reload reports drift but never applies it.
	if time.Since(h.spawnAt) >= time.Duration(h.declaration.Restart.ResetAfter)*time.Second {
		h.backoff = 0
		h.rapidExits = 0
	}
	minimum := time.Duration(h.declaration.Restart.MinSeconds) * time.Second
	maximum := time.Duration(h.declaration.Restart.MaxSeconds) * time.Second
	if h.backoff < minimum {
		h.backoff = minimum
	} else if h.backoff < maximum {
		h.backoff *= 2
		if h.backoff > maximum {
			h.backoff = maximum
		}
	}
	h.state.RestartCount++
	h.rapidExits++
	h.state.Phase = "restart_wait"
	if h.rapidExits >= 5 {
		// This diagnostic does not invent a new restart-policy stop condition.
		h.state.Phase = "crashloop"
	}
	var random [1]byte
	if _, err := rand.Read(random[:]); err != nil {
		h.storageFailed = true
		h.state.Phase = "error"
		h.writeState()
		return true
	}
	h.nextSpawn = time.Now().Add(restartDelay(h.backoff, maximum, random[0]))
	h.writeState()
	h.event("restart_scheduled", exitReason, "", "")
	return false
}

func restartDelay(base, maximum time.Duration, random byte) time.Duration {
	// Positive jitter up to one quarter of base, capped at configured maximum.
	delay := base + time.Duration(int64(base/4)*int64(random)/255)
	if delay > maximum {
		return maximum
	}
	return delay
}

func (h *holder) finishStopReplies(err error) {
	for _, request := range h.stopReplies {
		reply := holderReply{frame: frame("ok"), err: err}
		request.reply <- reply
		if request.sent != nil {
			_ = waitClosed(context.Background(), request.sent, operationWait)
		}
	}
	h.stopReplies = nil
}

func (h *holder) request(request holderRequest) {
	if request.op == "listener_failed" {
		h.storageFailed = true
		h.beginCleanup(true)
		return
	}
	if request.op == "leave" {
		h.removeViewer(request.viewer.id, "viewer_detached")
		return
	}
	if request.op == "hello" {
		hello := h.hello()
		f := frame("hello")
		f.Hello = &hello
		request.reply <- holderReply{frame: f}
		return
	}
	if request.op == "viewer" {
		request.reply <- h.viewerOperation(request)
		return
	}
	if request.frame.Op == "attach" {
		request.reply <- h.attach(request)
		return
	}
	if request.frame.Op == "status" {
		state := h.state
		f := frame("status")
		f.State = &state
		request.reply <- holderReply{frame: f}
		return
	}
	if request.frame.Incarnation != h.state.Incarnation || h.quarantined {
		request.reply <- holderReply{err: ErrConflict}
		return
	}
	switch request.frame.Op {
	case "reload":
		snapshot, err := h.store.LoadConfig()
		current, exists := snapshot.Lookup(h.state.Name)
		if err != nil || !exists {
			request.reply <- holderReply{err: ErrConfig}
			return
		}
		if current.Revision != h.state.DeclarationRevision {
			h.event("drift", "config_changed", "", "")
		} else {
			h.event("reload", "", "", "")
		}
		request.reply <- holderReply{frame: frame("ok")}
	case "stop", "restart":
		if request.frame.Op == "restart" {
			h.stopReason = "operator_restart"
		}
		h.stopReplies = append(h.stopReplies, request)
		h.beginCleanup(true)
	default:
		request.reply <- holderReply{err: ErrProtocol}
	}
}
