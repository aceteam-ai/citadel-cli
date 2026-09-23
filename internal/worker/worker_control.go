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

const workerControlCooldown = 2 * time.Minute

var workerControlReserveMu sync.Mutex

// WorkerControlConfig supplies the trusted node identity and the existing
// service-manager restart mechanism. StateDir must survive process restarts.
type WorkerControlConfig struct {
	NodeID   string
	StateDir string
	Managed  func() bool
	Restart  func() error
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
// responsible for publishing the returned result and ACKing before AfterAck.
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
		if value, ok := timeout.(string); !ok || value == "" {
			return h.reject("invalid_payload", "WORKER_CONTROL timeout_ms must be a nonempty string")
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
	if h.cfg.Managed == nil || !h.cfg.Managed() || h.cfg.Restart == nil {
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
	}
	duplicate, err := createDurableMarker(reserved)
	if err != nil {
		return h.reject("state_unavailable", "worker restart reservation could not be persisted")
	}
	return &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"action": "restart", "accepted": true, "restarting": true,
		"duplicate": duplicate,
		"message":   "worker restart accepted; verify a fresh heartbeat after reconnect",
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

// AfterAck runs only after the runner has published the typed result and
// acknowledged the queue message. A durable fired marker grants one restart
// attempt for this job ID, including across duplicate deliveries and reboot.
func (h *WorkerControlHandler) AfterAck(job *Job) error {
	if job == nil || job.ID == "" {
		return errors.New("missing WORKER_CONTROL job ID")
	}
	fired, err := h.marker(job.ID, "fired")
	if err != nil {
		return err
	}
	already, err := createDurableMarker(fired)
	if err != nil || already {
		return err
	}
	h.cfg.Log("WORKER_CONTROL: restarting managed worker after result and queue ack (job %s)", job.ID)
	if err := h.cfg.Restart(); err != nil {
		h.cfg.Log("WORKER_CONTROL: restart failed for job %s: %v", job.ID, err)
		return err
	}
	return nil
}
