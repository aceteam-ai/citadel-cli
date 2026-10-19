package rules

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestDeduplicatedDetectionDoesNotFeedParentCorrelation(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "a", body: testDetection("start")+"x-aceteam:\n  group_by: [source.ip]\n  dedup: 10s\n"},
		testRuleFile{id: "parent", body: testCorrelation("event_count", "a", "source.ip", "    gte: 2\n")},
	)
	got := evaluateEvents(t, e,
		syntheticEvent(0, "start", "192.0.2.1", "demo"),
		syntheticEvent(time.Second, "start", "192.0.2.1", "demo"),
	)
	if len(alertsFor(got, "a")) != 1 || len(alertsFor(got, "parent")) != 0 || e.Stats().Deduplicated != 1 {
		t.Fatalf("deduplicated event became a second parent observation: alerts=%+v stats=%+v", got, e.Stats())
	}
}

func TestRateLimitedEmissionStillFeedsParentDetection(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "a", body: testDetection("start")+"x-aceteam:\n  group_by: [source.ip]\n  rate:\n    capacity: 1\n    every: 10s\n"},
		testRuleFile{id: "parent", body: testCorrelation("event_count", "a", "source.ip", "    gte: 2\n")},
	)
	got := evaluateEvents(t, e,
		syntheticEvent(0, "start", "192.0.2.1", "demo"),
		syntheticEvent(time.Second, "start", "192.0.2.1", "demo"),
	)
	parents := alertsFor(got, "parent")
	if len(alertsFor(got, "a")) != 1 || e.Stats().RateLimited != 1 || len(parents) != 1 || parents[0].MatchCount != 2 {
		t.Fatalf("emission throttle incorrectly changed detection truth: alerts=%+v stats=%+v", got, e.Stats())
	}
}

func compositionValueRules() []testRuleFile {
	return []testRuleFile{
		{id: "input", body: testDetection("sample")},
		{id: "values", body: testCorrelation("value_count", "input", "source.ip", "    field: user.name\n    gte: 1\n")},
		{id: "success", body: testDetection("success")},
	}
}

func compositionPopulateDistinct(t *testing.T, e *Engine) []Alert {
	t.Helper()
	var last []Alert
	for i := 0; i < 1100; i++ {
		last = evaluateEvents(t, e, syntheticEvent(time.Duration(i)*time.Millisecond, "sample", "192.0.2.1", fmt.Sprintf("synthetic-user-%04d", i)))
	}
	values := alertsFor(last, "values")
	if len(values) != 1 || !values[0].Approximate {
		t.Fatalf("distinct input never qualified through the real HLL path: %+v", values)
	}
	return last
}

func TestOrderedCorrelationRetainsEarlierApproximateQualification(t *testing.T) {
	files := compositionValueRules()
	files = append(files, testRuleFile{id: "ordered", body: testCorrelation("temporal_ordered", "values, success", "source.ip", "")})
	e := testEngine(t, Options{}, files...)
	compositionPopulateDistinct(t, e)
	got := evaluateEvents(t, e, syntheticEvent(2*time.Second, "success", "192.0.2.1", "demo"))
	if len(alertsFor(got, "values")) != 0 { t.Fatal("success must not manufacture a current distinct-count qualification") }
	completed := alertsFor(got, "ordered")
	if len(completed) != 1 || !completed[0].Approximate || completed[0].MatchCount != 2 {
		t.Fatalf("exact final success erased earlier HLL uncertainty: %+v", completed)
	}
}

func TestDownstreamCountRetainsUncertaintyAfterCurrentSourceReturnsExact(t *testing.T) {
	files := compositionValueRules()
	files = append(files, testRuleFile{id: "parent", body: testCorrelation("event_count", "values", "source.ip", "    gte: 1\n")})
	e := testEngine(t, Options{}, files...)
	compositionPopulateDistinct(t, e)
	// At 10.5 s, the source's retained ten-second window has only 600
	// distinct inputs, so it is exact again. The parent still holds the
	// approximate qualifications emitted around 1.024 to 1.099 s.
	got := evaluateEvents(t, e, syntheticEvent(10500*time.Millisecond, "sample", "192.0.2.1", "synthetic-new-user"))
	values, parents := alertsFor(got, "values"), alertsFor(got, "parent")
	if len(values) != 1 || values[0].Approximate || values[0].MatchCount != 600 {
		t.Fatalf("current source window did not return to an exact 600-value set: %+v", values)
	}
	if len(parents) != 1 || !parents[0].Approximate || parents[0].MatchCount != 600 {
		t.Fatalf("exact current input erased uncertainty retained in the parent window: %+v", parents)
	}
	if e.Stats().State["parent"].ApproximateTransitions != 0 {
		t.Fatal("upstream uncertainty was misreported as the parent's own distinct-sketch conversion")
	}
}

func TestNumericSpellingsShareGroupsAndDistinctIdentityWithoutStringCoercion(t *testing.T) {
	e := testEngine(t, Options{},
		testRuleFile{id: "input", body: testDetection("sample")},
		testRuleFile{id: "grouped", body: testCorrelation("event_count", "input", "identity.value", "    gte: 1\n")},
		testRuleFile{id: "distinct", body: testCorrelation("value_count", "input", "source.ip", "    field: identity.value\n    gte: 1\n")},
	)
	values := []any{int(1), json.Number("1.0"), json.Number("1e0"), "1"}
	var numericKey string
	for i, value := range values {
		ev := syntheticEvent(time.Duration(i)*time.Second, "sample", "192.0.2.1", "demo")
		ev.Fields["identity.value"] = value
		got := evaluateEvents(t, e, ev)
		groups, distinct := alertsFor(got, "grouped"), alertsFor(got, "distinct")
		if len(groups) != 1 || len(distinct) != 1 { t.Fatalf("missing numeric composition matches: %+v", got) }
		if i < 3 {
			if i == 0 { numericKey = groups[0].GroupKey }
			if groups[0].GroupKey != numericKey || groups[0].MatchCount != uint64(i+1) || distinct[0].MatchCount != 1 {
				t.Fatalf("numeric spelling %v changed grouping or distinct identity: groups=%+v distinct=%+v", value, groups, distinct)
			}
		} else if groups[0].GroupKey == numericKey || groups[0].MatchCount != 1 || distinct[0].MatchCount != 2 {
			t.Fatalf("numeric-looking string aliased a typed number: groups=%+v distinct=%+v", groups, distinct)
		}
	}
	if numericKey != expectedPublicGroupKey(t, "identity.value", map[string]string{"$number": "1"}) {
		t.Fatal("numeric grouping did not follow the published tagged reduced-rational representation")
	}
}

func TestIdleWatermarkAndUnrelatedEventsExpireAllNonAbsenceState(t *testing.T) {
	for _, driver := range []string{"advance", "unrelated event"} {
		t.Run(driver, func(t *testing.T) {
			e := testEngine(t, Options{},
				testRuleFile{id: "start", body: testDetection("start")+"x-aceteam:\n  group_by: [source.ip]\n  dedup: 10s\n  rate:\n    capacity: 1\n    every: 10s\n"},
				testRuleFile{id: "finish", body: testDetection("finish")},
				testRuleFile{id: "count", body: testCorrelation("event_count", "start", "source.ip", "    gte: 2\n")},
				testRuleFile{id: "unfinished", body: testCorrelation("temporal_ordered", "start, finish", "source.ip", "")},
			)
			evaluateEvents(t, e, syntheticEvent(0, "start", "192.0.2.1", "demo"))
			before := e.Stats()
			if before.State["start"].ActiveKeys != 2 || before.State["count"].ActiveKeys != 1 || before.State["unfinished"].ActiveKeys != 1 {
				t.Fatalf("test did not populate dedup, rate, count and sequence state: %+v", before.State)
			}
			var out []Alert
			if driver == "advance" {
				var err error
				out, err = e.Advance(int64(20*time.Second))
				if err != nil { t.Fatal(err) }
			} else {
				out = evaluateEvents(t, e, syntheticEvent(20*time.Second, "unrelated", "192.0.2.1", "demo"))
			}
			if len(out) != 0 { t.Fatalf("expiry manufactured matches: %+v", out) }
			for rule, state := range e.Stats().State {
				if state.ActiveKeys != 0 || state.StateEvicted != 0 || state.InputRejected != 0 {
					t.Fatalf("%s retained naturally expired state or counted expiry as loss: %+v", rule, state)
				}
			}
		})
	}
}

