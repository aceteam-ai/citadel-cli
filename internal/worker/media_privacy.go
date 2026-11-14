package worker

import "github.com/aceteam-ai/citadel-cli/internal/jobprivacy"

func (job *Job) diagnosticPayload() map[string]any {
	if job.media != nil {
		return jobprivacy.PrivateMediaMarker()
	}
	return job.Payload
}

func (job *Job) sealMedia() {
	if job.media == nil {
		job.media = jobprivacy.CaptureMedia(job.Type, job.Payload, job.RawPayload, nil)
	}
	if job.media != nil {
		job.Payload = jobprivacy.PrivateMediaMarker()
		job.RawPayload = nil
	}
}

func (job *Job) executionPayload() (map[string]any, error) {
	if job.media == nil {
		return job.Payload, nil
	}
	payload, err := job.media.ExecutionPayload()
	if err == nil && job.mediaGPU != nil {
		payload["_gpuIndex"] = *job.mediaGPU
	}
	return payload, err
}

func (job *Job) executionField(key string) (any, bool) {
	if job.media != nil {
		return job.media.ExecutionField(key)
	}
	value, present := job.Payload[key]
	return value, present
}

func (job *Job) setGPUIndex(index int) {
	if job.media != nil {
		job.mediaGPU = &index
	} else {
		job.Payload["_gpuIndex"] = index
	}
}

func privateMediaRayID(media *jobprivacy.MediaPayload) string {
	if media == nil {
		return ""
	}
	data, _ := media.OriginalData()
	if ray, _ := data["rayId"].(string); ray != "" {
		return ray
	}
	value, _ := media.ExecutionField("rayId")
	ray, _ := value.(string)
	return ray
}
