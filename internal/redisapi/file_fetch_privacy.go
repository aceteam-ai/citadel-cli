package redisapi

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
)

func (job Job) MarshalJSON() ([]byte, error) {
	type plain Job
	safe := plain(job)
	if job.Type == jobprivacy.FileFetch {
		safe.Payload, safe.RawPayload, safe.RawData = nil, nil, nil
	}
	return json.Marshal(safe)
}

func (job Job) Format(state fmt.State, verb rune) {
	if job.Type == jobprivacy.FileFetch {
		_, _ = io.WriteString(state, "<FILE_FETCH job; payload omitted>")
		return
	}
	type plain Job
	_, _ = fmt.Fprintf(state, fmt.FormatString(state, verb), plain(job))
}
