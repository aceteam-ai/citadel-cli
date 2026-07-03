// Package worklock provides a single-instance guard for the Citadel worker
// (`citadel work`), keyed to a node's state directory.
//
// Why this exists (issues #443 / #435): a stale duplicate worker running beside
// the systemd-managed one is the amplifier behind the node-identity churn
// incident. Two workers for the same node:
//   - split job routing non-deterministically (an AGENT_UPDATE landed on a stale
//     old-version worker: "node 2.57.0 has no handler"),
//   - double the control-plane request volume (contributing to the 429 self-DoS),
//   - race on the same tsnet state directory, which can trigger a re-register
//     under a NEW fabric id + new mesh IP (the churn).
//
// The guard makes a second `citadel work` for the same node STRUCTURALLY unable
// to start: the first holder takes an exclusive OS advisory lock on a lock file
// inside the node's config dir, and any second invocation is refused with a clear
// message naming the holding PID. The lock is keyed to the resolved state dir, so
// every invocation on the same box (root service, sudo interactive, distinct
// service user) that converges on the same node identity also converges on the
// same lock.
//
// Robustness: the lock is an OS advisory lock (flock on Unix, LockFileEx on
// Windows), so it is released automatically by the kernel when the holding
// process dies — a crashed worker never leaves a lock that blocks restart. As an
// additional clarity layer, the lock file records the holder PID so a refusal can
// name it, and a stale lock whose recorded PID is dead is reclaimed with an
// explanatory log rather than an opaque failure.
package worklock

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LockFileName is the name of the lock file placed inside the node config dir.
// It lives next to the "network" state subdirectory rather than inside it so it
// is never swept away by network.ClearState (which RemoveAll's the network dir).
const LockFileName = "citadel-work.lock"

// LockPathForStateDir derives the lock file path from a resolved network state
// directory (network.GetStateDir(), e.g. <node>/network). The lock is placed in
// the PARENT (the node config dir) so it survives a network-state reset and so a
// single node has exactly one worker lock regardless of how state is resolved.
func LockPathForStateDir(stateDir string) string {
	// stateDir is typically <nodeConfigDir>/network; the lock belongs in
	// <nodeConfigDir>. filepath.Dir of a trailing-slash-free path gives the parent.
	parent := filepath.Dir(filepath.Clean(stateDir))
	return filepath.Join(parent, LockFileName)
}

// staleLockAction is the decision for what to do when a lock file already exists
// with a recorded holder PID.
type staleLockAction int

const (
	// actionRefuse: the holder is alive (or presumed alive) — refuse to start.
	actionRefuse staleLockAction = iota
	// actionReclaim: the recorded holder PID is dead — the lock is stale and may
	// be reclaimed.
	actionReclaim
)

// decideStaleLock is the pure classifier for a pre-existing lock file. It never
// touches the OS: callers pass the recorded holder PID, this process's PID,
// whether the holder is alive, and whether the live holder is actually a citadel
// process. Kept pure so the reclaim-vs-refuse policy is unit tested without
// spawning processes.
//
//   - holderPID <= 0 (unreadable/empty lock file): reclaim — a garbage lock file
//     must never permanently block a worker.
//   - holderPID == selfPID: reclaim — our own stale record is safe to overwrite.
//   - holderAlive == false: reclaim — the recorded process is gone.
//   - holderAlive but holderIsCitadel == false: reclaim — the recorded PID was
//     reused by an unrelated process (e.g. after a reboot the PID counter wrapped
//     onto some other program). A reused PID must never be mistaken for a live
//     worker and permanently wedge startup.
//   - otherwise: refuse — a live citadel process holds the node.
//
// holderIsCitadel is only consulted when holderAlive is true; callers pass a
// best-effort value (true on platforms where the cmdline cannot be inspected, so
// the guard fails closed and still refuses).
func decideStaleLock(holderPID, selfPID int, holderAlive, holderIsCitadel bool) staleLockAction {
	if holderPID <= 0 {
		return actionReclaim
	}
	if holderPID == selfPID {
		return actionReclaim
	}
	if !holderAlive {
		return actionReclaim
	}
	if !holderIsCitadel {
		return actionReclaim
	}
	return actionRefuse
}

// lockRecord is the JSON payload written into the lock file. It carries enough
// context to (a) print an actionable refusal naming the holder and when it
// started, and (b) detect a reused PID (a rebooted box whose PID counter wrapped
// onto some other process) by cross-checking liveness + cmdline at read time.
type lockRecord struct {
	PID       int    `json:"pid"`
	StartUnix int64  `json:"start_unix"`
	Version   string `json:"version,omitempty"`
}

// encodeRecord serializes a lock record to the bytes written into the lock file.
func encodeRecord(rec lockRecord) []byte {
	b, err := json.Marshal(rec)
	if err != nil {
		// Marshalling a fixed struct of scalars cannot realistically fail; fall
		// back to a bare PID line so the file is at least human-readable.
		return []byte(strconv.Itoa(rec.PID) + "\n")
	}
	return append(b, '\n')
}

// decodeRecord parses lock file bytes. It accepts both the JSON record format and
// a legacy bare-PID line (older workers wrote only the PID), so an in-place
// upgrade never misreads a lock file written by a prior version.
func decodeRecord(data []byte) lockRecord {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return lockRecord{}
	}
	if strings.HasPrefix(trimmed, "{") {
		var rec lockRecord
		if err := json.Unmarshal([]byte(trimmed), &rec); err == nil {
			return rec
		}
		return lockRecord{}
	}
	// Legacy bare-PID format.
	if pid, err := strconv.Atoi(trimmed); err == nil && pid > 0 {
		return lockRecord{PID: pid}
	}
	return lockRecord{}
}

// ErrAlreadyRunning is returned by Acquire when another live worker holds the
// lock for the same node. It carries the holder PID and start time for a clear,
// actionable operator message.
type ErrAlreadyRunning struct {
	// PID is the recorded holder PID, or 0 if it could not be read.
	PID int
	// StartTime is the recorded holder start time, or the zero value if unknown.
	StartTime time.Time
	// Path is the lock file path, for diagnostics.
	Path string
}

func (e *ErrAlreadyRunning) Error() string {
	if e.PID > 0 {
		started := "unknown time"
		if !e.StartTime.IsZero() {
			started = e.StartTime.Format(time.RFC3339)
		}
		return fmt.Sprintf("citadel worker already running (PID %d, started %s); refusing to start a second instance", e.PID, started)
	}
	return "citadel worker already running; refusing to start a second instance"
}
