package heartbeat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// failCommandHook makes one Redis command fail while the rest of the
// connection stays healthy. On a real node the direct-Redis publisher shares a
// single connection for PUBLISH and XADD, so their failures are strongly
// correlated; this hook lets the test isolate the ordering/severity contract
// that the shared connection would otherwise hide.
type failCommandHook struct {
	command string
	err     error
}

func (h failCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h failCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if strings.EqualFold(cmd.Name(), h.command) {
			cmd.SetErr(h.err)
			return h.err
		}
		return next(ctx, cmd)
	}
}

func (h failCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func newTestRedisPublisher(t *testing.T) (*RedisPublisher, *miniredis.Miniredis, *[]string) {
	t.Helper()

	mr := miniredis.RunT(t)

	var mu sync.Mutex
	var logs []string

	pub, err := NewRedisPublisher(RedisPublisherConfig{
		RedisURL: "redis://" + mr.Addr(),
		NodeID:   "test-node",
		LogFn: func(level, msg string) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, level+": "+msg)
		},
	}, status.NewCollector(status.CollectorConfig{NodeName: "test-node"}))
	if err != nil {
		t.Fatalf("NewRedisPublisher: %v", err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	return pub, mr, &logs
}

// TestRedisPublishPubSubFailureIsNotFatal is the direct-Redis half of #722: a
// failing PUBLISH must not skip the durable XADD, and must not be reported as a
// heartbeat failure.
func TestRedisPublishPubSubFailureIsNotFatal(t *testing.T) {
	pub, mr, logs := newTestRedisPublisher(t)
	pub.client.AddHook(failCommandHook{command: "publish", err: errors.New("simulated PUBLISH failure")})

	msg := testStatusMessage()
	if err := pub.publishMessage(context.Background(), msg, ""); err != nil {
		t.Fatalf("PUBLISH failure must not fail the heartbeat, got: %v", err)
	}

	entries := streamEntries(t, mr)
	if len(entries) != 1 {
		t.Fatalf("durable stream write must happen despite PUBLISH failure, got %d entries", len(entries))
	}
	if got := entries[0]["nodeId"]; got != "test-node" {
		t.Errorf("stream entry nodeId = %q, want test-node", got)
	}

	if !containsSubstring(*logs, "pub/sub publish to node:status:test-node failing") {
		t.Errorf("first pub/sub failure must be surfaced, logs: %v", *logs)
	}
}

// TestRedisPublishStreamFailureIsFatal proves the durable write owns the error.
func TestRedisPublishStreamFailureIsFatal(t *testing.T) {
	pub, mr, _ := newTestRedisPublisher(t)
	pub.client.AddHook(failCommandHook{command: "xadd", err: errors.New("simulated XADD failure")})

	msg := testStatusMessage()
	err := pub.publishMessage(context.Background(), msg, "")
	if err == nil {
		t.Fatal("a failed durable stream write must return an error")
	}
	if !strings.Contains(err.Error(), "node:status:stream") {
		t.Errorf("error should name the durable stream, got: %v", err)
	}

	if entries := streamEntries(t, mr); len(entries) != 0 {
		t.Errorf("expected no stream entries, got %d", len(entries))
	}
}

// TestRedisPublishHappyPath keeps the ordinary case honest.
func TestRedisPublishHappyPath(t *testing.T) {
	pub, mr, logs := newTestRedisPublisher(t)

	msg := testStatusMessage()
	if err := pub.publishMessage(context.Background(), msg, "device-code-1"); err != nil {
		t.Fatalf("publishMessage: %v", err)
	}

	entries := streamEntries(t, mr)
	if len(entries) != 1 {
		t.Fatalf("expected 1 stream entry, got %d", len(entries))
	}
	if got := entries[0]["deviceCode"]; got != "device-code-1" {
		t.Errorf("deviceCode field = %q, want device-code-1", got)
	}
	if len(*logs) != 0 {
		t.Errorf("a healthy heartbeat must not log, got: %v", *logs)
	}
}

// streamEntries reads node:status:stream out of miniredis as field maps.
func streamEntries(t *testing.T, mr *miniredis.Miniredis) []map[string]string {
	t.Helper()
	raw, err := mr.Stream("node:status:stream")
	if err != nil {
		// miniredis returns an error for a key that was never created, which
		// for these tests means "no heartbeat landed".
		return nil
	}
	out := make([]map[string]string, 0, len(raw))
	for _, e := range raw {
		fields := map[string]string{}
		for i := 0; i+1 < len(e.Values); i += 2 {
			fields[e.Values[i]] = e.Values[i+1]
		}
		out = append(out, fields)
	}
	return out
}
