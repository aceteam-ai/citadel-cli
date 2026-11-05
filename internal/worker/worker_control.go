package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var workerControlReserveMu sync.Mutex

// WorkerControlConfig supplies the trusted node identity and the existing
// service-manager restart mechanism. StateDir must survive process restarts.
type WorkerControlConfig struct {
	OrgID    string
	NodeID   string
	StateDir string
	Managed  func() bool
	// Schedule prepares a restart that cannot fire until commit is called.
	Schedule func(valid func() bool) (commit func(), cancel func(), err error)
	Log      func(string, ...any)
}

type WorkerControlHandler struct{ cfg WorkerControlConfig }

func NewWorkerControlHandler(cfg WorkerControlConfig) *WorkerControlHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &WorkerControlHandler{cfg: cfg}
}

func (h *WorkerControlHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeWorkerControl
}

func (h *WorkerControlHandler) reject(code, message string) (*JobResult, error) {
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"action": "restart", "accepted": false, "restarting": false,
		"code": code, "message": message,
	}}, nil
}

// Execute accepts only an exactly addressed restart over the dedicated
// authenticated stream. The producer's node:update/step-up authorization is upstream;
// the node independently checks the transport, queue, target, and action.
// A confirmed durable claim precedes the local reservation. The runner waits
// for a correlated completion permit before opening the prepared restart gate.
func (h *WorkerControlHandler) Execute(_ context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	if job == nil || job.ID == "" {
		return h.reject("invalid_job", "WORKER_CONTROL requires a job ID")
	}
	if !authenticatedWorkerControlStream(job, stream) {
		return h.reject("unauthenticated_control", "WORKER_CONTROL requires authenticated consumption and results")
	}
	if job.controlSession == nil || !job.controlSession.CanPrepare() {
		return h.reject("claim_unconfirmed", "worker control requires a live correlated claim")
	}
	if !workerControlQueueMatches(job.SourceQueue, h.cfg.OrgID, h.cfg.NodeID) {
		return h.reject("invalid_queue", "WORKER_CONTROL requires this node's per-node queue")
	}
	if len(job.Payload) == 0 {
		return h.reject("invalid_payload", "WORKER_CONTROL requires action and target_node")
	}
	for key := range job.Payload {
		switch key {
		case "action", "target_node":
		default:
			return h.reject("unknown_field", fmt.Sprintf("WORKER_CONTROL field %q is unsupported", key))
		}
	}
	action, actionOK := job.Payload["action"].(string)
	if !actionOK || action != "restart" {
		return h.reject("invalid_action", "WORKER_CONTROL supports only action=restart")
	}
	target, targetOK := job.Payload["target_node"].(string)
	if !targetOK || target != h.cfg.NodeID {
		return h.reject("invalid_target", "WORKER_CONTROL target_node must match this node")
	}
	if h.cfg.Managed == nil || !h.cfg.Managed() || h.cfg.Schedule == nil {
		return h.reject("unmanaged_worker", "worker restart requires a managed service")
	}
	if h.cfg.StateDir == "" {
		return h.reject("state_unavailable", "worker restart state directory is unavailable")
	}
	reserved, err := h.marker(controlReservationKey(job), "reserved")
	if err != nil {
		return h.reject("state_unavailable", "worker restart state directory is unavailable")
	}
	workerControlReserveMu.Lock()
	defer workerControlReserveMu.Unlock()
	aborted, err := h.marker(controlReservationKey(job), "aborted")
	if err != nil {
		return h.reject("state_unavailable", "worker restart state directory is unavailable")
	}
	if _, err := os.Stat(aborted); err == nil {
		return h.reject("aborted_control", "this restart request was cancelled and cannot be resumed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return h.reject("state_unavailable", "worker restart state could not be checked")
	}
	if _, err := os.Stat(reserved); errors.Is(err, os.ErrNotExist) {
		busy, checkErr := unresolvedWorkerControlMarker(filepath.Dir(reserved), filepath.Base(reserved))
		if checkErr != nil {
			return h.reject("state_unavailable", "worker restart state could not be checked")
		}
		if busy {
			return h.reject("restart_unconfirmed", "an unresolved worker restart still fences this node")
		}
	} else if err != nil {
		return h.reject("state_unavailable", "worker restart state could not be checked")
	} else {
		return h.reject("duplicate_control", "this restart request is already reserved or completed")
	}
	duplicate, err := createDurableMarker(reserved)
	if err != nil {
		return h.reject("state_unavailable", "worker restart reservation could not be persisted")
	}
	if duplicate {
		return h.reject("duplicate_control", "this restart request is already reserved or completed")
	}
	if !job.controlSession.CanPrepare() {
		_ = h.Abort(job)
		return h.reject("deadline_expired", "worker control deadline elapsed during reservation")
	}
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"action": "restart", "accepted": true, "restarting": true,
		"message": "worker restart prepared; outcome requires trusted supervisor verification",
	}}, nil
}

// Any unresolved reservation fences this node across job IDs and process
// replacements. Age and local cancellation cannot prove server resolution.
func unresolvedWorkerControlMarker(dir, own string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	ownStem := strings.TrimSuffix(own, ".reserved")
	for _, entry := range entries {
		name := entry.Name()
		stem := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".aborted"), ".commit"), ".reserved"), ".fired")
		if entry.IsDir() || stem == ownStem || stem == name {
			continue
		}
		return true, nil
	}
	return false, nil
}

func workerControlQueueMatches(queue, orgID, nodeID string) bool {
	return orgID != "" && nodeID != "" &&
		queue == workerControlQueue(orgID, nodeID)
}

func (h *WorkerControlHandler) marker(jobID, suffix string) (string, error) {
	dir := filepath.Join(h.cfg.StateDir, "worker-control")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(jobID))
	return filepath.Join(dir, fmt.Sprintf("%x.%s", digest, suffix)), nil
}

// createDurableMarker returns true when the marker already exists. A restart
// tombstone is never removed automatically: the same job ID can be redelivered
// after any process restart without triggering another restart loop.
func createDurableMarker(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer d.Close()
	return false, d.Sync()
}

// PrepareAfterAck schedules the exit after the durable logical claim/ACK.
// Only a correlated completion permit can open that gate.
func (h *WorkerControlHandler) PrepareAfterAck(job *Job) (commit func(), cancel func(), err error) {
	if job == nil || job.ID == "" {
		return nil, nil, errors.New("missing WORKER_CONTROL job ID")
	}
	if job.controlSession == nil || !job.controlSession.CanPrepare() {
		return nil, nil, errors.New("worker control claim expired or cancelled")
	}
	commit, cancel, err = h.cfg.Schedule(func() bool {
		if job.controlSession.CanCommit() {
			return true
		}
		_ = h.Abort(job)
		return false
	})
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, err
	}
	if commit == nil || cancel == nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, errors.New("restart schedule returned no commit or cancel")
	}
	fired, err := h.marker(controlReservationKey(job), "fired")
	if err != nil {
		cancel()
		return nil, nil, err
	}
	already, err := createDurableMarker(fired)
	if err != nil || already {
		cancel()
		if already {
			err = errors.New("restart already prepared for this job")
		}
		return nil, nil, err
	}
	if !job.controlSession.CanPrepare() {
		cancel()
		_ = h.Abort(job)
		return nil, nil, errors.New("worker control deadline elapsed during preparation")
	}
	rawCommit, rawCancel := commit, cancel
	var gate sync.Mutex
	closed := false
	cancel = func() {
		gate.Lock()
		defer gate.Unlock()
		if closed {
			return
		}
		closed = true
		job.controlSession.Cancel()
		rawCancel()
		_ = h.Abort(job)
	}
	commit = func() {
		gate.Lock()
		defer gate.Unlock()
		if closed {
			return
		}
		if !job.controlSession.CanCommit() {
			closed = true
			job.controlSession.Cancel()
			rawCancel()
			_ = h.Abort(job)
			return
		}
		intent, err := h.marker(controlReservationKey(job), "commit")
		if err == nil {
			_, err = createDurableMarker(intent)
		}
		if err != nil || !job.controlSession.CanCommit() {
			closed = true
			job.controlSession.Cancel()
			rawCancel()
			_ = h.Abort(job)
			return
		}
		closed = true
		rawCommit()
	}
	return commit, cancel, nil
}

func controlReservationKey(job *Job) string {
	if job != nil && job.controlSession != nil {
		return job.controlSession.ReservationKey()
	}
	return "invalid-control"
}

// Abort permanently cancels a local attempt while preserving its reservation.
// A failed or uncertain operation cannot release the node's unresolved fence.
func (h *WorkerControlHandler) Abort(job *Job) error {
	if job == nil {
		return errors.New("missing worker control attempt")
	}
	if job.controlSession != nil {
		job.controlSession.Cancel()
	}
	aborted, err := h.marker(controlReservationKey(job), "aborted")
	if err != nil {
		return err
	}
	if _, err := createDurableMarker(aborted); err != nil {
		return err
	}
	return nil
}

// PrepareWorkerRestart creates a delayed process exit behind an infallible
// commit gate. Production's exit callback is the existing processExiter(1),
// which does not return. A cancel before commit prevents the exit entirely.
func PrepareWorkerRestart(exit func(), valid func() bool) (func(), func(), error) {
	if exit == nil || valid == nil {
		return nil, nil, errors.New("worker exit callback is unavailable")
	}
	commitCh := make(chan struct{})
	cancelCh := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-cancelCh:
			return
		case <-commitCh:
			time.Sleep(500 * time.Millisecond)
			if valid() {
				exit()
			}
		}
	}()
	return func() { once.Do(func() { close(commitCh) }) },
		func() { once.Do(func() { close(cancelCh) }) }, nil
}
