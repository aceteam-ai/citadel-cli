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
}

func (c *Client) FineTuneState(ctx context.Context, jobID string) (FineTuneState, error) {
	var state FineTuneState
	err := c.doRequest(ctx, http.MethodGet, "/api/fabric/redis/finetune/"+jobID, nil, &state)
	return state, err
}

func (c *Client) FineTuneUpdate(ctx context.Context, jobID string, fields map[string]any) error {
	if len(fields) == 0 {
		return fmt.Errorf("fine-tune update has no fields")
	}
	return c.doRequest(ctx, http.MethodPost, "/api/fabric/redis/finetune/"+jobID, map[string]any{"fields": fields}, nil)
}
