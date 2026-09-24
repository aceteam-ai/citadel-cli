package worker

import (
	"context"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

type apiFineTuneControl struct{ source *APISource }

func NewAPIFineTuneControl(source *APISource) FineTuneControl {
	return &apiFineTuneControl{source: source}
}

func (c *apiFineTuneControl) client() (*redisapi.Client, error) {
	if c.source == nil || c.source.Client() == nil {
		return nil, fmt.Errorf("fine-tune API control unavailable")
	}
	return c.source.Client(), nil
}

func (c *apiFineTuneControl) Cancelled(ctx context.Context, jobID string) (bool, error) {
	client, err := c.client()
	if err != nil {
		return false, err
	}
	state, err := client.FineTuneState(ctx, jobID)
	return state.Cancelled, err
}

func (c *apiFineTuneControl) Update(ctx context.Context, jobID string, fields map[string]any) error {
	client, err := c.client()
	if err != nil {
		return err
	}
	if err = client.FineTuneUpdate(ctx, jobID, fields); err == nil {
		return nil
	}
	// A lost HTTP response is ambiguous: the backend may already have applied
	// the atomic update. Read back the canonical state before retrying or
	// failing a successful training job. Never infer ownership from a generic
	// KV endpoint; the scoped GET must itself succeed.
	state, readErr := client.FineTuneState(ctx, jobID)
	if readErr == nil && fineTuneStateConfirms(state, fields) {
		return nil
	}
	return err
}

func fineTuneStateConfirms(state redisapi.FineTuneState, fields map[string]any) bool {
	if value, ok := fields["status"]; ok && state.Status != value {
		return false
	}
	if value, ok := fields["progress_percent"]; ok {
		n, ok := fineTuneNumber(value)
		if !ok || state.ProgressPercent != n {
			return false
		}
	}
	if value, ok := fields["current_epoch"]; ok {
		n, ok := fineTuneNumber(value)
		if !ok || float64(state.CurrentEpoch) != n {
			return false
		}
	}
	if value, ok := fields["current_loss"]; ok {
		n, ok := fineTuneNumber(value)
		if !ok || state.CurrentLoss == nil || *state.CurrentLoss != n {
			return false
		}
	}
	return len(fields) > 0
}

func fineTuneNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}

// The scoped POST publishes progress after committing the hash update.
func (c *apiFineTuneControl) Progress(context.Context, string, map[string]any) error { return nil }
