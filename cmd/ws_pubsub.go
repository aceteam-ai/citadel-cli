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
// restores subscriptions. This function deliberately does not duplicate that:
// a second connect path could start a second readLoop on the same connection.
// The returned channel closes when the retry loop has finished (immediately
// when no loop was needed). Production callers ignore it; it exists so a test
// can join the goroutine instead of sleeping and guessing.
func enableWebSocketWithRetry(ctx context.Context, conn apiConnector, logf func(string, ...any)) <-chan struct{} {
	done := make(chan struct{})

	err := conn.Connect(ctx)
	if err == nil || ctx.Err() != nil {
		close(done)
		return done
	}

	logf("pub/sub WebSocket unavailable, falling back to HTTP and retrying in background: %v", err)

	go func() {
		defer close(done)
		if err := connectWithBackoffLabeled(ctx, "pub/sub WebSocket", conn); err != nil {
			// The only non-nil return is context cancellation (clean shutdown).
			return
		}
		logf("pub/sub WebSocket connected on retry; publishes are back on the real-time transport")
	}()

	return done
}
