package worker

import (
	"context"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type captureHandler struct{ payload map[string]string }

func (h *captureHandler) Execute(_ jobs.JobContext, job *nexus.Job) ([]byte, error) {
	h.payload = job.Payload
	return []byte(`{}`), nil
}

// A JSON number node id must reach the handler as a plain decimal, not as
// fmt.Sprint's "1e+06".
func TestCobrowseBackendNodeIDKeepsDecimalForm(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{float64(1000000), "1000000"},
		{float64(123), "123"},
		{float64(12345678901), "12345678901"},
		{"77", "77"},
	} {
		h := &captureHandler{}
		a := NewLegacyHandlerAdapter(JobTypeCobrowseSession, h)
		_, err := a.Execute(context.Background(), &Job{Type: JobTypeCobrowseSession, Payload: map[string]any{
			"action": "set_file_input", "backend_node_id": tc.in,
		}}, &NoOpStreamWriter{})
		if err != nil {
			t.Fatal(err)
		}
		if got := h.payload["backend_node_id"]; got != tc.want {
			t.Errorf("%v -> %q, want %q", tc.in, got, tc.want)
		}
	}
}
