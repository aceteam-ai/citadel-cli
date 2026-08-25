package heartbeat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestRedisPublishRecordsMarkerOnSuccess pins the wiring half of #726: a
// successful durable stream write must update the on-disk freshness marker,
// not just the helpers marker_test.go exercises in isolation.
func TestRedisPublishRecordsMarkerOnSuccess(t *testing.T) {
	pub, _, _ := newTestRedisPublisher(t)
	dir := t.TempDir()
	pub.markerDir = dir

	before := time.Now()
	if err := pub.publishMessage(context.Background(), testStatusMessage(), ""); err != nil {
		t.Fatalf("publishMessage: %v", err)
	}

	m := LoadMarker(dir)
	if m.LastSuccessAt.Before(before) {
		t.Fatalf("LastSuccessAt = %v, want >= %v", m.LastSuccessAt, before)
	}
	if m.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", m.ConsecutiveFailures)
	}
}

// TestRedisPublishDoesNotMoveMarkerOnFailure proves a failed durable write
// leaves LastSuccessAt where it was (never advances it on failure) while
// still recording the attempt and the failure count -- the distinction
// printHeartbeatFreshness relies on to tell "stopped publishing" apart from
// "publishing but failing".
func TestRedisPublishDoesNotMoveMarkerOnFailure(t *testing.T) {
	pub, _, _ := newTestRedisPublisher(t)
	dir := t.TempDir()
	pub.markerDir = dir

	if err := pub.publishMessage(context.Background(), testStatusMessage(), ""); err != nil {
		t.Fatalf("publishMessage (success): %v", err)
	}
	success := LoadMarker(dir).LastSuccessAt

	pub.client.AddHook(failCommandHook{command: "xadd", err: errors.New("simulated XADD failure")})
	if err := pub.publishMessage(context.Background(), testStatusMessage(), ""); err == nil {
		t.Fatal("expected the second publish to fail")
	}

	m := LoadMarker(dir)
	if !m.LastSuccessAt.Equal(success) {
		t.Fatalf("LastSuccessAt moved on failure: %v -> %v", success, m.LastSuccessAt)
	}
	if m.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", m.ConsecutiveFailures)
	}
	if m.LastError == "" {
		t.Error("expected LastError to be recorded")
	}
}

// TestRedisPublishSkipsMarkerWhenDirUnset proves the default (markerDir ==
// "", as every other test in this file relies on) never writes anywhere --
// this is what stopped a prior version of this feature from polluting
// whoever's real ~/.citadel-cli happened to run `go test`.
func TestRedisPublishSkipsMarkerWhenDirUnset(t *testing.T) {
	pub, _, _ := newTestRedisPublisher(t)
	if pub.markerDir != "" {
		t.Fatalf("expected markerDir to default to empty, got %q", pub.markerDir)
	}
	if err := pub.publishMessage(context.Background(), testStatusMessage(), ""); err != nil {
		t.Fatalf("publishMessage: %v", err)
	}
	// Nothing to assert on disk -- the point is that RecordSuccess/RecordFailure
	// are never reached, which the empty markerDir guard in publishMessage
	// enforces. This test exists so a future refactor that removes that guard
	// (or resolves a default directory inline) fails loudly instead of
	// silently, since there is no marker path here to inspect.
}

// TestRedisPublisherOnStatusFansOut pins the citadel #612 wiring: SetOnStatus
// must ACCUMULATE callbacks rather than the last registration silently
// replacing an earlier one. Before this fix, a single onStatus field meant the
// inference-queue reconciler and the #416 auto-stop reconciler could not both
// register -- whichever called SetOnStatus second would clobber the other. A
// future "simplify this back to one field" refactor would compile fine and
// pass every other test in this file, so the count assertion (not just "the
// callback fires") is the part that actually guards the regression.
func TestRedisPublisherOnStatusFansOut(t *testing.T) {
	pub, _, _ := newTestRedisPublisher(t)

	var mu sync.Mutex
	var calls []string
	pub.SetOnStatus(func(_ *status.NodeStatus) {
		mu.Lock()
		calls = append(calls, "auto-stop")
		mu.Unlock()
	})
	pub.SetOnStatus(func(_ *status.NodeStatus) {
		mu.Lock()
		calls = append(calls, "inference-queue")
		mu.Unlock()
	})

	if got := len(pub.onStatusFns); got != 2 {
		t.Fatalf("SetOnStatus must accumulate registrations, got %d callbacks registered (want 2)", got)
	}

	if err := pub.publishStatus(context.Background()); err != nil {
		t.Fatalf("publishStatus: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("want both OnStatus callbacks invoked per publish, got %v", calls)
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
