package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// A delivery is admitted only after its original HTTP claim is renewed. No
// passive observation, Pub/Sub message or later epoch can replace this claim.
type deliveryControl interface {
	RenewDelivery(context.Context, *redisapi.DeliveryClaim) (time.Duration, error)
	CompleteDelivery(context.Context, *redisapi.DeliveryClaim, []byte) error
}

type durableDelivery struct {
	control         deliveryControl
	claim           *redisapi.DeliveryClaim
	now             func() time.Time
	mu              sync.Mutex
	finishMu        sync.Mutex
	expiry          *time.Timer
	deadline        time.Time
	renewAt         time.Time
	live, committed bool
	intent          []byte // frozen once, including after an ambiguous commit response
	terminal        *deliveryTerminal
	raw             StreamWriter
	lost            context.Context
	lose            context.CancelFunc
	loop            context.Context
	stopLoop        context.CancelFunc
	onStop          func()
	stopOnce        sync.Once
}
type deliveryTerminal struct {
	kind    string
	output  json.RawMessage
	message string
}

func newDurableDelivery(control deliveryControl, claim *redisapi.DeliveryClaim) *durableDelivery {
	lost, lose := context.WithCancel(context.Background())
	loop, stop := context.WithCancel(context.Background())
	return &durableDelivery{control: control, claim: claim.Clone(), now: time.Now, lost: lost, lose: lose, loop: loop, stopLoop: stop}
}

func (d *durableDelivery) renew(ctx context.Context) error {
	start := d.now() // monotonic sample BEFORE the HTTP request; latency is charged.
	d.mu.Lock()
	previous := d.deadline
	refused := d.loop.Err() != nil || d.committed || (!previous.IsZero() && !d.live)
	d.mu.Unlock()
	if refused {
		return redisapi.ErrDurableDelivery
	}
	if !previous.IsZero() {
		remaining := previous.Sub(start)
		if remaining <= 0 {
			d.stop()
			return redisapi.ErrDurableDelivery
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, remaining)
		defer cancel()
	}
	budget, err := d.control.RenewDelivery(ctx, d.claim)
	deadline := start.Add(budget)
	d.mu.Lock()
	// Completion owns the receipt barrier now. Cancelling an overlapping renew
	// must not cancel the post-receipt wake publisher as though the lease was lost.
	if d.committed {
		d.mu.Unlock()
		return redisapi.ErrDurableDelivery
	}
	if err != nil || ctx.Err() != nil || budget <= 0 || budget > 5*time.Minute || !d.now().Before(deadline) || (!previous.IsZero() && !d.now().Before(previous)) || d.loop.Err() != nil || (!previous.IsZero() && !d.live) {
		d.mu.Unlock()
		d.stop()
		return redisapi.ErrDurableDelivery
	}
	d.deadline, d.live = deadline, true
	d.renewAt = start.Add(budget / 2)
	if d.expiry != nil {
		d.expiry.Stop()
	}
	d.expiry = time.AfterFunc(deadline.Sub(d.now()), func() {
		d.mu.Lock()
		expired := d.live && d.deadline.Equal(deadline) && !d.now().Before(deadline)
		if expired {
			d.live = false
		}
		d.mu.Unlock()
		if expired {
			d.stop()
		}
	})
	d.mu.Unlock()
	return nil
}

func (d *durableDelivery) keepAlive() {
	for {
		d.mu.Lock()
		remaining := d.deadline.Sub(d.now())
		wait := d.renewAt.Sub(d.now())
		d.mu.Unlock()
		if remaining <= 0 {
			d.stop()
			return
		}
		timer := time.NewTimer(max(wait, 0))
		select {
		case <-d.loop.Done():
			timer.Stop()
			return
		case <-timer.C:
			if d.renew(d.loop) != nil {
				return
			}
		}
	}
}
func (d *durableDelivery) stop() {
	d.mu.Lock()
	d.live = false
	if d.expiry != nil {
		d.expiry.Stop()
	}
	d.mu.Unlock()
	d.lose()
	d.stopLoop()
	d.stopOnce.Do(func() {
		if d.onStop != nil {
			d.onStop()
		}
	})
}
func (d *durableDelivery) validLocked() bool {
	return d.live && !d.committed && d.lost.Err() == nil && d.now().Before(d.deadline)
}
func (d *durableDelivery) valid() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.validLocked()
}
func (d *durableDelivery) isCommitted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.committed
}
func (d *durableDelivery) bind(ctx context.Context) (context.Context, func()) {
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.lost, cancel)
	if !d.valid() {
		cancel()
	}
	return bound, func() { stop(); cancel() }
}

// Runner writes stage one terminal; handlers get a different wrapper whose
// terminal methods never stage or publish. Only source Ack/Fail can commit it.
func (d *durableDelivery) stage(terminal deliveryTerminal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.validLocked() {
		return nil
	}
	if d.terminal == nil {
		d.terminal = &terminal
	}
	return nil
}

func (d *durableDelivery) finish(ctx context.Context, fallback error, code string) error {
	d.finishMu.Lock()
	defer d.finishMu.Unlock()
	d.mu.Lock()
	if d.committed {
		d.mu.Unlock()
		return nil
	}
	if !d.validLocked() {
		d.mu.Unlock()
		d.stop()
		return redisapi.ErrDurableDelivery
	}
	if d.intent == nil {
		terminal := d.terminal
		if terminal == nil && fallback != nil {
			terminal = &deliveryTerminal{kind: "error", message: fallback.Error()}
		}
		if terminal == nil {
			d.mu.Unlock()
			d.stop()
			return redisapi.ErrDurableDelivery
		}
		var payload any
		if terminal.kind == "result" {
			payload = map[string]any{"result": terminal.output}
		} else {
			if terminal.kind == "cancelled" {
				code = "cancelled"
			}
			if code == "" {
				code = "job_failed"
			}
			payload = map[string]any{"error": map[string]any{"code": code, "message": terminal.message}}
		}
		var err error
		d.intent, err = json.Marshal(payload)
		if err != nil {
			d.mu.Unlock()
			d.stop()
			return redisapi.ErrDurableDelivery
		}
		d.terminal = terminal
	}
	envelope := bytes.Clone(d.intent)
	remaining := d.deadline.Sub(d.now())
	d.mu.Unlock()
	// Worker cancellation still owes a cancelled terminal. Preserve request values,
	// but bound this HTTP-only operation by the currently held lease, never a new one.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remaining)
	defer cancel()
	if err := d.control.CompleteDelivery(commitCtx, d.claim, envelope); err != nil {
		d.stop()
		return redisapi.ErrDurableDelivery
	}
	d.mu.Lock()
	if !d.validLocked() {
		d.mu.Unlock()
		d.stop()
		return redisapi.ErrDurableDelivery
	}
	d.committed, d.live = true, false
	if d.expiry != nil {
		d.expiry.Stop()
	}
	terminal, raw := *d.terminal, d.raw
	d.mu.Unlock()
	d.stopLoop()
	defer d.lose()
	d.stopOnce.Do(func() {
		if d.onStop != nil {
			d.onStop()
		}
	})
	if raw == nil {
		return nil
	}
	// The durable receipt already ACKed atomically. Failed wake publication can
	// never fall through to a legacy ACK or a second execution.
	return retryStreamWrite(commitCtx, func() error {
		switch terminal.kind {
		case "result":
			var output map[string]any
			if json.Unmarshal(terminal.output, &output) != nil {
				return redisapi.ErrDurableDelivery
			}
			return raw.WriteEnd(output)
		case "cancelled":
			return raw.WriteCancelled(terminal.message)
		default:
			return raw.WriteError(errors.New(terminal.message), false)
		}
	})
}

type durableStream struct {
	StreamWriter
	delivery *durableDelivery
	handler  bool
}

func (s *durableStream) WriteClaimed(v string) error {
	if !s.delivery.valid() {
		return nil
	}
	return s.StreamWriter.WriteClaimed(v)
}
func (s *durableStream) WriteStart(v string) error {
	if !s.delivery.valid() {
		return nil
	}
	return s.StreamWriter.WriteStart(v)
}
func (s *durableStream) WriteChunk(v string, i int) error {
	if !s.delivery.valid() {
		return nil
	}
	return s.StreamWriter.WriteChunk(v, i)
}
func (s *durableStream) WriteProgress(v map[string]any) error {
	if !s.delivery.valid() {
		return nil
	}
	return s.StreamWriter.WriteProgress(v)
}
func (s *durableStream) WriteEnd(v map[string]any) error {
	if s.handler {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return redisapi.ErrDurableDelivery
	}
	return s.delivery.stage(deliveryTerminal{kind: "result", output: raw})
}
func (s *durableStream) WriteError(err error, _ bool) error {
	if s.handler {
		return nil
	}
	message := "job failed"
	if err != nil {
		message = err.Error()
	}
	return s.delivery.stage(deliveryTerminal{kind: "error", message: message})
}
func (s *durableStream) WriteCancelled(reason string) error {
	if s.handler {
		return nil
	}
	return s.delivery.stage(deliveryTerminal{kind: "cancelled", message: reason})
}
func handlerDeliveryStream(job *Job, stream StreamWriter) StreamWriter {
	if job.delivery == nil {
		return stream
	}
	return &durableStream{StreamWriter: stream, delivery: job.delivery(), handler: true}
}

// Only top-level stream metadata counts. A payload field cannot opt a legacy
// delivery into v1, and malformed/partial metadata cannot opt v1 back out.
func hasDurableCarrier[M ~map[string]V, V any](data M) bool {
	for name := range data {
		for _, key := range []string{"protocol", "dispatchId", "requestDigest", "claimToken", "leaseEpoch"} {
			if strings.EqualFold(name, key) {
				return true
			}
		}
	}
	return false
}

func (r *Runner) cancelWaitingDelivery(ctx context.Context, job *Job, stream StreamWriter, err error) {
	if job.delivery != nil && job.delivery().valid() {
		_ = stream.WriteCancelled("Job cancelled while queued")
		_ = r.source.Ack(ctx, job)
		return
	}
	_ = r.source.Nack(ctx, job, err)
}
