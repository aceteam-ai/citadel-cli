package worker

import (
	"encoding/json"
	"errors"
	"sync"
)

// A control delivery has one claim followed by one immutable outcome. Retries
// can resend the same event after an uncertain response, but cannot replace a
// terminal outcome or reopen it with a late claim. The server independently
// enforces enrollment, exact job identity and terminal ownership.
type workerControlStreamWriter struct {
	*APIStreamWriter
	mu       sync.Mutex
	claimed  bool
	terminal string
}

func (w *workerControlStreamWriter) WriteClaimed(version string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal != "" {
		return errors.New("worker control is already terminal")
	}
	if err := w.APIStreamWriter.WriteClaimed(version); err != nil {
		return err
	}
	w.claimed = true
	return nil
}

func (w *workerControlStreamWriter) terminalEvent(kind string, data map[string]any, publish func() error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.claimed {
		return errors.New("worker control outcome requires an authenticated claim")
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	identity := kind + string(encoded)
	if w.terminal != "" && w.terminal != identity {
		return errors.New("worker control outcome cannot change after publication")
	}
	w.terminal = identity // Freeze even if the response is lost after delivery.
	return publish()
}

func (w *workerControlStreamWriter) WriteEnd(result map[string]any) error {
	return w.terminalEvent("end", result, func() error { return w.APIStreamWriter.WriteEnd(result) })
}

func (w *workerControlStreamWriter) WriteError(err error, recoverable bool) error {
	return w.terminalEvent("error", map[string]any{"error": err.Error(), "recoverable": recoverable}, func() error { return w.APIStreamWriter.WriteError(err, recoverable) })
}

func (w *workerControlStreamWriter) WriteStart(string) error { return nil }
func (w *workerControlStreamWriter) WriteChunk(string, int) error {
	return errors.New("unsupported control event")
}
func (w *workerControlStreamWriter) WriteProgress(map[string]any) error {
	return errors.New("unsupported control event")
}
func (w *workerControlStreamWriter) WriteCancelled(string) error {
	return errors.New("unsupported control event")
}
