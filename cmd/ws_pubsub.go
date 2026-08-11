// cmd/ws_pubsub.go
//
// Background retry for the real-time pub/sub WebSocket (issue #723).
//
// The bug this closes: EnableWebSocket was called exactly once at worker
// startup. A single failed connect was logged at Debug and then forgotten for
// the lifetime of the process -- ReconnectEnabled only ever governed a
// connection that had already succeeded once, so there was nothing left to
// reconnect. And the "optional HTTP fallback" it degraded to does not actually
// work (citadel-cli#721), so the node published no durable heartbeat and the
// platform considered it offline. A node sat that way for twelve hours.
//
// The timing is the ordinary case, not bad luck: workers restart BECAUSE the
// control plane blipped, so the startup connect is disproportionately likely to
// land inside the very window that breaks it.
//
// The retry deliberately reuses connectWithBackoffLabeled -- the same policy the
// control-plane connect a few lines above already uses -- rather than inventing
// a second scheme: exponential backoff (2s..2min) with +/-20% jitter, a 429
// retry_after honored IN FULL, and ctx-aware chunked sleeps so shutdown stays
// instant. That is what keeps this from recreating #443: the retry is
// in-process (no systemd restart storm), it never polls tighter than the server
// asked, and it stops permanently on first success.
package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// wsLogFunc reports retry progress at a severity the caller's log sink
// understands. Levels used: "warning" for the degrade, "info" for the recovery.
// The worker's Log() ignores the level (it has one stream); the control center's
// activity() renders it, so a recovery does not surface there as a warning.
type wsLogFunc func(level, format string, args ...any)

// wsConnector adapts a redisapi.Client's WebSocket enablement to the
// apiConnector interface connectWithBackoff retries against.
//
// Client.EnableWebSocket is idempotent (no-op when already connected, exactly
// one dial attempt otherwise) and no longer discards the WebSocket client on
// failure, which is what makes retrying it meaningful at all.
type wsConnector struct{ client *redisapi.Client }

func (w wsConnector) Connect(ctx context.Context) error {
	return w.client.EnableWebSocket(ctx)
}

// enableWebSocketWithRetry connects the pub/sub WebSocket, and on failure keeps
// retrying in the background instead of giving up.
//
// It returns immediately. The first attempt is made synchronously so the common
// (healthy) case is unchanged and the heartbeat's first publish already has the
// WebSocket; only a FAILED first attempt spawns the background retry loop, which
// exits on first success or on ctx cancellation.
//
// Non-fatal by design -- pub/sub is degraded, not dead, and the worker must
// still consume jobs -- but no longer silent: the failure is logged at Log
// level (it used to be Debug, which is why a node in this state produced no
// evidence at default verbosity), and so is the recovery.
//
// Re-arming after a LOST connection is already handled one layer down by
// WSClient.handleDisconnect -> reconnect, which retries indefinitely and
// restores subscriptions. This function deliberately does not duplicate that.
//
// It used to be unsafe as well as redundant: a second connect path could start a
// second readLoop on the same gorilla Conn, which permits exactly one concurrent
// reader (#740). That is fixed (WSClient.loopsOnce starts the loops once for the
// client's whole life), so the reason to keep a single re-arm path is now purely
// that two of them would dial twice and leave one of the connections orphaned
// (#748), not that they would corrupt the reader.
// The returned channel closes when the retry loop has finished (immediately
// when no loop was needed). Production callers ignore it; it exists so a test
// can join the goroutine instead of sleeping and guessing.
func enableWebSocketWithRetry(ctx context.Context, conn apiConnector, logf wsLogFunc) <-chan struct{} {
	done := make(chan struct{})

	err := conn.Connect(ctx)
	if err == nil || ctx.Err() != nil {
		close(done)
		return done
	}

	logf("warning", "pub/sub WebSocket unavailable, falling back to HTTP and retrying in background: %v", err)

	go func() {
		defer close(done)
		if err := connectWithBackoffLabeled(ctx, "pub/sub WebSocket", conn); err != nil {
			// The only non-nil return is context cancellation (clean shutdown).
			return
		}
		logf("info", "pub/sub WebSocket connected on retry; publishes are back on the real-time transport")
	}()

	return done
}

// wakeEnabler is the subset of worker.APISource armWakeAfterWebSocket needs.
type wakeEnabler interface {
	EnableWake(ctx context.Context, channel string) error
}

// armWakeAfterWebSocket re-arms per-node push-wake once a background WebSocket
// connect finally lands.
//
// Push-wake (#7270) is one-shot post-connect setup: APISource.EnableWake
// registers a "message" handler and subscribes to the node's wake channel, and
// it refuses when the WebSocket is not connected. Before #723 that check could
// only ever see the startup verdict, because the verdict was permanent. Now the
// WebSocket can come up minutes later -- and because a late connect goes through
// Connect rather than reconnect, WSClient's OnReconnect callbacks do NOT fire,
// so nothing would re-run this wiring.
//
// Without this, a node that started during a control-plane blip would recover
// its heartbeat (visible) but stay poll-only forever (invisible, and reported as
// a healthy "websocket" transport) -- the same class of silent partial failure
// #723 is about.
//
// No-op when the retry already finished: EnableWake was then called against the
// final state and there is nothing further to wait for.
func armWakeAfterWebSocket(ctx context.Context, wsRetryDone <-chan struct{}, src wakeEnabler, channel string, logf wsLogFunc) {
	if wsRetryDone == nil || src == nil || channel == "" {
		return
	}
	select {
	case <-wsRetryDone:
		return
	default:
	}

	go func() {
		select {
		case <-wsRetryDone:
		case <-ctx.Done():
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err := src.EnableWake(ctx, channel); err != nil {
			logf("warning", "per-node push-wake still unavailable after the WebSocket retry (staying poll-only): %v", err)
			return
		}
		logf("info", "per-node push-wake armed after WebSocket recovery: %s", channel)
	}()
}

// workerWSLog adapts the worker's single-stream Log() to wsLogFunc. The level is
// dropped deliberately: latest.log has no severity column, and the point of
// #723 is that these lines appear AT ALL (they used to be Debug-only).
func workerWSLog(_ string, format string, args ...any) {
	Log(format, args...)
}
