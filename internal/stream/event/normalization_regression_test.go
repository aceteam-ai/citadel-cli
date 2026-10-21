package event

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONDecimalAndExponentPrecisionIsPreserved(t *testing.T) {
	options := ParserOptions{FieldMap: map[string]string{"number": "test.number"}}
	for _, number := range []string{
		"9007199254740993.0",
		"9007199254740992.0",
		"9.007199254740993e15",
		"9223372036854775808.0",
		"0.1",
		"1e999",
		"1e1024",
		"1e-1024",
	} {
		t.Run(number, func(t *testing.T) {
			raw := `{"number":` + number + `}`
			got, err := Parse("json", raw, testEnvelope(), options)
			if err != nil {
				t.Fatal(err)
			}
			value, ok := got.Fields["test.number"].(json.Number)
			if !ok || string(value) != number || got.Raw != raw {
				t.Fatalf("decimal was rounded or retyped: got %#v, want json.Number(%q)", got.Fields["test.number"], number)
			}
		})
	}
	got, err := Parse("json", `{"number":9223372036854775807}`, testEnvelope(), options)
	if err != nil || got.Fields["test.number"] != int64(9223372036854775807) {
		t.Fatal("supported integer literals must retain the existing int64 policy")
	}
	for _, number := range []string{"9223372036854775808", "-9223372036854775809", "1e1025", "1e-1025", "0." + strings.Repeat("0", 126) + "1"} {
		got, err := Parse("json", `{"number":`+number+`}`, testEnvelope(), options)
		if err == nil || got.ParseStatus != ParseStatusError || got.Fields != nil {
			t.Fatalf("out-of-policy number accepted: %s", number)
		}
	}
}

func TestJSONMappedScalarArraysRefuseBeyondMatcherLimit(t *testing.T) {
	options := ParserOptions{FieldMap: map[string]string{"values": "test.values"}}
	for _, size := range []int{1024, 1025} {
		raw := `{"values":[` + strings.Repeat("0,", size-1) + `0]}`
		got, err := Parse("json", raw, testEnvelope(), options)
		if size == 1024 {
			if err != nil || len(got.Fields["test.values"].([]any)) != size {
				t.Fatalf("array at the matcher limit was refused: %v", err)
			}
		} else if err == nil || got.ParseStatus != ParseStatusError || got.Fields != nil {
			t.Fatal("oversize mapped array must fail at normalization, not silently become a non-match")
		}
	}
}

func TestCombinedAbsoluteURIPathNeverComesFromQuery(t *testing.T) {
	for _, target := range []string{
		"http://example.test?next=/admin",
		"https://example.test?next=/admin/private",
		"http://example.test:8080?next=/admin",
		"http://example.test/login?next=/admin",
	} {
		raw := `192.0.2.1 - - [19/Oct/2026:10:00:00 +0000] "GET ` + target + ` HTTP/1.1" 200 0 "-" "agent"`
		got, err := Parse("combined", raw, testEnvelope(), ParserOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := "/"
		if strings.Contains(target, "/login?") {
			want = "/login"
		}
		if got.Fields["url.path"] != want || got.Fields["url.original"] != target || got.Raw != raw {
			t.Fatalf("query reinterpreted as a path: %#v", got.Fields)
		}
	}
}

func TestRFC5424RepeatedParametersAndUnknownEscapesPreserved(t *testing.T) {
	// RFC5424 section 6.3.3: repeated SD-PARAM is allowed; an unknown
	// backslash sequence must remain two literal characters. SD-ID remains
	// unique, and reserved escapes must still protect framing characters.
	structured := `[example@32473 x="1" x="2" unknown="\q" literal="\n" open="\[" bracket="\]" quote="\"" backslash="\\"]`
	raw := "<1>1 - host app 1 ID " + structured + " original message"
	got, err := Parse("syslog5424", raw, testEnvelope(), ParserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fields["log.syslog.structured_data"] != structured || got.Fields["message"] != "original message" || got.Raw != raw {
		t.Fatalf("structured data bytes changed: %#v", got.Fields)
	}
	if _, err := Parse("syslog5424", `<1>1 - host app 1 ID [example@32473 x="1"][example@32473 x="2"]`, testEnvelope(), ParserOptions{}); err == nil {
		t.Fatal("duplicate SD-ID must remain refused")
	}
}
