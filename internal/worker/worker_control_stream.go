package worker

import (
	"errors"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol"
)

// Control uses dedicated correlated operations, never generic publication.
type workerControlStreamWriter struct {
	*APIStreamWriter
	session *workercontrol.Session
}

func (w *workerControlStreamWriter) authenticatedWorkerControl(job *Job) bool {
	return job.control.matches(job) && w.session == job.controlSession && w.client == job.control.client && w.jobID == job.ID
}
func (w *workerControlStreamWriter) WriteClaimed(string) error {
	if w.session == nil {
		return workercontrol.ErrDenied
	}
	return w.session.Claim(w.ctx)
}
func (w *workerControlStreamWriter) WriteEnd(result map[string]any) error {
	if w.session == nil {
		return workercontrol.ErrDenied
	}
	accepted, ok := result["accepted"].(bool)
	if !ok {
		w.session.Cancel()
		return workercontrol.ErrInvalid
	}
	if accepted {
		return w.session.Complete(w.ctx, "prepared", "prepared")
	}
	code, ok := result["code"].(string)
	if !ok {
		code = "refused"
	}
	return w.session.Complete(w.ctx, "refused_no_effect", code)
}
func (w *workerControlStreamWriter) WriteError(error, bool) error {
	if w.session == nil {
		return workercontrol.ErrDenied
	}
	return w.session.Complete(w.ctx, "refused_no_effect", "execution_failed")
}
func (w *workerControlStreamWriter) WriteStart(string) error { return nil }
func (w *workerControlStreamWriter) WriteChunk(string, int) error {
	return errors.New("unsupported control event")
}
func (w *workerControlStreamWriter) WriteProgress(map[string]any) error {
	return errors.New("unsupported control event")
}
func (w *workerControlStreamWriter) WriteCancelled(string) error {
	if w.session != nil {
		w.session.Cancel()
	}
	return workercontrol.ErrDenied
}
