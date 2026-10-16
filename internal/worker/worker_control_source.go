package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

const WorkerControlProtocol = "worker-control-v1"

// This is local transport state, never decoded from a job or a replacement
// for the server's verified enrollment and signature checks.
type workerControlBinding struct {
	orgID, nodeID, queue, group string
	client                      *redisapi.Client
}

func validateWorkerControl(cfg *redisapi.WorkerConfigResponse, token, orgID, nodeID string) bool {
	if strings.TrimSpace(token) == "" || orgID == "" || nodeID == "" || cfg == nil || cfg.OrgID != orgID || cfg.WorkerControl == nil {
		return false
	}
	c := cfg.WorkerControl
	return c.Protocol == WorkerControlProtocol && c.Group == WorkerControlProtocol && workerControlQueueMatches(c.Queue, orgID, nodeID)
}

// WorkerControlSource opts in only to metadata freshly fetched using the same
// device token as the normal API source. Control has its own queue and group;
// this method is unavailable to direct Redis workers.
func (s *APISource) WorkerControlSource(ctx context.Context, orgID, nodeID string) (*APISource, error) {
	if s.client == nil || strings.TrimSpace(s.config.Token) == "" || orgID == "" || nodeID == "" {
		return nil, nil
	}
	cfg, err := s.client.FetchWorkerConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !validateWorkerControl(cfg, s.config.Token, orgID, nodeID) {
		return nil, nil
	}
	c := cfg.WorkerControl
	control := NewAPISource(APISourceConfig{
		BaseURL: s.config.BaseURL, Token: s.config.Token,
		QueueName: c.Queue, ConsumerGroup: c.Group, BlockMs: s.config.BlockMs,
		DebugFunc: s.config.DebugFunc, LogFn: s.config.LogFn,
	})
	control.control = &workerControlBinding{orgID: orgID, nodeID: nodeID, queue: c.Queue, group: c.Group}
	return control, nil
}

func (b *workerControlBinding) matches(job *Job) bool {
	return b != nil && b.client != nil && job != nil && job.ID != "" &&
		job.Source == "redis-api" && job.SourceQueue == b.queue &&
		b.group == WorkerControlProtocol && workerControlQueueMatches(b.queue, b.orgID, b.nodeID)
}

func authenticatedWorkerControlStream(job *Job, stream StreamWriter) bool {
	if job == nil || !job.control.matches(job) {
		return false
	}
	if w, ok := stream.(*clockStreamWriter); ok {
		stream = w.StreamWriter
	}
	w, ok := stream.(interface{ authenticatedWorkerControl(*Job) bool })
	return ok && w.authenticatedWorkerControl(job)
}

func (w *APIStreamWriter) authenticatedWorkerControl(job *Job) bool {
	return job.control.matches(job) && w.client == job.control.client && w.jobID == job.ID
}

func isWorkerControlQueue(queue string) bool {
	return strings.HasPrefix(queue, "jobs:v1:control:")
}

// Generic sources cannot accidentally advertise control readiness by consuming
// a control queue supplied through --queue or dynamic resubscription.
func (s *APISource) validateControlQueues() error {
	queues := s.snapshotQueues()
	if s.control != nil && (len(queues) != 1 || queues[0] != s.control.queue || s.config.ConsumerGroup != WorkerControlProtocol) {
		return errors.New("worker control source requires its dedicated queue and group")
	}
	for _, queue := range queues {
		if isWorkerControlQueue(queue) && (s.control == nil || queue != s.control.queue || s.config.ConsumerGroup != WorkerControlProtocol || len(queues) != 1) {
			return errors.New("worker control queue requires a dedicated authenticated source")
		}
	}
	return nil
}

func workerControlQueue(orgID, nodeID string) string {
	return fmt.Sprintf("jobs:v1:control:org_%s:node:%s", orgID, nodeID)
}
