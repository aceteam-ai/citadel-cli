package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/stream/event"
	"github.com/aceteam-ai/citadel-cli/internal/stream/ops"
)

func syntheticEvent(at time.Duration, action, host, user string) event.Event {
	return event.Event{
		Envelope: event.Envelope{TMonoNS: int64(at), TWallNS: int64(time.Hour + at), SourceID: host, ClockState: "synthetic", ParseStatus: "ok", RawRef: "synthetic:" + host},
		Fields:   map[string]any{"event.action": action, "source.ip": host, "host.name": host, "user.name": user},
		Raw:      "synthetic evidence sentinel " + action,
	}
}

func alertsFor(alerts []Alert, id string) []Alert {
	var out []Alert
	for _, alert := range alerts {
		if alert.RuleID == id {
			out = append(out, alert)
		}
	}
	return out
}

// This calculation intentionally does not call the production groupKey helper:
// stream-alert-v1.md specifies compact canonical JSON, then a public digest.
func expectedPublicGroupKey(t *testing.T, parts ...any) string {
	t.Helper()
	if parts == nil {
		parts = []any{}
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func evaluateEvents(t *testing.T, engine *Engine, events ...event.Event) []Alert {
	t.Helper()
	var out []Alert
	for _, ev := range events {
		alerts, err := engine.Evaluate(ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, alerts...)
	}
	return out
}

func testEngine(t *testing.T, options Options, files ...testRuleFile) *Engine {
	t.Helper()
	e, err := NewEngine(loadTestPack(t, files...), options)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func r5TestRules() []testRuleFile {
	return []testRuleFile{
		{id: "failed", body: testDetection("failure")},
		{id: "success", body: testDetection("success")},
		{id: "failures", body: testCorrelation("event_count", "failed", "source.ip", "    gte: 5\n")},
		{id: "r5", body: testCorrelation("temporal_ordered", "failures, success", "source.ip", "")},
	}
}

func TestR5ChainsFiveFailuresThenSuccessInOneGroup(t *testing.T) {
	e := testEngine(t, Options{}, r5TestRules()...)
	var inputs []event.Event
	for i := 0; i < 5; i++ {
		inputs = append(inputs, syntheticEvent(time.Duration(i)*time.Second, "failure", "192.0.2.1", "demo"))
	}
	inputs = append(inputs, syntheticEvent(5*time.Second, "success", "192.0.2.1", "demo"))
	got := alertsFor(evaluateEvents(t, e, inputs...), "r5")
	if len(got) != 1 || got[0].WindowStartNS != int64(4*time.Second) || got[0].WindowEndNS != int64(5*time.Second) {
		t.Fatalf("R5 chain did not preserve correlation firing time: %+v", got)
	}
}

func TestR5RejectsWrongOrderCrossGroupAndExpiredWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []event.Event
	}{
		{name: "success before failures", events: []event.Event{syntheticEvent(0, "success", "192.0.2.1", "demo"), syntheticEvent(time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(2*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(3*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(4*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(5*time.Second, "failure", "192.0.2.1", "demo")}},
		{name: "wrong source", events: []event.Event{syntheticEvent(0, "failure", "192.0.2.1", "demo"), syntheticEvent(time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(2*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(3*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(4*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(5*time.Second, "success", "192.0.2.2", "demo")}},
		{name: "expired count sequence", events: []event.Event{syntheticEvent(0, "failure", "192.0.2.1", "demo"), syntheticEvent(time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(2*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(3*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(4*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(15*time.Second, "success", "192.0.2.1", "demo")}},
		{name: "expired failures", events: []event.Event{syntheticEvent(0, "failure", "192.0.2.1", "demo"), syntheticEvent(time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(2*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(3*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(11*time.Second, "failure", "192.0.2.1", "demo"), syntheticEvent(12*time.Second, "success", "192.0.2.1", "demo")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := testEngine(t, Options{}, r5TestRules()...)
			if got := alertsFor(evaluateEvents(t, e, tc.events...), "r5"); len(got) != 0 {
				t.Fatalf("false R5: %+v", got)
			}
		})
	}
}

func TestCorrelationKindsCountValuesUnorderedAndOrdered(t *testing.T) {
	files := []testRuleFile{
		{id: "a", body: testDetection("a")}, {id: "b", body: testDetection("b")},
		{id: "count", body: testCorrelation("event_count", "a, b", "source.ip", "    gte: 2\n")},
		{id: "values", body: testCorrelation("value_count", "a, b", "source.ip", "    field: user.name\n    gte: 2\n")},
		{id: "unordered", body: testCorrelation("temporal", "a, b", "source.ip", "")},
		{id: "ordered", body: testCorrelation("temporal_ordered", "a, b", "source.ip", "")},
	}
	e := testEngine(t, Options{}, files...)
	got := evaluateEvents(t, e, syntheticEvent(0, "b", "192.0.2.1", "first"), syntheticEvent(time.Second, "a", "192.0.2.1", "second"))
	for _, id := range []string{"count", "values", "unordered"} {
		if matches := alertsFor(got, id); len(matches) != 1 || matches[0].MatchCount != 2 {
			t.Fatalf("%s incorrect: %+v", id, matches)
		}
	}
	if len(alertsFor(got, "ordered")) != 0 {
		t.Fatal("unordered pair falsely satisfied ordered correlation")
	}
	got = evaluateEvents(t, e, syntheticEvent(2*time.Second, "b", "192.0.2.1", "second"))
	if len(alertsFor(got, "ordered")) != 1 {
		t.Fatal("correctly ordered pair did not match")
	}
	if values := alertsFor(got, "values"); len(values) != 1 || values[0].MatchCount != 2 {
		t.Fatalf("duplicate username inflated distinct values: %+v", values)
	}
}

func TestCountCountsEventsOnceWhenSeveralReferencesMatchOneEvent(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "one", body: testDetection("a")}, testRuleFile{id: "two", body: testDetection("a")},
		testRuleFile{id: "count", body: testCorrelation("event_count", "one, two", "", "    gte: 2\n")},
	)
	if got := alertsFor(evaluateEvents(t, e, syntheticEvent(0, "a", "host", "user")), "count"); len(got) != 0 {
		t.Fatalf("one event double-counted through multiple matching rule references: %+v", got)
	}
	if got := alertsFor(evaluateEvents(t, e, syntheticEvent(time.Second, "a", "host", "user")), "count"); len(got) != 1 || got[0].MatchCount != 2 {
		t.Fatalf("second actual event did not count: %+v", got)
	}
}

func TestCorrelationGroupingUsesDeclaredKeyNotDetectionExtensionKey(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "base", body: testDetection("failure") + "x-aceteam:\n  group_by: [user.name]\n"},
		testRuleFile{id: "count", body: testCorrelation("event_count", "base", "source.ip", "    gte: 2\n")},
	)
	got := evaluateEvents(t, e, syntheticEvent(0, "failure", "192.0.2.1", "one"), syntheticEvent(time.Second, "failure", "192.0.2.2", "one"), syntheticEvent(2*time.Second, "failure", "192.0.2.1", "two"))
	matches := alertsFor(got, "count")
	if len(matches) != 1 || matches[0].GroupKey != expectedPublicGroupKey(t, "source.ip", "192.0.2.1") || matches[0].MatchCount != 2 {
		t.Fatalf("correlation inherited wrong grouping: %+v", matches)
	}
}

func TestEventAndKeyCapsAreVisibleInEngineStats(t *testing.T) {
	e := testEngine(t, Options{Limits: ops.Limits{MaxKeys: 1, MaxEventsPerKey: 2}},
		testRuleFile{id: "base", body: testDetection("failure")},
		testRuleFile{id: "count", body: testCorrelation("event_count", "base", "source.ip", "    gte: 1\n")},
	)
	evaluateEvents(t, e, syntheticEvent(0, "failure", "one", "user"), syntheticEvent(time.Second, "failure", "one", "user"), syntheticEvent(2*time.Second, "failure", "one", "user"), syntheticEvent(3*time.Second, "failure", "two", "user"))
	s := e.Stats()
	if state := s.State["count"]; state.StateEvicted != 2 || state.EventEvictions != 1 || state.KeyEvictions != 1 || state.ActiveKeys != 1 {
		t.Fatalf("bounded state loss invisible: %+v", state)
	}
	s.Matches["count"] = 999
	if e.Stats().Matches["count"] == 999 {
		t.Fatal("stats snapshot exposes mutable engine counters")
	}
}

func TestAlertIdentityAndEvidenceAreDeterministic(t *testing.T) {
	p := loadTestPack(t, testRuleFile{id: "base", body: testDetection("success")})
	a, _ := NewEngine(p, Options{StreamID: "synthetic-stream", DefinitionDigest: "sha256:synthetic"})
	b, _ := NewEngine(p, Options{StreamID: "synthetic-stream", DefinitionDigest: "sha256:synthetic"})
	ev := syntheticEvent(4*time.Second, "success", "192.0.2.1", "demo")
	first := evaluateEvents(t, a, ev)
	second := evaluateEvents(t, b, ev)
	if !reflect.DeepEqual(first, second) || len(first) != 1 {
		t.Fatalf("same input diverged: %+v %+v", first, second)
	}
	publicKey := expectedPublicGroupKey(t)
	if first[0].GroupKey != publicKey {
		t.Fatalf("empty grouping did not use public digest: %+v", first[0])
	}
	identity, _ := json.Marshal([]string{p.Digest, "base", "1", publicKey, strconv.FormatInt(int64(4*time.Second), 10)})
	want := sha256.Sum256(identity)
	if first[0].AlertID != hex.EncodeToString(want[:]) {
		t.Fatalf("alert id violates published identity contract: %+v", first[0])
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), ev.Raw) || strings.Contains(string(encoded), "evidence sentinel") || first[0].Evidence.Mode != "digest" || len(first[0].Evidence.Digests) != 1 {
		t.Fatalf("raw evidence escaped digest-only evaluator: %s", encoded)
	}
}

func TestAlertWireNanosecondsRemainCanonicalStringsBeyondJavaScriptPrecision(t *testing.T) {
	p := loadTestPack(t, testRuleFile{id: "base", body: testDetection("success")})
	e, err := NewEngine(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const at int64 = 9007199254740993 // 2^53+1 cannot survive a JavaScript Number.
	ev := syntheticEvent(0, "success", "logical-source", "demo")
	ev.TMonoNS = at
	ev.TWallNS = math.MaxInt64
	alerts := evaluateEvents(t, e, ev)
	if len(alerts) != 1 {
		t.Fatalf("missing precision fixture alert: %+v", alerts)
	}
	encoded, err := json.Marshal(alerts[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]string{"window_start_ns": "9007199254740993", "window_end_ns": "9007199254740993", "t_mono_ns": "9007199254740993", "t_wall_ns": "9223372036854775807"} {
		if got, ok := wire[key].(string); !ok || got != expected {
			t.Fatalf("%s lost canonical int64 string precision: %#v", key, wire[key])
		}
	}
	identity, err := json.Marshal([]string{p.Digest, "base", "1", expectedPublicGroupKey(t), "9007199254740993"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(identity)
	if alerts[0].AlertID != hex.EncodeToString(digest[:]) {
		t.Fatalf("alert id did not hash the string timestamp: %+v", alerts[0])
	}
}

func TestArbitraryGroupingValuesStayOutOfDigestOnlyAlertJSON(t *testing.T) {
	const privateIP = "192.0.2.239"
	const privateUser = "PRIVATE-GROUP-USER-1352"
	const privateMessage = "PRIVATE-GROUP-MESSAGE-1352"
	e := testEngine(t, Options{}, testRuleFile{id: "private", body: testDetection("success") + "x-aceteam:\n  group_by: [source.ip, user.name, message]\n"})
	ev := syntheticEvent(time.Second, "success", privateIP, privateUser)
	ev.SourceID = "logical-auth-source"
	ev.RawRef = "synthetic-record:1"
	ev.Fields["message"] = privateMessage
	ev.Raw = privateIP + " " + privateUser + " " + privateMessage
	got := evaluateEvents(t, e, ev)
	if len(got) != 1 {
		t.Fatalf("privacy fixture did not match: %+v", got)
	}
	if got[0].GroupKey != expectedPublicGroupKey(t, "source.ip", privateIP, "user.name", privateUser, "message", privateMessage) {
		t.Fatalf("public grouping digest does not cover declared key: %+v", got[0])
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{privateIP, privateUser, privateMessage} {
		if strings.Contains(string(encoded), privateValue) {
			t.Fatalf("grouping value leaked into digest-only alert: %s", encoded)
		}
	}
	if got[0].Evidence.Mode != "digest" || len(got[0].Evidence.Samples) != 0 || got[0].Evidence.Count != 1 {
		t.Fatalf("privacy fixture bypassed digest-only evidence: %+v", got[0].Evidence)
	}
}

func TestWireContractRejectsNegativeEnvelopeClocksInEitherTimeMode(t *testing.T) {
	for _, mode := range []string{"ingest", "event"} {
		for _, field := range []string{"t_mono_ns", "t_wall_ns"} {
			t.Run(mode+"/"+field, func(t *testing.T) {
				e := testEngine(t, Options{TimeMode: mode}, testRuleFile{id: "base", body: testDetection("success")})
				ev := syntheticEvent(0, "success", "logical-source", "demo")
				ev.Fields["@timestamp"] = "1970-01-01T00:00:01Z"
				if field == "t_mono_ns" {
					ev.TMonoNS = -1
				} else {
					ev.TWallNS = -1
				}
				if alerts, err := e.Evaluate(ev); err == nil {
					t.Fatalf("negative %s accepted in %s mode: %+v", field, mode, alerts)
				}
			})
		}
	}
}

func TestDedupRateAndAbsenceAreIndependentBoundedOperators(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "dedup", body: testDetection("a") + "x-aceteam:\n  group_by: [source.ip]\n  dedup: 10s\n"},
		testRuleFile{id: "rate", body: testDetection("a") + "x-aceteam:\n  group_by: [source.ip]\n  rate:\n    capacity: 1\n    every: 10s\n"},
		testRuleFile{id: "absence", body: testDetection("heartbeat") + "x-aceteam:\n  group_by: [source.ip]\n  absence: 10s\n"},
	)
	got := evaluateEvents(t, e, syntheticEvent(0, "a", "one", "user"), syntheticEvent(time.Second, "a", "one", "user"), syntheticEvent(2*time.Second, "heartbeat", "one", "user"))
	if len(alertsFor(got, "dedup")) != 1 || len(alertsFor(got, "rate")) != 1 || len(alertsFor(got, "absence")) != 0 {
		t.Fatalf("suppression semantics wrong: %+v", got)
	}
	if s := e.Stats(); s.Deduplicated != 1 || s.RateLimited != 1 {
		t.Fatalf("suppression invisible: %+v", s)
	}
	more, err := e.Advance(int64(12 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if missing := alertsFor(more, "absence"); len(missing) != 1 || missing[0].WindowStartNS != int64(2*time.Second) || missing[0].WindowEndNS != int64(12*time.Second) {
		t.Fatalf("absence deadline wrong: %+v", missing)
	}
	more, err = e.Advance(int64(13 * time.Second))
	if err != nil || len(alertsFor(more, "absence")) != 0 {
		t.Fatalf("absence repeated without new heartbeat: %+v %v", more, err)
	}
}

func TestAbsenceFeedsCorrelationOnlyWhenTimerActuallyExpires(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "silence", body: testDetection("heartbeat") + "x-aceteam:\n  group_by: [source.ip]\n  absence: 10s\n"},
		testRuleFile{id: "count", body: testCorrelation("event_count", "silence", "source.ip", "    gte: 1\n")},
	)
	if got := alertsFor(evaluateEvents(t, e, syntheticEvent(0, "heartbeat", "one", "user")), "count"); len(got) != 0 {
		t.Fatalf("presence falsely signaled absence correlation: %+v", got)
	}
	got, err := e.Advance(int64(10 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if matches := alertsFor(got, "count"); len(matches) != 1 || matches[0].GroupKey != expectedPublicGroupKey(t, "source.ip", "one") {
		t.Fatalf("actual absence did not reach downstream chain: %+v", got)
	}
}

func TestAbsenceUsesMissingGroupsMetadataNotUnrelatedLatestEvent(t *testing.T) {
	e := testEngine(t, Options{}, testRuleFile{id: "silence", body: testDetection("heartbeat") + "x-aceteam:\n  group_by: [source.ip]\n  absence: 10s\n"})
	evaluateEvents(t, e, syntheticEvent(0, "heartbeat", "one", "one-user"), syntheticEvent(time.Second, "heartbeat", "two", "two-user"))
	got := evaluateEvents(t, e, syntheticEvent(12*time.Second, "unrelated", "three", "third-user"))
	missing := alertsFor(got, "silence")
	if len(missing) != 2 {
		t.Fatalf("expected two distinct missing groups: %+v", missing)
	}
	for _, alert := range missing {
		if (alert.SourceID != "one" && alert.SourceID != "two") || alert.GroupKey != expectedPublicGroupKey(t, "source.ip", alert.SourceID) {
			t.Fatalf("absence attributed to unrelated source: %+v", alert)
		}
		if alert.TMonoNS != alert.WindowEndNS {
			t.Fatalf("absence timestamp is not timer deadline: %+v", alert)
		}
		if alert.Evidence.Count != 0 || len(alert.RawRefs) != 0 {
			t.Fatalf("timer used unrelated raw evidence: %+v", alert)
		}
	}
}

func TestAbsenceEmissionStillHonorsDedupAndRate(t *testing.T) {
	e := testEngine(t, Options{}, testRuleFile{id: "silence", body: testDetection("heartbeat") + "x-aceteam:\n  group_by: [source.ip]\n  absence: 2s\n  dedup: 10s\n  rate:\n    capacity: 1\n    every: 10s\n"})
	var out []Alert
	for _, at := range []time.Duration{0, 3 * time.Second, 11 * time.Second} {
		evaluateEvents(t, e, syntheticEvent(at, "heartbeat", "one", "user"))
		got, err := e.Advance(int64(at + 2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, alertsFor(got, "silence")...)
	}
	if len(out) != 2 || out[0].WindowEndNS != int64(2*time.Second) || out[1].WindowEndNS != int64(13*time.Second) {
		t.Fatalf("absence bypassed suppression: %+v", out)
	}
	if e.Stats().Deduplicated != 1 {
		t.Fatalf("absence suppression not counted: %+v", e.Stats())
	}
}

func TestTumblingCountResetsAtBucketBoundary(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "base", body: testDetection("a")},
		testRuleFile{id: "count", body: testCorrelation("event_count", "base", "source.ip", "    gte: 2\n") + "x-aceteam:\n  window: tumbling\n"},
	)
	got := evaluateEvents(t, e, syntheticEvent(9*time.Second, "a", "one", "user"), syntheticEvent(10*time.Second, "a", "one", "user"), syntheticEvent(11*time.Second, "a", "one", "user"))
	matches := alertsFor(got, "count")
	if len(matches) != 1 || matches[0].WindowStartNS != int64(10*time.Second) || matches[0].MatchCount != 2 {
		t.Fatalf("tumbling count crossed a bucket boundary: %+v", matches)
	}
}

func TestEventTimeReordersWithinLatenessAndCountsTrulyLate(t *testing.T) {
	e := testEngine(t, Options{TimeMode: "event", AllowedLateness: 3 * time.Second, MaxPending: 4}, testRuleFile{id: "base", body: testDetection("a")})
	stamp := func(at time.Duration) event.Event {
		ev := syntheticEvent(at, "a", "one", "user")
		ev.Fields["@timestamp"] = time.Unix(0, int64(at)).UTC().Format(time.RFC3339Nano)
		return ev
	}
	if got := evaluateEvents(t, e, stamp(5*time.Second)); len(got) != 0 {
		t.Fatalf("pending event emitted before watermark: %+v", got)
	}
	got := evaluateEvents(t, e, stamp(3*time.Second), stamp(7*time.Second))
	if len(got) != 1 || got[0].WindowEndNS != int64(3*time.Second) {
		t.Fatalf("out-of-order event not sorted: %+v", got)
	}
	if got := evaluateEvents(t, e, stamp(time.Second)); len(got) != 0 || e.Stats().Late != 1 {
		t.Fatalf("late event not counted: %+v %+v", got, e.Stats())
	}
	more, err := e.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 2 || more[0].WindowEndNS != int64(5*time.Second) || more[1].WindowEndNS != int64(7*time.Second) || e.Stats().Events != 3 {
		t.Fatalf("EOF reorder flush incorrect: %+v %+v", more, e.Stats())
	}
}

func TestEventTimePendingCapIsReportedAndTimestampRangeRefused(t *testing.T) {
	e := testEngine(t, Options{TimeMode: "event", AllowedLateness: time.Hour, MaxPending: 1}, testRuleFile{id: "base", body: testDetection("a")})
	ev := syntheticEvent(0, "a", "one", "user")
	ev.Fields["@timestamp"] = "1970-01-01T00:00:01Z"
	if _, err := e.Evaluate(ev); err != nil {
		t.Fatal(err)
	}
	ev.Fields["@timestamp"] = "1970-01-01T00:00:02Z"
	if _, err := e.Evaluate(ev); err == nil || e.Stats().PendingRejected != 1 {
		t.Fatalf("pending bound not explicit: %v %+v", err, e.Stats())
	}
	for _, stamp := range []string{"1969-12-31T23:59:59Z", "2600-01-01T00:00:00Z", "not-a-time"} {
		fresh := testEngine(t, Options{TimeMode: "event"}, testRuleFile{id: "base", body: testDetection("a")})
		bad := syntheticEvent(0, "a", "one", "user")
		bad.Fields["@timestamp"] = stamp
		if _, err := fresh.Evaluate(bad); err == nil {
			t.Fatalf("unrepresentable timestamp accepted: %s", stamp)
		}
	}
}
