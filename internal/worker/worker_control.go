package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const workerControlCooldown = 2 * time.Minute

var workerControlReserveMu sync.Mutex

// WorkerControlConfig supplies the trusted node identity and the existing
// service-manager restart mechanism. StateDir must survive process restarts.
type WorkerControlConfig struct {
	NodeID   string
	StateDir string
	Managed  func() bool
	// Schedule prepares a restart that cannot fire until commit is called.
	Schedule func() (commit func(), cancel func(), err error)
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

// Execute accepts only an exactly addressed restart delivered over the per-node
// Redis stream. The producer's node:update/step-up authorization is upstream;
// the node independently checks the transport, queue, target, and action.
// A durable reservation is written before claiming acceptance. The runner is
// responsible for ACKing, scheduling, and publishing before the restart fires.
func (h *WorkerControlHandler) Execute(_ context.Context, job *Job, _ StreamWriter) (*JobResult, error) {
	if job == nil || job.ID == "" {
		return h.reject("invalid_job", "WORKER_CONTROL requires a job ID")
	}
	if job.Source != "redis" && job.Source != "redis-api" {
		return h.reject("invalid_source", "WORKER_CONTROL requires a Redis job source")
	}
	if h.cfg.NodeID == "" || !workerControlQueueMatches(job.SourceQueue, h.cfg.NodeID) {
		return h.reject("invalid_queue", "WORKER_CONTROL requires this node's per-node queue")
	}
	if len(job.Payload) == 0 {
		return h.reject("invalid_payload", "WORKER_CONTROL requires action and target_node")
	}
	for key := range job.Payload {
		switch key {
		case "action", "target_node", "timeout_ms", "rayId":
		default:
			return h.reject("unknown_field", fmt.Sprintf("WORKER_CONTROL field %q is unsupported", key))
		}
	}
	if timeout, exists := job.Payload["timeout_ms"]; exists {
		value, ok := timeout.(string)
		if !ok {
			return h.reject("invalid_payload", "WORKER_CONTROL timeout_ms must be a positive integer string")
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return h.reject("invalid_payload", "WORKER_CONTROL timeout_ms must be a positive integer string")
		}
	}
	if ray, exists := job.Payload["rayId"]; exists {
		if _, ok := ray.(string); !ok {
			return h.reject("invalid_payload", "WORKER_CONTROL rayId must be a string")
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
	reserved, err := h.marker(job.ID, "reserved")
	if err != nil {
		return h.reject("state_unavailable", "worker restart state directory is unavailable")
	}
	workerControlReserveMu.Lock()
	defer workerControlReserveMu.Unlock()
	aborted, err := h.marker(job.ID, "aborted")
	if err != nil {
		return h.reject("state_unavailable", "worker restart state directory is unavailable")
	}
	if _, err := os.Stat(aborted); err == nil {
		return h.reject("aborted_control", "this restart request was not accepted; dispatch a new job")
	} else if !errors.Is(err, os.ErrNotExist) {
		return h.reject("state_unavailable", "worker restart state could not be checked")
	}
	if _, err := os.Stat(reserved); errors.Is(err, os.ErrNotExist) {
		busy, checkErr := recentWorkerControlMarker(filepath.Dir(reserved), filepath.Base(reserved))
		if checkErr != nil {
			return h.reject("state_unavailable", "worker restart state could not be checked")
		}
		if busy {
			return h.reject("restart_cooldown", "a worker restart was recently accepted on this node")
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
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"action": "restart", "accepted": true, "restarting": true,
		"message": "worker restart accepted; verify a fresh heartbeat after reconnect",
	}}, nil
}

// A new job ID is held briefly when a restart is already reserved or fired.
// The producer also coalesces by node in Redis; this local guard covers a new
// dispatch ID after a producer timeout. Exact-ID tombstones remain forever.
func recentWorkerControlMarker(dir, own string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	ownStem := strings.TrimSuffix(own, ".reserved")
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.TrimSuffix(strings.TrimSuffix(name, ".reserved"), ".fired") == ownStem ||
			(!strings.HasSuffix(name, ".reserved") && !strings.HasSuffix(name, ".fired")) {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(name, ".reserved"), ".fired")
		if _, err := os.Stat(filepath.Join(dir, stem+".aborted")); err == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return false, err
		}
		if time.Since(info.ModTime()) < workerControlCooldown {
			return true, nil
		}
	}
	return false, nil
}

func workerControlQueueMatches(queue, nodeID string) bool {
	parts := strings.Split(queue, ":")
	return len(parts) == 6 && parts[0] == "jobs" && parts[1] == "v1" &&
		parts[2] == "shell" && strings.HasPrefix(parts[3], "org_") &&
		len(parts[3]) > len("org_") && parts[4] == "node" && parts[5] == nodeID
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

// PrepareAfterAck schedules the exit behind a gate. It is called only after a
// successful queue ACK. The runner opens the gate after the accepted result is
// published. The fired marker prevents a second schedule for this job ID.
func (h *WorkerControlHandler) PrepareAfterAck(job *Job) (commit func(), cancel func(), err error) {
	if job == nil || job.ID == "" {
		return nil, nil, errors.New("missing WORKER_CONTROL job ID")
	}
	commit, cancel, err = h.cfg.Schedule()
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
	fired, err := h.marker(job.ID, "fired")
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
	return commit, cancel, nil
}

// Abort persistently refuses a job whose ACK or scheduling failed. This makes
// a later redelivery inert while releasing the cooldown for a fresh job ID.
func (h *WorkerControlHandler) Abort(job *Job) error {
	aborted, err := h.marker(job.ID, "aborted")
	if err != nil {
		return err
	}
	if _, err := createDurableMarker(aborted); err != nil {
		return err
	}
	for _, suffix := range []string{"fired", "reserved"} {
		path, err := h.marker(job.ID, suffix)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// PrepareWorkerRestart creates a delayed process exit behind an infallible
// commit gate. Production's exit callback is the existing processExiter(1),
// which does not return. A cancel before commit prevents the exit entirely.
func PrepareWorkerRestart(exit func()) (func(), func(), error) {
	if exit == nil {
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
			exit()
		}
	}()
	return func() { once.Do(func() { close(commitCh) }) },
		func() { once.Do(func() { close(cancelCh) }) }, nil
}
