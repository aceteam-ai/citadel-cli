package worker

import (
	"context"
	"fmt"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
)

type redisFineTuneControl struct {
	source *RedisSource
	orgID  string
	nodeID string
}

// NewRedisFineTuneControl uses the existing direct Redis connection. API-mode
// workers need the scoped backend companion before this capability is exposed.
func NewRedisFineTuneControl(source *RedisSource, orgID, nodeID string) FineTuneControl {
	return &redisFineTuneControl{source: source, orgID: orgID, nodeID: nodeID}
}

func (c *redisFineTuneControl) client() (*redisclient.Client, error) {
	if c.source == nil || c.source.Client() == nil || c.orgID == "" || c.nodeID == "" {
		return nil, fmt.Errorf("fine-tune Redis control unavailable")
	}
	return c.source.Client(), nil
}

func (c *redisFineTuneControl) Cancelled(ctx context.Context, jobID string) (bool, error) {
	client, err := c.client()
	if err != nil {
		return false, err
	}
	return client.FineTuneCancelled(ctx, jobID, c.orgID, c.nodeID)
}

func (c *redisFineTuneControl) Update(ctx context.Context, jobID string, fields map[string]any) error {
	client, err := c.client()
	if err != nil {
		return err
	}
	return client.FineTuneUpdate(ctx, jobID, c.orgID, c.nodeID, fields)
}

func (c *redisFineTuneControl) Progress(ctx context.Context, jobID string, event map[string]any) error {
	client, err := c.client()
	if err != nil {
		return err
	}
	return client.PublishStreamEvent(ctx, jobID, "", "progress", event)
}
