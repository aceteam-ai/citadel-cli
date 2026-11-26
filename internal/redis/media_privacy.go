package redis

import "github.com/aceteam-ai/citadel-cli/internal/jobprivacy"

func (job *Job) diagnosticPayload() map[string]any {
	if job.media != nil {
		return jobprivacy.PrivateMediaMarker()
	}
	return job.Payload
}

func (job *Job) sealMedia() {
	if job.media == nil {
		job.media = jobprivacy.CaptureMedia(job.Type, job.Payload, job.RawPayload, job.RawData)
	}
	if job.media != nil {
		job.Payload = jobprivacy.PrivateMediaMarker()
		job.RawPayload, job.RawData = nil, nil
	}
}

// PrivateMediaPayload transfers an opaque, immutable execution snapshot to the
// trusted worker adapter. It is unrelated to passive delivery observations.
func (job *Job) PrivateMediaPayload() *jobprivacy.MediaPayload {
	return job.media.Clone()
}
