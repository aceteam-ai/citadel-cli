package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/stream/event"
)

const shieldReplayEpochNS int64 = 1767225600000000000 // Same fixed 2026 UTC epoch as the offline driver.

type shieldFixture struct {
	name string
	data []byte
	digest string
}

type shieldVerdicts struct {
	Flagged map[string][]string `json:"flagged"`
	StatOnly []string `json:"stat_only"`
}

type shieldSnapshot struct {
	Name string `json:"name"`
	InputDigest string `json:"input_digest"`
	Alerts []Alert `json:"alerts"`
	Stats EngineStats `json:"stats"`
}

type shieldCorpusSnapshot struct {
	PackDigest string `json:"pack_digest"`
	InputDigest string `json:"input_digest"`
	TimeMode string `json:"time_mode"`
	Scenarios []shieldSnapshot `json:"scenarios"`
}

// These are the actual seven pre-existing prototype inputs, not regenerated
// lookalikes. Hashes are recorded independently in shield/PROVENANCE.md.
func shieldFixtures(tb testing.TB) ([]shieldFixture, map[string]shieldVerdicts) {
	tb.Helper()
	sources := []struct { name, digest string }{
		{"a_clean", "c7ddc9b3d510b292f14eeffd0524737cb694be2c4f4afb9e6dbeceb6ee67c71b"},
		{"b_sqli_knownbad", "13b565d8ad093aa12d6e5375b453e4557c8946ed55ee526293c5d2d8b9519533"},
		{"c_ambiguous_crawler", "38aabe9b55a4efd0d1329d24350c2610857e17b8cfaaec1ec34b9e15238bde3b"},
		{"d_repeat_credstuffing", "e3e07c4bcccbf5ff6573559c2690a268fab768c5a5e43976ad4b5dbca367440f"},
		{"e_fail_then_success", "2d4b9099965c560e117e8115c5700eea50a7206ef84a053895b4ab85d30e9066"},
		{"e2_split_ip_fail_then_success", "5e03df6c856113175305e4fef54bfcc82ad5f9ad29dc7f517e8dba8dcddbb4d5"},
		{"f_multikey_match", "271a9ee1ca05c312f140e1aea887d6496c01e00ba4c80fb5e6a07f3b21afeb88"},
	}
	root, err := os.OpenRoot(filepath.Join("..", "testdata", "shield"))
	if err != nil { tb.Fatal(err) }
	defer root.Close()
	fixtures := make([]shieldFixture, 0, len(sources))
	totalBytes, totalLines := 0, 0
	for _, source := range sources {
		data, err := readBounded(root, source.name+".log", 64*1024)
		if err != nil { tb.Fatal(err) }
		digest := sha256.Sum256(data)
		if len(data) == 0 || data[len(data)-1] != '\n' || hex.EncodeToString(digest[:]) != source.digest {
			tb.Fatalf("%s differs from its original prototype bytes or final newline", source.name)
		}
		totalBytes += len(data)
		totalLines += bytes.Count(data, []byte{'\n'})
		fixtures = append(fixtures, shieldFixture{source.name, data, "sha256:"+source.digest})
	}
	if totalBytes != 21792 || totalLines != 147 {
		tb.Fatalf("unexpected Shield corpus bounds: %d bytes, %d lines", totalBytes, totalLines)
	}
	labels, err := readBounded(root, "expected.json", 64*1024)
	if err != nil { tb.Fatal(err) }
	var expected map[string]shieldVerdicts
	if err := json.Unmarshal(labels, &expected); err != nil { tb.Fatal(err) }
	if len(expected) != len(fixtures) { tb.Fatal("golden label count differs from fixture count") }
	for _, fixture := range fixtures {
		if labels, ok := expected[fixture.name]; !ok || labels.Flagged == nil || labels.StatOnly == nil {
			tb.Fatalf("missing or malformed original labels for %s", fixture.name)
		}
	}
	return fixtures, expected
}

func shieldPack(tb testing.TB) *Pack {
	tb.Helper()
	pack, err := LoadPack(filepath.Join("..", "testdata", "pack.yml"))
	if err != nil { tb.Fatal(err) }
	if pack.Manifest.Parser != "combined" || len(pack.Refused) != 0 {
		tb.Fatal("Shield golden test requires the complete shared combined-log pack")
	}
	return pack
}

// The same evaluator used by stream serve/eval consumes all real fixture
// rows. Only the replay driver is synthetic: fixed epoch, 1 ms per record,
// and basename:byte-offset references. No machine clock or network is read.
func shieldEvaluate(tb testing.TB, pack *Pack, fixture shieldFixture, mode string) (shieldSnapshot, map[string]string) {
	tb.Helper()
	engine, err := NewEngine(pack, Options{TimeMode: mode, AllowedLateness: 30*time.Second})
	if err != nil { tb.Fatal(err) }
	snapshot := shieldSnapshot{Name: fixture.name, InputDigest: fixture.digest, Alerts: []Alert{}}
	actors := make(map[string]string)
	filename := fixture.name+".log"
	var offset uint64
	var count uint64
	for _, line := range bytes.SplitAfter(fixture.data, []byte{'\n'}) {
		if len(line) == 0 { continue }
		count++
		if len(line) > event.DefaultMaxLineBytes { tb.Fatal("fixture line exceeds parser limit") }
		stamp := int64(count)*int64(time.Millisecond)
		meta := event.Envelope{
			Seq: count,
			TMonoNS: stamp,
			TWallNS: shieldReplayEpochNS+stamp,
			ClockState: "offline_replay",
			SourceID: filename,
			NodeID: "offline-eval",
			RawRef: filename+":"+strconv.FormatUint(offset, 10),
		}
		parsed, err := event.Parse(pack.Manifest.Parser, string(line), meta, pack.ParserOptions())
		if err != nil { tb.Fatalf("parse %s record %d: %v", fixture.name, count, err) }
		actor, ok := parsed.Fields["source.ip"].(string)
		if !ok || actor == "" { tb.Fatal("fixture parser did not retain its source IP") }
		actors[meta.RawRef] = actor
		alerts, err := engine.Evaluate(parsed)
		if err != nil { tb.Fatalf("evaluate %s record %d: %v", fixture.name, count, err) }
		snapshot.Alerts = append(snapshot.Alerts, alerts...)
		offset += uint64(len(line))
	}
	flushed, err := engine.Flush()
	if err != nil { tb.Fatal(err) }
	snapshot.Alerts = append(snapshot.Alerts, flushed...)
	SortAlerts(snapshot.Alerts)
	snapshot.Stats = engine.Stats()
	if snapshot.Stats.Events != count || snapshot.Stats.Late != 0 || snapshot.Stats.PendingRejected != 0 || snapshot.Stats.StateRejected != 0 {
		tb.Fatalf("fixture evaluation lost events or rejected state: %+v", snapshot.Stats)
	}
	for rule, state := range snapshot.Stats.State {
		if state.StateEvicted != 0 || state.InputRejected != 0 {
			tb.Fatalf("fixture %s evicted or rejected %s state: %+v", fixture.name, rule, state)
		}
	}
	return snapshot, actors
}

// GroupKey is intentionally opaque. Resolve the local final-event reference
// instead of leaking grouping values into alert JSON or reversing a hash.
// stat_only is a final entity verdict, so malicious matches take precedence.
func shieldProject(tb testing.TB, snapshot shieldSnapshot, actors map[string]string) shieldVerdicts {
	tb.Helper()
	flags := make(map[string]map[string]bool)
	statistical := make(map[string]bool)
	for _, alert := range snapshot.Alerts {
		if len(alert.RawRefs) != 1 { tb.Fatalf("unexpected fixture match references: %+v", alert.RawRefs) }
		actor, ok := actors[alert.RawRefs[0]]
		if !ok { tb.Fatal("match references an event outside its fixture") }
		if !strings.HasPrefix(alert.GroupKey, "sha256:") || len(alert.GroupKey) != len("sha256:")+64 {
			tb.Fatal("match exposes a non-digest group key")
		}
		switch alert.RuleID {
		case "R1", "R2", "R3", "R4", "R5":
			if !alert.Escalate { tb.Fatalf("malicious rule %s unexpectedly became a local-only match", alert.RuleID) }
			if flags[actor] == nil { flags[actor] = make(map[string]bool) }
			flags[actor][alert.RuleID] = true
		case "stat_only":
			if alert.Escalate { tb.Fatal("stat-only match must remain local-only") }
			statistical[actor] = true
		default:
			if !strings.HasPrefix(alert.RuleID, "_") || alert.Escalate {
				tb.Fatalf("unexpected public or escalating internal fixture rule %s", alert.RuleID)
			}
		}
	}
	projected := shieldVerdicts{Flagged: make(map[string][]string), StatOnly: []string{}}
	for actor, rules := range flags {
		ids := make([]string, 0, len(rules))
		for id := range rules { ids = append(ids, id) }
		sort.Strings(ids)
		projected.Flagged[actor] = ids
	}
	for actor := range statistical {
		if len(flags[actor]) == 0 { projected.StatOnly = append(projected.StatOnly, actor) }
	}
	sort.Strings(projected.StatOnly)
	return projected
}

func shieldInputDigest(packDigest string, fixtures []shieldFixture) string {
	hash := sha256.New()
	// One selected pack, then the original named input byte frames, not just
	// a concatenation whose boundaries could alias a different input corpus.
	fmt.Fprintf(hash, "%d:", len(packDigest))
	_, _ = hash.Write([]byte(packDigest))
	for _, fixture := range fixtures {
		fmt.Fprintf(hash, "%d:", len(fixture.name))
		_, _ = hash.Write([]byte(fixture.name))
		fmt.Fprintf(hash, "%d:", len(fixture.data))
		_, _ = hash.Write(fixture.data)
	}
	return "sha256:"+hex.EncodeToString(hash.Sum(nil))
}

func TestShieldOriginalGoldenFixturesBothTimeModes(t *testing.T) {
	fixtures, expected := shieldFixtures(t)
	pack := shieldPack(t)
	inputDigest := shieldInputDigest(pack.Digest, fixtures)
	for _, mode := range []string{"ingest", "event"} {
		t.Run(mode, func(t *testing.T) {
			var firstJSON []byte
			var firstDigest [32]byte
			for repetition := 0; repetition < 2; repetition++ {
				corpus := shieldCorpusSnapshot{PackDigest: pack.Digest, InputDigest: inputDigest, TimeMode: mode, Scenarios: []shieldSnapshot{}}
				for _, fixture := range fixtures {
					snapshot, actors := shieldEvaluate(t, pack, fixture, mode)
					got := shieldProject(t, snapshot, actors)
					if !reflect.DeepEqual(got, expected[fixture.name]) {
						t.Fatalf("%s original Shield flags differ: got %+v, want %+v", fixture.name, got, expected[fixture.name])
					}
					corpus.Scenarios = append(corpus.Scenarios, snapshot)
				}
				encoded, err := json.Marshal(corpus)
				if err != nil { t.Fatal(err) }
				digest := sha256.Sum256(encoded)
				if repetition == 0 {
					firstJSON, firstDigest = encoded, digest
				} else if !bytes.Equal(firstJSON, encoded) || firstDigest != digest {
					t.Fatal("same pack and input byte frames produced different exact JSON or output digests")
				}
			}
		})
	}
}

var shieldBenchmarkMatches uint64

// Measures the actual combined parser and complete Shield rule/correlation
// pack over all 147 original events. Each source scenario starts a fresh
// engine, including in every benchmark iteration. Loading the reviewed pack
// and reading/hash-checking immutable fixture files are outside the timer;
// parsing, rule evaluation, EOF flushing, output sorting and stats are inside.
// No throughput result is implied merely by the existence of this benchmark.
func BenchmarkShieldEvaluatorCorpus(b *testing.B) {
	fixtures, _ := shieldFixtures(b)
	pack := shieldPack(b)
	var totalBytes int64
	for _, fixture := range fixtures { totalBytes += int64(len(fixture.data)) }
	for _, mode := range []string{"ingest", "event"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(totalBytes)
			b.ResetTimer()
			var matches uint64
			for iteration := 0; iteration < b.N; iteration++ {
				for _, fixture := range fixtures {
					snapshot, _ := shieldEvaluate(b, pack, fixture, mode)
					matches += uint64(len(snapshot.Alerts))
				}
			}
			b.StopTimer()
			shieldBenchmarkMatches = matches
			b.ReportMetric(147, "events/op")
			if elapsed := b.Elapsed().Seconds(); elapsed > 0 {
				b.ReportMetric(147*float64(b.N)/elapsed, "events/s")
			}
		})
	}
}
