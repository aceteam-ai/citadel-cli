package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
)

func (s *RedisSource) ackDelivery(ctx context.Context, kind string, payload map[string]any, queue, messageID string) error {
	if kind == JobTypeFileFetch {
		return s.client.AckFileFetchJob(ctx, queue, messageID)
	}
	// Only a stage_file entry (a user's file as base64) is disposable; every
	// other COBROWSE_SESSION action keeps the plain ack below.
	if kind == JobTypeCobrowseSession {
		if action, _ := payload["action"].(string); action == "stage_file" {
			return s.client.AckStagedFileJob(ctx, queue, messageID)
		}
	}
	if queue != "" {
		return s.client.AckJobOnQueue(ctx, queue, messageID)
	}
	return s.client.AckJob(ctx, messageID)
}

func (job Job) MarshalJSON() ([]byte, error) {
	type plain Job
	safe := plain(job)
	if job.media != nil || job.Type == jobprivacy.FileFetch || jobprivacy.IsPrivateMedia(job.Type, job.Payload) {
		safe.Payload, safe.RawPayload = nil, nil
	} else if jobprivacy.HasPasscode(job.Payload) {
		// The raw stream bytes carry the passcode too, so drop them.
		safe.Payload = jobprivacy.WithoutPasscode(job.Payload)
		safe.RawPayload = nil
	}
	return json.Marshal(safe)
}

func (job Job) Format(state fmt.State, verb rune) {
	if job.media != nil || jobprivacy.IsPrivateMedia(job.Type, job.Payload) {
		_, _ = io.WriteString(state, "<MEDIA_GENERATE job; payload omitted>")
		return
	}
	if job.Type == jobprivacy.FileFetch {
		_, _ = io.WriteString(state, "<FILE_FETCH job; payload omitted>")
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
