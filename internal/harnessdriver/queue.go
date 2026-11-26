package harnessdriver

import (
	"context"
	"errors"
	"io"
	"sync"
)

var ErrClosed = errors.New("harness_driver_queue_closed")

// EventQueue is bounded by both event count and the protocol envelope limit.
// No event, including control messages, is dropped or coalesced. A full queue
// backpressures producers, which must pass a cancellable context. Each queue is
// intended for one stream; sequence allocation remains the caller's job.
type EventQueue struct {
	mu       sync.Mutex
	items    []Event
	capacity int
	closed   bool
	changed  chan struct{}
}

func NewEventQueue(capacity int) (*EventQueue, error) {
	if capacity < 1 || capacity > 64 {
		return nil, ErrInvalidLimit
	}
	return &EventQueue{capacity: capacity, changed: make(chan struct{})}, nil
}
func (q *EventQueue) signal() { close(q.changed); q.changed = make(chan struct{}) }
func (q *EventQueue) Push(ctx context.Context, e Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	e.Data = append(e.Data[:0:0], e.Data...)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return ErrClosed
		}
		if len(q.items) < q.capacity {
			q.items = append(q.items, e)
			q.signal()
			q.mu.Unlock()
			return nil
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (q *EventQueue) Pop(ctx context.Context) (Event, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
		q.mu.Lock()
		if len(q.items) > 0 {
			e := q.items[0]
			copy(q.items, q.items[1:])
			q.items[len(q.items)-1] = Event{}
			q.items = q.items[:len(q.items)-1]
			q.signal()
			q.mu.Unlock()
			return e, nil
		}
		if q.closed {
			q.mu.Unlock()
			return Event{}, io.EOF
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-changed:
		}
	}
}

// Close refuses new pushes and lets consumers drain already queued events.
func (q *EventQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		q.signal()
	}
}
