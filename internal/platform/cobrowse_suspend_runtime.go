package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// completeStagedThrowawaySuspend is the private post-intent manager transaction.
// Its trusted caller MUST retain registry and action exclusion, normally through
// withStagedThrowawayCapture. It may not call ordinary Stop/teardown or a legacy
// writer. No production action invokes it yet. Saved profiles are unsupported.
//
// clean must remove only the pinned private working directory and honor ctx;
// closeRoot must honor ctx too. Neither callback may re-enter the manager/store.
// Failure keeps the slot and intent for recovery, not an automatic rollback.
// The finite budget is cooperative, not a hard synchronous-filesystem deadline.
func (m *CobrowseSessionManager) completeStagedThrowawaySuspend(ctx context.Context,
	s *cobrowseSession, store *suspendIntentStore, intent sessionRecord, now time.Time,
	closeRoot func(context.Context, suspendBrowserClosePlan, func() error) error,
	clean func(context.Context, string) error) error {
	refuse := func() error { return errors.New("browser suspend runtime transaction incomplete") }
	if m == nil || s == nil || store == nil || store.root == nil || store.payloads == nil || store.payloads.root == nil || ctx == nil || ctx.Err() != nil ||
		closeRoot == nil || clean == nil || now.IsZero() || intent.State != "suspending" ||
		intent.Persistent || intent.ProfileName != "" || intent.Suspension == nil || !m.useResolver {
		return refuse()
	}
	if _, finite := ctx.Deadline(); !finite {
		return refuse()
	}
	ctx, cancel := context.WithTimeout(ctx, suspendGracefulCloseLimit+suspendJobProofLimit)
	defer cancel()
	// Never wait on a manager/staging lock while holding the registry/action
	// gates. Keep the manager slot reserved across every awaited effect.
	if !m.mu.TryLock() {
		return refuse()
	}
	defer m.mu.Unlock()
	if m.sessions[intent.ID] != s || !s.stageMu.TryLock() {
		return refuse()
	}
	defer s.stageMu.Unlock()
	s.mu.Lock()
	proc, profile, regDir := s.proc, s.profile, s.regDir
	s.mu.Unlock()
	if proc == nil || proc.stopForSuspend == nil || proc.exited == nil ||
		!filepath.IsAbs(m.baseDir) || filepath.Clean(profile) != filepath.Join(m.baseDir, intent.ID) ||
		profile != intent.Profile || !sessionIDPattern.MatchString(intent.ID) || len(intent.ID) > 64 {
		return refuse()
	}
	exited, proof := proc.exited, proc.stopForSuspend
	baseInfo, e1 := os.Lstat(m.baseDir)
	workInfo, e2 := os.Lstat(profile)
	registryInfo, e3 := os.Stat(regDir)
	storeInfo, e4 := store.root.Stat(".")
	managerDir := m.registryDir()
	managerInfo, e5 := os.Stat(managerDir)
	if e1 != nil || e3 != nil || e4 != nil || e5 != nil || !baseInfo.IsDir() ||
		validateCobrowseBaseDirPerms(m.baseDir, baseInfo) != nil ||
		!os.SameFile(registryInfo, storeInfo) || !os.SameFile(managerInfo, storeInfo) ||
		(e2 != nil && !os.IsNotExist(e2)) ||
		(e2 == nil && (!workInfo.IsDir() || validateCobrowseBaseDirPerms(profile, workInfo) != nil ||
			(runtime.GOOS != "windows" && workInfo.Mode().Perm()&0o077 != 0))) {
		return refuse()
	}
	transitioned := false
	check := func() error {
		if ctx.Err() != nil || m.sessions[intent.ID] != s || m.registryDir() != managerDir {
			return refuse()
		}
		s.mu.Lock()
		valid := s.proc == proc && proc.exited == exited && s.id == intent.ID && s.generation == intent.Generation &&
			s.profile == profile && (s.workDir == "" || s.workDir == profile) && s.regDir == regDir &&
			!s.persistent && s.encProfile == nil && s.profileName == "" &&
			s.ownerUserID == intent.OwnerUserID && s.tracking == intent.Tracking && s.explicitHandoff == intent.Handoff &&
			(s.state == SessionSuspending || (!transitioned && (s.state == SessionRunning || s.state == SessionAttached))) &&
			proc.browserID == intent.BrowserID && proc.debugPort == intent.DebugPort &&
			proc.browserPID == intent.BrowserPID && proc.xvfbPID == intent.XvfbPID &&
			proc.browserScope == intent.BrowserScope && proc.xvfbScope == intent.XvfbScope && proc.scopeUser == intent.ScopeUser &&
			proc.containerID == intent.ContainerID && proc.containerStart == intent.ContainerStart && proc.containerCtrl == intent.ControlPort
		s.mu.Unlock()
		namedBase, bErr := os.Lstat(m.baseDir)
		namedRegistry, rErr := os.Stat(regDir)
		if !valid || bErr != nil || rErr != nil || !os.SameFile(baseInfo, namedBase) ||
			validateCobrowseBaseDirPerms(m.baseDir, namedBase) != nil ||
			!os.SameFile(registryInfo, namedRegistry) || store.private() != nil || ctx.Err() != nil {
			return refuse()
		}
		return nil
	}
	if check() != nil {
		return refuse()
	}
	s.stageClosed = true
	s.mu.Lock()
	s.state = SessionSuspending
	s.mu.Unlock()
	transitioned = true
	_, err := store.completeSuspending(ctx, intent, now, func(ctx context.Context, identity suspendStopIdentity) error {
		if identity != suspendIdentity(intent) || check() != nil ||
			stopSuspendProcWith(ctx, proc, check, closeRoot) != nil || check() != nil {
			return refuse()
		}
		// The positive adapter proved resources gone before any working-copy
		// cleanup. A replacement directory must not become cleanup authority.
		current, err := os.Lstat(profile)
		if err == nil && (workInfo == nil || !os.SameFile(workInfo, current) || !current.IsDir() ||
			validateCobrowseBaseDirPerms(profile, current) != nil ||
			(runtime.GOOS != "windows" && current.Mode().Perm()&0o077 != 0)) {
			return refuse()
		}
		if err != nil && !os.IsNotExist(err) {
			return refuse()
		}
		s.stopChooserWatch()
		if clean(ctx, profile) != nil || check() != nil {
			return refuse()
		}
		if _, err := os.Lstat(profile); !os.IsNotExist(err) {
			return refuse()
		}
		return nil
	})
	if err != nil || check() != nil {
		return refuse()
	}
	// An uncertain final publish can be reconciled without rerunning its
	// callback. Still require this proc's proof, reaper and absent working copy
	// before releasing the slot. Root exit or final metadata alone is not proof.
	if proof(ctx) != nil || check() != nil {
		return refuse()
	}
	select {
	case <-exited:
	default:
		return refuse()
	}
	if _, err := os.Lstat(profile); !os.IsNotExist(err) || check() != nil {
		return refuse()
	}
	s.stopChooserWatch()
	s.mu.Lock()
	s.proc, s.state = nil, SessionSuspended
	s.mu.Unlock()
	delete(m.sessions, intent.ID)
	dropTargetSet(proc.debugPort)
	return nil
}
