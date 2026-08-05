package worker

import "context"

// wakePump turns a per-node Pub/Sub "wake" nudge into an immediate, non-blocking
// stream drain while keeping the normal blocking poll as the correctness backstop
// (issue #7270, Phase 1). It is shared by RedisSource and APISource; only the
// underlying read + subscribe mechanics differ.
//
// Design (see the #7270 design notes and the advisor review):
//
//   - **Never abandon a read.** A single reader goroutine does exactly ONE
//     blocking read at a time via a request/response handshake (readReq/readResp).
//     We never cancel a blocking XREADGROUP that may have already moved a message
//     into the consumer group's pending list, so no message is lost.
//   - **No continuous prefetch.** The reader only reads when next() asks, so
//     read/process stays serialized exactly as before. A wake-path return can
//     leave one blocking read still in flight; its result is delivered to the
//     NEXT next() call — a bounded prefetch-by-one, never a runaway.
//   - **Poll cadence preserved.** The blocking read returns at least every
//     BlockMs (job or nil), so next() returns at least that often and the
//     self-heal monitor (issue #548) never mistakes a wake-enabled idle node for
//     a wedge.
//   - **Coalesced signal.** trigger is buffered size 1 with a non-blocking send,
//     so a burst of 100 nudges collapses to one pending drain and never backs up
//     the subscriber goroutine.
//
// wakePump is deliberately kept OFF the JobSource interface (like AddQueue), so
// the Nexus HTTP source and the test mocks are unaffected.
type wakePump struct {
	// readBlocking performs one normal (BlockMs) read; readNonBlocking performs
	// an immediate, non-blocking read of the same stream(s).
	readBlocking    func(context.Context) (*Job, error)
	readNonBlocking func(context.Context) (*Job, error)

	trigger  chan struct{}
	readReq  chan struct{}
	readResp chan wakeReadResult

	// readInFlight tracks whether a blocking read is outstanding. Mutated only by
	// next(), which is called by the single runner loop goroutine, so no lock is
	// needed.
	readInFlight bool
}

type wakeReadResult struct {
	job *Job
	err error
}

// newWakePump builds a pump over the two read closures.
func newWakePump(readBlocking, readNonBlocking func(context.Context) (*Job, error)) *wakePump {
	return &wakePump{
		readBlocking:    readBlocking,
		readNonBlocking: readNonBlocking,
		trigger:         make(chan struct{}, 1),
		readReq:         make(chan struct{}),
		readResp:        make(chan wakeReadResult),
	}
}

// start launches the single reader goroutine. It exits when ctx is cancelled.
func (w *wakePump) start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.readReq:
				job, err := w.readBlocking(ctx)
				select {
				case w.readResp <- wakeReadResult{job: job, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
}

// signal delivers a coalesced wake. Safe to call from the subscriber goroutine
// (or a burst of them); never blocks.
func (w *wakePump) signal() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// next returns the next job, waking immediately on a nudge. It blocks up to one
// BlockMs (the reader's blocking read) when no nudge arrives, so the runner's
// poll cadence is unchanged.
func (w *wakePump) next(ctx context.Context) (*Job, error) {
	// Ask the reader for a blocking read if one isn't already outstanding (an
	// outstanding read is a prior wake-path prefetch we're still awaiting).
	if !w.readInFlight {
		select {
		case w.readReq <- struct{}{}:
			w.readInFlight = true
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	select {
	case res := <-w.readResp:
		w.readInFlight = false
		return res.job, res.err
	case <-w.trigger:
		// A nudge arrived: drain the stream right now. The in-flight blocking
		// read stays pending and its result is consumed by the next next() call.
		// A nudge that raced ahead of the XADD's visibility yields (nil, nil)
		// here; the pending blocking read (or the next poll) catches the job.
		return w.readNonBlocking(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
