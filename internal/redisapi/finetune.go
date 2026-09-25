package redisapi

import (
	"context"
	"fmt"
	"net/http"
)

// FineTuneState is read from the platform's canonical finetune job hash and
// cancel key through a device-authenticated, node-scoped API route.
type FineTuneState struct {
	Status          string   `json:"status"`
	ProgressPercent float64  `json:"progress_percent"`
	CurrentEpoch    int      `json:"current_epoch"`
	CurrentLoss     *float64 `json:"current_loss"`
	Cancelled       bool     `json:"cancelled"`
	Error           *string  `json:"error"`
	FinishedAt      *string  `json:"finished_at"`
	AdapterPath     *string  `json:"adapter_path"`
}

func (c *Client) FineTuneState(ctx context.Context, jobID string) (FineTuneState, error) {
	var state FineTuneState
	err := c.doRequest(ctx, http.MethodGet, "/api/fabric/redis/finetune/"+jobID, nil, &state)
	return state, err
}

func (c *Client) FineTuneUpdate(ctx context.Context, jobID string, fields map[string]any) error {
	return c.fineTuneUpdate(ctx, jobID, fields, false)
}

func (c *Client) FineTuneFailCritical(ctx context.Context, jobID string, fields map[string]any) error {
	if fields["status"] != "failed" || fields["error"] == nil || fields["finished_at"] == nil {
		return fmt.Errorf("critical fine-tune failure requires status, error and finish time")
	}
	return c.fineTuneUpdate(ctx, jobID, fields, true)
}

func (c *Client) fineTuneUpdate(ctx context.Context, jobID string, fields map[string]any, critical bool) error {
	if len(fields) == 0 {
		return fmt.Errorf("fine-tune update has no fields")
	}
	body := map[string]any{"fields": fields}
	if critical {
		body["critical_failure"] = true
	}
	return c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/finetune/"+jobID, body, nil)
}
