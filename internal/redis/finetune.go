package redis

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

var fineTuneKeyID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
var ErrFineTuneCancelled = errors.New("fine-tune job cancelled")

const fineTuneCancelledLua = `
local key = KEYS[1]
if redis.call('HGET', key, 'org_id') ~= ARGV[1] or redis.call('HGET', key, 'node_id') ~= ARGV[2] then return -1 end
local status = redis.call('HGET', key, 'status')
if status == 'cancelling' or status == 'cancelled' then return 1 end
if status ~= 'succeeded' and status ~= 'failed' and redis.call('EXISTS', KEYS[2]) == 1 then return 1 end
return 0`

const fineTuneUpdateLua = `
local key = KEYS[1]
if redis.call('HGET', key, 'org_id') ~= ARGV[1] or redis.call('HGET', key, 'node_id') ~= ARGV[2] then return -1 end
local status = redis.call('HGET', key, 'status')
local critical = ARGV[4] == '1'
if critical and ARGV[3] ~= 'failed' then return -3 end
if status == 'succeeded' or status == 'failed' or status == 'cancelled' then
  if ARGV[3] ~= status then return -3 end
  for i=5,#ARGV,2 do
    if redis.call('HGET', key, ARGV[i]) ~= ARGV[i+1] then return -3 end
  end
  return 1
end
if (status == 'cancelling' or redis.call('EXISTS', KEYS[2]) == 1) and
   ARGV[3] ~= 'cancelled' and not (ARGV[3] == 'failed' and critical) then return -2 end
if ARGV[3] == 'running' and status ~= 'queued' and status ~= 'pending' and status ~= 'running' then return -3 end
if ARGV[3] == 'succeeded' and status ~= 'running' then return -3 end
if ARGV[3] == '' and status ~= 'running' then return -3 end
for i=5,#ARGV,2 do redis.call('HSET', key, ARGV[i], ARGV[i+1]) end
return 1`

// FineTuneCancelled checks the platform's canonical cancellation state. Both
// the job hash and cancel key are scoped to the same org and pinned node.
func (c *Client) FineTuneCancelled(ctx context.Context, jobID, orgID, nodeID string) (bool, error) {
	if !fineTuneKeyID.MatchString(jobID) || orgID == "" || nodeID == "" {
		return false, errors.New("invalid fine-tune identity")
	}
	n, err := c.client.Eval(ctx, fineTuneCancelledLua, []string{"finetune:job:" + jobID, "cancel:" + jobID}, orgID, nodeID).Int()
	if err != nil {
		return false, err
	}
	if n < 0 {
		return false, errors.New("fine-tune job does not belong to this org and node")
	}
	return n == 1, nil
}

// FineTuneUpdate writes only the platform's existing state fields, preserving
// its 31-day TTL and refusing to resurrect a terminal job.
func (c *Client) FineTuneUpdate(ctx context.Context, jobID, orgID, nodeID string, fields map[string]any) error {
	return c.fineTuneUpdate(ctx, jobID, orgID, nodeID, fields, false)
}

// FineTuneFailCritical is the only update permitted after a cancellation
// request besides worker-confirmed cancellation. It is reserved for cleanup
// failures that must remain visible instead of falsely claiming cancellation.
func (c *Client) FineTuneFailCritical(ctx context.Context, jobID, orgID, nodeID string, fields map[string]any) error {
	if fields["status"] != "failed" || fields["error"] == nil || fields["finished_at"] == nil {
		return errors.New("critical fine-tune failure requires status, error and finish time")
	}
	return c.fineTuneUpdate(ctx, jobID, orgID, nodeID, fields, true)
}

func (c *Client) fineTuneUpdate(ctx context.Context, jobID, orgID, nodeID string, fields map[string]any, critical bool) error {
	if !fineTuneKeyID.MatchString(jobID) || orgID == "" || nodeID == "" {
		return errors.New("invalid fine-tune identity")
	}
	allowed := map[string]bool{"status": true, "started_at": true, "finished_at": true, "progress_percent": true, "current_epoch": true, "current_loss": true, "eta_seconds": true, "error": true, "adapter_path": true}
	status, _ := fields["status"].(string)
	criticalArg := "0"
	if critical {
		criticalArg = "1"
	}
	args := []any{orgID, nodeID, status, criticalArg}
	for key, value := range fields {
		if !allowed[key] {
			return fmt.Errorf("unsupported fine-tune state field %q", key)
		}
		args = append(args, key, fmt.Sprint(value))
	}
	n, err := c.client.Eval(ctx, fineTuneUpdateLua, []string{"finetune:job:" + jobID, "cancel:" + jobID}, args...).Int()
	if err != nil {
		return err
	}
	switch n {
	case 1:
		return nil
	case -2:
		return ErrFineTuneCancelled
	case -3:
		return errors.New("fine-tune job is terminal")
	default:
		return errors.New("fine-tune job does not belong to this org and node")
	}
}
