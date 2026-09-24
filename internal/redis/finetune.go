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
if redis.call('HGET', key, 'status') == 'cancelled' or redis.call('EXISTS', KEYS[2]) == 1 then return 1 end
return 0`

const fineTuneUpdateLua = `
local key = KEYS[1]
if redis.call('HGET', key, 'org_id') ~= ARGV[1] or redis.call('HGET', key, 'node_id') ~= ARGV[2] then return -1 end
local status = redis.call('HGET', key, 'status')
if (status == 'cancelled' or redis.call('EXISTS', KEYS[2]) == 1) and ARGV[3] ~= 'cancelled' then return -2 end
if status == 'succeeded' or status == 'failed' or status == 'cancelled' then
  if ARGV[3] ~= status then return -3 end
  for i=4,#ARGV,2 do
    if redis.call('HGET', key, ARGV[i]) ~= ARGV[i+1] then return -3 end
  end
  return 1
end
if ARGV[3] == 'running' and status ~= 'queued' and status ~= 'pending' and status ~= 'running' then return -3 end
if ARGV[3] == 'succeeded' and status ~= 'running' and status ~= 'succeeded' then return -3 end
if ARGV[3] == '' and status ~= 'running' then return -3 end
for i=4,#ARGV,2 do redis.call('HSET', key, ARGV[i], ARGV[i+1]) end
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
	if !fineTuneKeyID.MatchString(jobID) || orgID == "" || nodeID == "" {
		return errors.New("invalid fine-tune identity")
	}
	allowed := map[string]bool{"status": true, "started_at": true, "finished_at": true, "progress_percent": true, "current_epoch": true, "current_loss": true, "eta_seconds": true, "error": true, "adapter_path": true}
	status, _ := fields["status"].(string)
	args := []any{orgID, nodeID, status}
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
