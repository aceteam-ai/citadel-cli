package event

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testEnvelope() Envelope {
	return Envelope{
		Seq: 7,
		TMonoNS: 1234567,
		TWallNS: time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC).UnixNano(),
		ClockState: "synced",
		SourceID: "fixture",
		NodeID: "offline-node",
		RawRef: "fixture:42",
	}
}

func TestCombinedPreservesEvidenceAndExtractsFields(t *testing.T) {
	raw := `203.0.113.9 - alice [11/Oct/2026:10:03:20 +0000] "GET /Products?id=1'%20OR%20'1'='1 HTTP/1.1" 200 300 "-" "sqlmap/1.7"` + "\r\n"
	meta := testEnvelope()
	got, err := Parse("combined", raw, meta, ParserOptions{Enrichments: []string{"source.ip_subnet24"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Raw != raw || got.Seq != meta.Seq || got.TMonoNS != meta.TMonoNS || got.TWallNS != meta.TWallNS || got.RawRef != meta.RawRef || got.ParseStatus != ParseStatusOK {
		t.Fatalf("evidence or ingest envelope changed: %+v", got.Envelope)
	}
	want := map[string]any{
		"@timestamp": "2026-10-11T10:03:20Z",
		"source.ip": "203.0.113.9",
		"source.ip_subnet24": "203.0.113.0/24",
		"http.request.method": "GET",
		"http.request.line": "GET /Products?id=1'%20OR%20'1'='1 HTTP/1.1",
		"url.original": "/Products?id=1'%20OR%20'1'='1",
		"url.path": "/Products",
		"http.response.status_code": int64(200),
		"network.bytes": int64(300),
		"user.name": "alice",
		"user_agent.original": "sqlmap/1.7",
		"message": strings.TrimSuffix(raw, "\r\n"),
	}
	for key, value := range want {
		if !reflect.DeepEqual(got.Fields[key], value) {
			t.Errorf("%s: got %#v, want %#v", key, got.Fields[key], value)
		}
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"Raw"`) || strings.Contains(string(encoded), `"raw"`) || !strings.Contains(string(encoded), `"t_mono_ns":1234567`) {
		t.Fatal("event JSON lost envelope naming or exposed Raw")
	}
}

func TestCombinedQuotedEscapesIPv6AndNoBytes(t *testing.T) {
	raw := `2001:db8::1 - - [19/Oct/2026:12:30:00 +0230] "POST /login HTTP/2.0" 401 - "https://example.test/" "agent\"quoted\x20name"`
	got, err := Parse("combined", raw, testEnvelope(), ParserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fields["source.ip"] != "2001:db8::1" || got.Fields["@timestamp"] != "2026-10-19T10:00:00Z" || got.Fields["user_agent.original"] != `agent"quoted name` {
		t.Fatalf("wrong combined normalization: %#v", got.Fields)
	}
	if _, found := got.Fields["network.bytes"]; found {
		t.Fatal("unknown byte count must not become zero")
	}
	if _, err := Parse("combined", raw, testEnvelope(), ParserOptions{Enrichments: []string{"source.ip_subnet24"}}); err == nil {
		t.Fatal("IPv6 must not be mislabeled as an IPv4 /24")
	}
}

func TestSyslog3164DerivesYearFromIngest(t *testing.T) {
	meta := testEnvelope()
	meta.TWallNS = time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC).UnixNano()
	raw := "<34>Dec 31 23:59:59 host sshd[123]: Failed password for jason from 192.0.2.1 port 2222 ssh2"
	got, err := Parse("syslog3164", raw, meta, ParserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fields["@timestamp"] != "2025-12-31T23:59:59Z" || got.Fields["host.name"] != "host" || got.Fields["process.name"] != "sshd" || got.Fields["process.pid"] != int64(123) || got.Fields["log.syslog.priority"] != int64(34) {
		t.Fatalf("incorrect RFC3164 fields: %#v", got.Fields)
	}
	meta.TWallNS = 0
	if _, err := Parse("syslog3164", raw, meta, ParserOptions{}); err == nil {
		t.Fatal("missing syslog year must not read the process clock")
	}
}

func TestSyslog5424StructuredDataAndNilFields(t *testing.T) {
	raw := `<165>1 2026-10-19T16:00:00.123+01:00 host app 123 ID47 [example@32473 a="value\]escaped" b="quoted\"value"][other x="y"] ` + "\ufeff" + "original message"
	got, err := Parse("syslog", raw, testEnvelope(), ParserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fields["@timestamp"] != "2026-10-19T15:00:00.123Z" || got.Fields["message"] != "original message" || got.Fields["log.syslog.msgid"] != "ID47" || got.Fields["process.pid"] != int64(123) {
		t.Fatalf("wrong RFC5424 event: %#v", got.Fields)
	}
	got, err = Parse("syslog5424", "<1>1 - - - - - -", testEnvelope(), ParserOptions{})
	if err != nil || got.Fields["message"] != "" {
		t.Fatalf("nil RFC5424 fields: %#v %v", got.Fields, err)
	}
	if _, exists := got.Fields["@timestamp"]; exists {
		t.Fatal("unknown sender timestamp must stay absent")
	}
}

func TestOpenSSHAcceptedFailedAndInvalidUsers(t *testing.T) {
	cases := []struct {
		line string
		user string
		outcome string
		action string
	}{
		{"Oct 19 14:00:00 host sshd[123]: Accepted publickey for alice from 192.0.2.5 port 2222 ssh2: ED25519 SHA256:example", "alice", "success", "ssh_login"},
		{"2026-10-19T14:00:00+00:00 host sshd-session[124]: Failed password for invalid user bob from 192.0.2.6 port 2223 ssh2", "bob", "failure", "ssh_login"},
		{"Invalid user carol from 2001:db8::5 port 2224 [preauth]", "carol", "failure", "ssh_invalid_user"},
		{"sshd[3]: Failed password for dave from 192.0.2.7 port 2225 ssh2", "dave", "failure", "ssh_login"},
		{"Accepted keyboard-interactive/pam for erin from 192.0.2.8 port 2226 ssh2", "erin", "success", "ssh_login"},
	}
	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			got, err := Parse("openssh", tc.line, testEnvelope(), ParserOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Fields["user.name"] != tc.user || got.Fields["event.outcome"] != tc.outcome || got.Fields["event.action"] != tc.action || got.Fields["event.category"] != "authentication" {
				t.Fatalf("wrong SSH event: %#v", got.Fields)
			}
		})
	}
}

func TestJSONDeclaredMappingAndTypedFields(t *testing.T) {
	raw := `{"client":{"ip":"192.0.2.44"},"status":401,"stamp":"2026-10-19T16:00:00+01:00","text":"original","labels":["one","two"]}`
	options := ParserOptions{
		FieldMap: map[string]string{"client.ip": "source.ip", "status": "http.response.status_code", "stamp": "@timestamp", "text": "message", "labels": "labels"},
		Enrichments: []string{"source.ip_subnet24"},
	}
	got, err := Parse("jsonl", raw, testEnvelope(), options)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fields["source.ip_subnet24"] != "192.0.2.0/24" || got.Fields["http.response.status_code"] != int64(401) || got.Fields["@timestamp"] != "2026-10-19T15:00:00Z" || got.Fields["message"] != "original" || got.Raw != raw {
		t.Fatalf("wrong JSON mapping: %#v", got.Fields)
	}
	if !reflect.DeepEqual(got.Fields["labels"], []any{"one", "two"}) {
		t.Fatal("scalar arrays were not preserved")
	}
	got, err = Parse("json", `{"a.b":"literal","a":{"b":"nested"}}`, testEnvelope(), ParserOptions{FieldMap: map[string]string{"a.b": "message"}})
	if err != nil || got.Fields["message"] != "literal" {
		t.Fatal("literal dotted key must take documented precedence")
	}
}

func TestParsersRefuseMalformedInputWithoutLeakingEvidence(t *testing.T) {
	jsonMap := ParserOptions{FieldMap: map[string]string{"secret": "message"}}
	cases := []struct {
		name string
		kind string
		line string
		options ParserOptions
	}{
		{"unknown parser", "shell", "sensitive-evidence", ParserOptions{}},
		{"multiline", "combined", "sensitive-evidence\nsecond", ParserOptions{}},
		{"bad UTF8", "combined", "sensitive-evidence\xff", ParserOptions{}},
		{"unknown enrichment", "json", `{"secret":"sensitive-evidence"}`, ParserOptions{FieldMap: jsonMap.FieldMap, Enrichments: []string{"network.lookup"}}},
		{"duplicate enrichment", "json", `{"secret":"sensitive-evidence"}`, ParserOptions{FieldMap: jsonMap.FieldMap, Enrichments: []string{"source.ip_subnet24", "source.ip_subnet24"}}},
		{"missing map", "json", `{"secret":"sensitive-evidence"}`, ParserOptions{}},
		{"duplicate key", "json", `{"secret":"sensitive-evidence","secret":"last"}`, jsonMap},
		{"escaped duplicate key", "json", `{"secret":"sensitive-evidence","\u0073ecret":"last"}`, jsonMap},
		{"unmapped nested duplicate", "json", `{"secret":"sensitive-evidence","other":{"x":1,"x":2}}`, jsonMap},
		{"duplicate in unmapped array", "json", `{"secret":"sensitive-evidence","other":[{"x":1,"x":2}]}`, jsonMap},
		{"trailing JSON", "json", `{"secret":"sensitive-evidence"} {}`, jsonMap},
		{"missing JSON input", "json", `{"other":"sensitive-evidence"}`, jsonMap},
		{"object JSON value", "json", `{"secret":{"value":"sensitive-evidence"}}`, jsonMap},
		{"deep JSON", "json", `{"secret":` + strings.Repeat("[", 18) + `"sensitive-evidence"` + strings.Repeat("]", 18) + `}`, jsonMap},
		{"overflow integer", "json", `{"secret":99999999999999999999999}`, jsonMap},
		{"excessive exponent", "json", `{"secret":1e1025}`, jsonMap},
		{"map conflict", "json", `{"secret":"sensitive-evidence","other":"x"}`, ParserOptions{FieldMap: map[string]string{"secret": "message", "other": "message"}}},
		{"map on syslog", "syslog", "sensitive-evidence", jsonMap},
		{"invalid priority", "syslog", "<192>Oct 19 10:00:00 host app: sensitive-evidence", ParserOptions{}},
		{"invalid month", "syslog3164", "Zzz 19 10:00:00 host app: sensitive-evidence", ParserOptions{}},
		{"malformed structured data", "syslog5424", `<1>1 - host app 1 ID [a x="sensitive-evidence]`, ParserOptions{}},
		{"duplicate SD id", "syslog5424", `<1>1 - host app 1 ID [a][a] sensitive-evidence`, ParserOptions{}},
		{"unsupported syslog version", "syslog5424", `<1>2 - host app 1 ID - sensitive-evidence`, ParserOptions{}},
		{"unescaped SD bracket", "syslog5424", `<1>1 - host app 1 ID [a x="bad]value"] sensitive-evidence`, ParserOptions{}},
		{"unescaped SD quote", "syslog5424", `<1>1 - host app 1 ID [a x="bad"value"] sensitive-evidence`, ParserOptions{}},
		{"invalid HTTP status", "combined", `192.0.2.1 - - [19/Oct/2026:10:00:00 +0000] "GET /sensitive-evidence HTTP/1.1" 999 0 "-" "agent"`, ParserOptions{}},
		{"invalid HTTP timestamp", "combined", `192.0.2.1 - - [99/Oct/2026:10:00:00 +0000] "GET /sensitive-evidence HTTP/1.1" 200 0 "-" "agent"`, ParserOptions{}},
		{"non-numeric JSON status", "json", `{"secret":"sensitive-evidence"}`, ParserOptions{FieldMap: map[string]string{"secret": "http.response.status_code"}}},
		{"SSH invalid port", "openssh", "Failed password for sensitive-evidence from 192.0.2.1 port 65536 ssh2", ParserOptions{}},
		{"SSH invalid IP", "openssh", "Failed password for sensitive-evidence from attacker.example port 22 ssh2", ParserOptions{}},
		{"SSH non-auth", "openssh", "sshd[1]: sensitive-evidence", ParserOptions{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.kind, tc.line, testEnvelope(), tc.options)
			if err == nil || got.ParseStatus != ParseStatusError || got.Fields != nil {
				t.Fatalf("malformed input did not fail closed: %+v, %v", got.Envelope, err)
			}
			if strings.Contains(err.Error(), "sensitive-evidence") || got.Raw != tc.line {
				t.Fatal("error leaked evidence or raw evidence changed")
			}
		})
	}
}

func TestLineLimitsAndDeterminism(t *testing.T) {
	if _, err := Parse("json", strings.Repeat("x", DefaultMaxLineBytes+1), testEnvelope(), ParserOptions{}); !errors.Is(err, ErrOversize) {
		t.Fatalf("default line cap: %v", err)
	}
	if _, err := Parse("json", "{}", testEnvelope(), ParserOptions{MaxLineBytes: HardMaxLineBytes+1}); err == nil {
		t.Fatal("configuration must not raise the hard line cap")
	}
	raw := `{"secret":"value","number":3}`
	options := ParserOptions{FieldMap: map[string]string{"secret": "message", "number": "network.bytes"}}
	first, err := Parse("json", raw, testEnvelope(), options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse("json", raw, testEnvelope(), options)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("same bytes and envelope did not give the same event")
	}
}
