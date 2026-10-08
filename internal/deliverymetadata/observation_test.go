package deliverymetadata

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const testID = "b386d2ec-f65d-4791-a9de-f5b78b09dce3"

var testClaim = base64.RawURLEncoding.EncodeToString([]byte("bounded-claim-test-only-32-bytes!!")[:32])

func dataFixture() string {
	return fmt.Sprintf(`{"jobId":%q,"type":"SHELL_COMMAND","payload":"{\"n\":9007199254740993}","protocol":"durable-results-v1","dispatchId":%q,"requestDigest":%q,"claimToken":%q,"leaseEpoch":"9007199254740991"}`, testID, testID, strings.Repeat("a", 64), testClaim)
}
func httpFixture(data string) []byte {
	return []byte(`{"messages":[{"id":"1-0","data":` + data + `}]}`)
}
func wsFixture(data string) []byte {
	return []byte(`{"type":"job","queue":"jobs:test","id":"1-0","data":` + data + `}`)
}

func TestObservationCaptureExactOwnedAndSource(t *testing.T) {
	for _, transport := range []string{"redis-api", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			raw := httpFixture(dataFixture())
			capture := CaptureHTTP
			if transport == "websocket" {
				raw = wsFixture(dataFixture())
				capture = CaptureWS
			}
			o := capture(raw)
			if o.State() != Observed {
				t.Fatalf("state %s, claim length %d", o.State(), len(testClaim))
			}
			want := [5]string{"durable-results-v1", testID, strings.Repeat("a", 64), testClaim, "9007199254740991"}
			if o.record().values != want {
				t.Fatal("exact values not retained")
			}
			for _, p := range o.record().presence {
				if p != Captured {
					t.Fatal("presence not captured")
				}
			}
			for i := range raw {
				raw[i] = 'x'
			}
			if o.record().values != want {
				t.Fatal("input buffer retained")
			}
			copy := o.Clone()
			if copy == o {
				t.Fatal("wrapper aliases")
			}
			changed := copy.record()
			changed.values[3] = "other"
			copy = freeze(changed)
			if o.record().values != want {
				t.Fatal("clone mutation changes original")
			}
			attached := o.WithSource("requested-queue", "requested-group", transport)
			if attached == o || attached.Identity().JobID != testID || attached.Identity().MessageID != "1-0" || attached.Identity().Type != "SHELL_COMMAND" || attached.Identity().RequestedGroup != "requested-group" {
				t.Fatal("identity/context dropped")
			}
			wantQueue := "requested-queue"
			if transport == "websocket" {
				wantQueue = "jobs:test"
			}
			if attached.Identity().Queue != wantQueue || attached.Identity().Transport != transport {
				t.Fatal("wire queue rebound")
			}
			identity := attached.Identity()
			identity.JobID = "mutation"
			if attached.Identity().JobID != testID {
				t.Fatal("identity is mutable")
			}
		})
	}
}

func TestObservationPassiveClassifications(t *testing.T) {
	fixtures := []struct {
		name, data string
		state      State
		field      string
		presence   Presence
	}{
		{"absent", `{"jobId":"legacy","type":"x"}`, Absent, "protocol", Missing},
		{"protocol-case-exact", strings.Replace(dataFixture(), `"protocol"`, `"Protocol"`, 1), Incomplete, "protocol", Missing},
		{"unsupported", strings.Replace(dataFixture(), "durable-results-v1", "future-v2", 1), Unsupported, "protocol", Captured},
		{"null", strings.Replace(dataFixture(), `"leaseEpoch":"9007199254740991"`, `"leaseEpoch":null`, 1), Incomplete, "leaseEpoch", Null},
		{"numeric", strings.Replace(dataFixture(), `"leaseEpoch":"9007199254740991"`, `"leaseEpoch":1`, 1), Incomplete, "leaseEpoch", WrongType},
		{"boolean", strings.Replace(dataFixture(), `"leaseEpoch":"9007199254740991"`, `"leaseEpoch":true`, 1), Incomplete, "leaseEpoch", WrongType},
		{"encoded-limit", strings.Replace(dataFixture(), `"leaseEpoch":"9007199254740991"`, `"leaseEpoch":"`+strings.Repeat("1", 99)+`"`, 1), Incomplete, "leaseEpoch", EncodedLimit},
		{"decoded-limit", strings.Replace(dataFixture(), `"leaseEpoch":"9007199254740991"`, `"leaseEpoch":"`+strings.Repeat("1", 17)+`"`, 1), Incomplete, "leaseEpoch", Invalid},
		{"unicode-not-ascii", strings.Replace(dataFixture(), `"leaseEpoch":"9007199254740991"`, `"leaseEpoch":"é"`, 1), Incomplete, "leaseEpoch", Invalid},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			o := CaptureHTTP(httpFixture(f.data))
			if o.State() != f.state || o.Presence(f.field) != f.presence {
				t.Fatalf("got %s/%s", o.State(), o.Presence(f.field))
			}
		})
	}
	for _, epoch := range []string{"0", "-1", "01", " 1", "1 ", "1.0", "1e3", "9007199254740992", "9999999999999999"} {
		o := CaptureHTTP(httpFixture(strings.Replace(dataFixture(), "9007199254740991", epoch, 1)))
		if o.State() != Incomplete {
			t.Fatalf("invalid epoch became %s", o.State())
		}
	}
	for _, epoch := range []string{"1", "9007199254740991"} {
		o := CaptureHTTP(httpFixture(strings.Replace(dataFixture(), "9007199254740991", epoch, 1)))
		if o.State() != Observed || o.record().values[4] != epoch {
			t.Fatal("valid epoch was coerced")
		}
	}
	for _, invalid := range []string{strings.Replace(dataFixture(), testID, "00000000-0000-0000-0000-000000000000", -1), strings.Replace(dataFixture(), `"jobId":"`+testID+`"`, `"jobId":"different"`, 1), strings.Replace(dataFixture(), strings.Repeat("a", 64), strings.Repeat("A", 64), 1), strings.Replace(dataFixture(), testClaim, strings.Repeat("?", 43), 1)} {
		if CaptureHTTP(httpFixture(invalid)).State() != Incomplete {
			t.Fatal("invalid lexical identity observed")
		}
	}
	// Syntactically valid digest observation is not payload digest verification.
	if CaptureHTTP(httpFixture(strings.Replace(dataFixture(), strings.Repeat("a", 64), strings.Repeat("b", 64), 1))).State() != Observed {
		t.Fatal("prep introduced digest verification")
	}
	if (*Observation)(nil).State() != Unknown || (*Observation)(nil).Clone() != nil {
		t.Fatal("nil fabricated")
	}
}

func escaped(v string) string {
	var out strings.Builder
	for _, c := range v {
		fmt.Fprintf(&out, `\u%04x`, c)
	}
	return out.String()
}

func TestObservationEscapedNamesValuesAndDuplicates(t *testing.T) {
	data := dataFixture()
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			encoded := strings.Replace(data, `"`+name+`"`, `"`+escaped(name)+`"`, 1)
			o := CaptureHTTP(httpFixture(encoded))
			if o.State() != Observed || o.record().values[i] != CaptureHTTP(httpFixture(data)).record().values[i] {
				t.Fatal("escaped name dropped")
			}
			value := o.record().values[i]
			encoded = strings.Replace(encoded, `"`+value+`"`, `"`+escaped(value)+`"`, 1)
			if CaptureHTTP(httpFixture(encoded)).State() != Observed {
				t.Fatal("fully escaped value rejected")
			}
			duplicate := data[:len(data)-1] + `,"` + escaped(name) + `":"` + value + `"}`
			dupe := CaptureHTTP(httpFixture(duplicate))
			if dupe.State() != Ambiguous || dupe.Presence(name) != Duplicate {
				t.Fatal("same-value escaped duplicate trusted")
			}
		})
	}
}

func TestObservationRepeatedContainersAndIdentity(t *testing.T) {
	a := `[{"id":"1-0","data":` + dataFixture() + `}]`
	b := `[{"id":"2-0","data":` + strings.Replace(dataFixture(), "SHELL_COMMAND", "OTHER", 1) + `}]`
	for _, name := range []string{"messages", "Messages", "meſſageſ", `me\u017f\u017fage\u017f`} {
		for _, pair := range [][2]string{{a, b}, {b, a}, {"null", a}, {a, "null"}} {
			raw := []byte(`{"messages":` + pair[0] + `,"` + name + `":` + pair[1] + `}`)
			if CaptureHTTP(raw).State() != Ambiguous {
				t.Fatal("repeated slice scope trusted")
			}
		}
	}
	for _, raw := range [][]byte{
		[]byte(`{"messages":[{"id":"1-0","data":` + dataFixture() + `,"DATA":{"payload":"{}"}}]}`),
		[]byte(`{"messages":[{"id":"1-0","ID":"2-0","data":` + dataFixture() + `}]}`),
		httpFixture(dataFixture()[:len(dataFixture())-1] + `,"JobID":"other"}`),
		httpFixture(dataFixture()[:len(dataFixture())-1] + `,"TYPE":"other"}`),
		[]byte(`{"type":"job","Type":"job","queue":"q","id":"1-0","data":` + dataFixture() + `}`),
		[]byte(`{"type":"job","queue":"q","Queue":"r","id":"1-0","data":` + dataFixture() + `}`),
	} {
		capture := CaptureHTTP
		if strings.HasPrefix(string(raw), `{"type"`) {
			capture = CaptureWS
		}
		if capture(raw).State() != Ambiguous {
			t.Fatal("repeated container/identity trusted")
		}
	}
	for _, raw := range []string{`{}`, `{"messages":null}`, `{"messages":[]}`, `{"messages":[null]}`, `{"messages":[{"data":null}]}`, `{"messages":[{"data":{}}]}`, `{"messages":`, `null`} {
		if CaptureHTTP([]byte(raw)).State() == Observed {
			t.Fatal("uncertain/absent scope observed")
		}
	}
	// Lookalikes outside the selected scope must not poison unique metadata.
	data := dataFixture()[:len(dataFixture())-1] + `,"unknown":{"claimToken":"other","protocol":"unknown"}}`
	if CaptureHTTP(httpFixture(data)).State() != Observed {
		t.Fatal("nested lookalike captured")
	}
}

func TestObservationCaptureBoundsAndDepth(t *testing.T) {
	small := httpFixture(dataFixture()[:len(dataFixture())-1] + `,"ignored":"x"}`)
	large := httpFixture(dataFixture()[:len(dataFixture())-1] + `,"ignored":"` + strings.Repeat("x", 1<<20) + `"}`)
	measure := func(raw []byte) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range 10 {
			if CaptureHTTP(raw).State() != Observed {
				t.Fatal("unknown skip broke capture")
			}
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	if measure(large) > measure(small)+128*1024 {
		t.Fatal("unknown value allocated a private copy")
	}
	many := dataFixture()[:len(dataFixture())-1] + strings.Repeat(`,"unknown":null`, 10000) + `}`
	if measure(httpFixture(many)) > measure(small)+128*1024 {
		t.Fatal("unknown keys allocate unbounded private strings")
	}
	deep := dataFixture()[:len(dataFixture())-1] + `,"ignored":` + strings.Repeat("[", 1025) + "null" + strings.Repeat("]", 1025) + `}`
	if CaptureHTTP(httpFixture(deep)).State() != Unknown {
		t.Fatal("depth uncertainty trusted")
	}
	unused := `{"messages":[{"id":"1-0","data":` + dataFixture() + `},{"data":{"claimToken":"` + strings.Repeat("x", 1<<20) + `"}}]}`
	if measure([]byte(unused)) > measure(small)+128*1024 {
		t.Fatal("unused messages captured")
	}
	giant := strings.Repeat("x", 1<<20)
	unknownKey := httpFixture(dataFixture()[:len(dataFixture())-1] + `,"` + giant + `":null}`)
	if measure(unknownKey) > measure(small)+128*1024 {
		t.Fatal("giant unknown key allocated a private copy")
	}
	measureAny := func(raw []byte) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range 10 {
			_ = CaptureHTTP(raw)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	for _, selected := range []struct{ name, value string }{{"claimToken", testClaim}, {"protocol", "durable-results-v1"}} {
		raw := httpFixture(strings.Replace(dataFixture(), `"`+selected.name+`":"`+selected.value+`"`, `"`+selected.name+`":"`+giant+`"`, 1))
		if CaptureHTTP(raw).Presence(selected.name) != EncodedLimit {
			t.Fatal("giant selected value not classified before decode")
		}
		if measureAny(raw) > measureAny(small)+128*1024 {
			t.Fatal("selected value was allocated before its encoded cap")
		}
	}
	for _, name := range names {
		data := dataFixture()[:len(dataFixture())-1] + `,"` + name + `":"` + strings.Repeat("x", 1<<20) + `"}`
		if CaptureHTTP(httpFixture(data)).State() != Ambiguous {
			t.Fatal("giant duplicate trusted")
		}
		data = strings.Replace(dataFixture(), `"`+name+`":`, `"`+strings.Repeat("x", 1<<20)+`":`, 1)
		if CaptureHTTP(httpFixture(data)).State() == Observed {
			t.Fatal("giant key fabricated metadata")
		}
	}
}

func TestObservationFormatsAndJSON(t *testing.T) {
	o := CaptureHTTP(httpFixture(dataFixture()))
	formats := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%f", "%x", "%p", "%T", "%z", "%+020.12v", "%#020.12f"}
	for verb := '!'; verb <= '~'; verb++ {
		formats = append(formats, "%"+string(verb), "%#+020.12"+string(verb))
	}
	parents := []any{*o, o, struct{ observation *Observation }{o}, struct{ observation Observation }{*o}, []*Observation{o}, []Observation{*o}, map[string]*Observation{"entry": o}, map[string]Observation{"entry": *o}, struct{ observation *Observation }{nil}, (*Observation)(nil), Observation{}, &Observation{}}
	for _, format := range formats {
		for index, parent := range parents {
			out := fmt.Sprintf(format, parent)
			for _, secret := range []string{testClaim, base64.StdEncoding.EncodeToString([]byte(testClaim)), testID} {
				if strings.Contains(out, secret) {
					t.Fatalf("format %s parent %d leaked: %s", format, index, out)
				}
			}
		}
	}
	wire, err := json.Marshal(o)
	if err != nil || string(wire) != "{}" {
		t.Fatal("opaque observation serialized")
	}
	zero := &Observation{}
	if zero.State() != Unknown || zero.Provenance() != Uncertain || zero.Presence("protocol") != Missing || zero.WithSource("q", "g", "redis-api").State() != Unknown {
		t.Fatal("zero value fabricated authority")
	}
	if zero.Identity() != (Identity{}) || (*Observation)(nil).WithSource("q", "g", "redis-api") != nil {
		t.Fatal("nil/zero identity fabricated")
	}
	shape := reflect.TypeFor[Observation]()
	if shape.NumField() != 1 || shape.Field(0).Name != "snapshot" || shape.Field(0).PkgPath == "" || shape.Field(0).Type != reflect.TypeFor[func() ownedRecord]() {
		t.Fatal("reflect-safe function-only layout changed")
	}
	if o.record().values != o.Clone().record().values {
		t.Fatal("shared immutable record changed")
	}
	attached := o.WithSource("new-queue", "new-group", "redis-api")
	if o.Identity().Queue != "" || o.Identity().RequestedGroup != "" || attached.Identity().Queue != "new-queue" {
		t.Fatal("source attachment mutated shared record")
	}
	if (&Observation{}).Clone().State() != Unknown {
		t.Fatal("zero clone fabricated")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%f", "%z", "%#020.12f"} {
		if fmt.Sprintf(format, *o) != redacted || fmt.Sprintf(format, o) != redacted {
			t.Fatal("static all-verb formatter contract changed")
		}
	}
}
