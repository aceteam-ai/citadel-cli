package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
)

func (s *RedisSource) ackDelivery(ctx context.Context, kind, queue, messageID string) error {
	if jobprivacy.OmitPayload(kind) {
		return s.client.AckSensitiveJob(ctx, queue, messageID)
	}
	if queue != "" {
		return s.client.AckJobOnQueue(ctx, queue, messageID)
	}
	return s.client.AckJob(ctx, messageID)
}

func (job Job) MarshalJSON() ([]byte, error) {
	type plain Job
	safe := plain(job)
	if jobprivacy.OmitPayload(job.Type) {
		safe.Payload, safe.RawPayload = nil, nil
	} else if jobprivacy.HasPasscode(job.Payload) {
		// The raw stream bytes carry the passcode too, so drop them.
		safe.Payload = jobprivacy.WithoutPasscode(job.Payload)
		safe.RawPayload = nil
	}
	return json.Marshal(safe)
}

func (job Job) Format(state fmt.State, verb rune) {
	if jobprivacy.OmitPayload(job.Type) {
		_, _ = io.WriteString(state, "<"+job.Type+" job; payload omitted>")
		return
	}
	type plain Job
	safe := plain(job)
	if jobprivacy.HasPasscode(job.Payload) {
		safe.Payload = jobprivacy.WithoutPasscode(job.Payload)
		safe.RawPayload = nil
	}
	_, _ = fmt.Fprintf(state, fmt.FormatString(state, verb), safe)
}
