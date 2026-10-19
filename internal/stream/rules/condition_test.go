package rules

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

func conditionTestCompile(t *testing.T, selection any) predicate {
	t.Helper()
	compiled, err := compileDetection(map[string]any{"selection": selection, "condition": "selection"})
	if err != nil {
		t.Fatalf("compile detection: %v", err)
	}
	return compiled
}

func TestDetectionFieldSemantics(t *testing.T) {
	tests := []struct {
		name string
		selection any
		fields map[string]any
		want bool
	}{
		{"exact case-insensitive", map[string]any{"user.name": "ALICE"}, map[string]any{"user.name": "alice"}, true},
		{"exact not substring", map[string]any{"user.name": "alice"}, map[string]any{"user.name": "alice2"}, false},
		{"wildcard star", map[string]any{"message": "start*end"}, map[string]any{"message": "START\nline\nEND"}, true},
		{"wildcard question Unicode", map[string]any{"message": "a?c"}, map[string]any{"message": "a界c"}, true},
		{"question mandatory", map[string]any{"message": "a?c"}, map[string]any{"message": "ac"}, false},
		{"literal star", map[string]any{"message": `a\*b`}, map[string]any{"message": "a*b"}, true},
		{"literal question", map[string]any{"message": `a\?b`}, map[string]any{"message": "a?b"}, true},
		{"plain Windows backslashes", map[string]any{"message": `C:\Windows\System32\cmd.exe`}, map[string]any{"message": `c:\windows\system32\CMD.EXE`}, true},
		{"escaped backslash wildcard", map[string]any{"message": `C:\\*`}, map[string]any{"message": `C:\path\file.exe`}, true},
		{"escaped backslash literal star", map[string]any{"message": `C:\\\*`}, map[string]any{"message": `C:\*`}, true},
		{"double escaped backslash", map[string]any{"message": `a\\\\b`}, map[string]any{"message": `a\\b`}, true},
		{"ordinary regex punctuation literal", map[string]any{"message": "[x].(y)+"}, map[string]any{"message": "[x].(y)+"}, true},
		{"contains", map[string]any{"message|contains": "EVIL"}, map[string]any{"message": "prefix evil suffix"}, true},
		{"contains wildcard", map[string]any{"message|contains": "evil?.exe"}, map[string]any{"message": "prefix evil1.exe suffix"}, true},
		{"startswith", map[string]any{"message|startswith": "cmd"}, map[string]any{"message": "CMD /c whoami"}, true},
		{"startswith not middle", map[string]any{"message|startswith": "cmd"}, map[string]any{"message": "prefix cmd"}, false},
		{"endswith", map[string]any{"message|endswith": ".EXE"}, map[string]any{"message": "cmd.exe"}, true},
		{"endswith not middle", map[string]any{"message|endswith": ".exe"}, map[string]any{"message": "cmd.exe suffix"}, false},
		{"cased match", map[string]any{"message|contains|cased": "Evil"}, map[string]any{"message": "xEvilx"}, true},
		{"cased mismatch", map[string]any{"message|cased|contains": "Evil"}, map[string]any{"message": "xEVILx"}, false},
		{"map is AND", map[string]any{"a": "x", "b": "y"}, map[string]any{"a": "x"}, false},
		{"field list is OR", map[string]any{"status": []any{401, 403}}, map[string]any{"status": int64(403)}, true},
		{"all list is AND", map[string]any{"message|contains|all": []string{"cmd", "/c"}}, map[string]any{"message": "cmd /c whoami"}, true},
		{"all list missing item", map[string]any{"message|all|contains": []string{"cmd", "/c"}}, map[string]any{"message": "cmd.exe"}, false},
		{"array field OR", map[string]any{"tags": "malicious"}, map[string]any{"tags": []any{"benign", "MALICIOUS"}}, true},
		{"array field all", map[string]any{"tags|all": []string{"a", "b"}}, map[string]any{"tags": []any{"B", "A"}}, true},
		{"nested array not searched", map[string]any{"tags": "malicious"}, map[string]any{"tags": []any{[]any{"malicious"}}}, false},
		{"numeric types equal", map[string]any{"status": int(401)}, map[string]any{"status": float64(401)}, true},
		{"JSON numeric decimal", map[string]any{"number": json.Number("4.010e2")}, map[string]any{"number": int64(401)}, true},
		{"fraction retained", map[string]any{"number": 401.5}, map[string]any{"number": int64(401)}, false},
		{"large uint no float rounding", map[string]any{"number": uint64(9007199254740993)}, map[string]any{"number": uint64(9007199254740992)}, false},
		{"numeric not string", map[string]any{"status": 401}, map[string]any{"status": "401"}, false},
		{"string not numeric", map[string]any{"status": "401"}, map[string]any{"status": 401}, false},
		{"boolean typed", map[string]any{"enabled": true}, map[string]any{"enabled": true}, true},
		{"boolean not string", map[string]any{"enabled": true}, map[string]any{"enabled": "true"}, false},
		{"boolean not number", map[string]any{"enabled": true}, map[string]any{"enabled": 1}, false},
		{"explicit null", map[string]any{"user.name": nil}, map[string]any{"user.name": nil}, true},
		{"missing not null", map[string]any{"user.name": nil}, map[string]any{}, false},
		{"empty not null", map[string]any{"user.name": nil}, map[string]any{"user.name": ""}, false},
		{"array null not field null", map[string]any{"user.name": nil}, map[string]any{"user.name": []any{nil}}, false},
		{"exists null", map[string]any{"user.name|exists": true}, map[string]any{"user.name": nil}, true},
		{"exists empty", map[string]any{"user.name|exists": true}, map[string]any{"user.name": ""}, true},
		{"exists missing", map[string]any{"user.name|exists": true}, map[string]any{}, false},
		{"not exists", map[string]any{"user.name|exists": false}, map[string]any{}, true},
		{"empty matches only empty", map[string]any{"message": ""}, map[string]any{"message": ""}, true},
		{"empty not missing", map[string]any{"message": ""}, map[string]any{}, false},
		{"no nested-map traversal", map[string]any{"user.name": "alice"}, map[string]any{"user": map[string]any{"name": "alice"}}, false},
		{"list maps OR", []any{map[string]any{"status": 401}, map[string]any{"status": 403}}, map[string]any{"status": 403}, true},
		{"keyword list", []string{"EVILSERVICE", "svchost.exe -n evil"}, map[string]any{"message": "prefix EvilService suffix"}, true},
		{"keyword searches message only", []string{"secret"}, map[string]any{"user.name": "secret", "message": "clean"}, false},
		{"keyword all", map[string]any{"|all": []string{"OabVirtualDirectory", " -ExternalUrl "}}, map[string]any{"message": "set OabVirtualDirectory -ExternalUrl https://example.invalid"}, true},
		{"keyword all incomplete", map[string]any{"|all": []string{"OabVirtualDirectory", " -ExternalUrl "}}, map[string]any{"message": "OabVirtualDirectory"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled := conditionTestCompile(t, test.selection)
			if got := compiled(test.fields); got != test.want {
				t.Fatalf("matched = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDetectionRegexAndCIDR(t *testing.T) {
	tests := []struct {
		name string
		key string
		pattern string
		value any
		want bool
	}{
		{"regex case-sensitive", "message|re", "evil", "EVIL", false},
		{"regex searches", "message|re", "evil", "prefix evil suffix", true},
		{"regex insensitive", "message|re|i", "evil", "EVIL", true},
		{"regex grouping", "message|re", "^(foo|bar)[0-9]{1,3}$", "bar123", true},
		{"regex single-line default", "message|re", "^first.*last$", "first\nlast", false},
		{"regex single-line flag", "message|re|s", "^first.*last$", "first\nlast", true},
		{"regex multiline flag", "message|re|m", "^middle$", "first\nmiddle\nlast", true},
		{"regex combined flags", "message|re|i|m", "^middle$", "first\nMIDDLE\nlast", true},
		{"regex is not a glob", "message|re", "a.b", "a*b", true},
		{"regex typed", "message|re", "401", 401, false},
		{"IPv4 inside", "source.ip|cidr", "192.0.2.0/24", "192.0.2.42", true},
		{"IPv4 outside", "source.ip|cidr", "192.0.2.0/24", "192.0.3.42", false},
		{"IPv4 host bits normalized", "source.ip|cidr", "192.0.2.9/24", "192.0.2.200", true},
		{"IPv6 inside", "source.ip|cidr", "2001:db8::/32", "2001:db8:abcd::1", true},
		{"IPv6 outside", "source.ip|cidr", "2001:db8::/32", "2001:db9::1", false},
		{"family distinct", "source.ip|cidr", "192.0.2.0/24", "::ffff:192.0.2.42", false},
		{"invalid IP", "source.ip|cidr", "192.0.2.0/24", "192.0.2.999", false},
		{"zoned IP refused", "source.ip|cidr", "fe80::/10", "fe80::1%eth0", false},
		{"IP with port refused", "source.ip|cidr", "192.0.2.0/24", "192.0.2.42:22", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled := conditionTestCompile(t, map[string]any{test.key: test.pattern})
			if got := compiled(map[string]any{strings.Split(test.key, "|")[0]: test.value}); got != test.want {
				t.Fatalf("matched = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDetectionConditions(t *testing.T) {
	tests := []struct {
		expression any
		fields map[string]any
		want bool
	}{
		{"a or b and c", map[string]any{"a": true}, true},
		{"a or b and c", map[string]any{"b": true}, false},
		{"a or b and c", map[string]any{"b": true, "c": true}, true},
		{"(a or b) and c", map[string]any{"a": true}, false},
		{"(a or b) and c", map[string]any{"a": true, "c": true}, true},
		{"a and not b", map[string]any{"a": true}, true},
		{"not not a", map[string]any{"a": true}, true},
		{"a and not (b or c)", map[string]any{"a": true, "c": true}, false},
		{"1 of them", map[string]any{"hidden": true}, false},
		{"all of them", map[string]any{"a": true, "b": true, "c": true, "notepad": true}, true},
		{"1 of a*", map[string]any{"a": true}, true},
		{"all of *", map[string]any{"a": true, "b": true, "c": true, "notepad": true}, true},
		{"1 of _hidden*", map[string]any{"hidden": true}, true},
		{"_hidden", map[string]any{"hidden": true}, true},
		{"notepad", map[string]any{"notepad": true}, true},
		{[]string{"a and b", "c"}, map[string]any{"c": true}, true},
		{[]any{"a and b", "c"}, map[string]any{"a": true}, false},
	}
	for index, test := range tests {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			detection := map[string]any{
				"a": map[string]any{"a": true},
				"b": map[string]any{"b": true},
				"c": map[string]any{"c": true},
				"notepad": map[string]any{"notepad": true},
				"_hidden": map[string]any{"hidden": true},
				"condition": test.expression,
			}
			compiled, err := compileDetection(detection)
			if err != nil {
				t.Fatalf("compile condition: %v", err)
			}
			if got := compiled(test.fields); got != test.want {
				t.Fatalf("matched = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDetectionRefusesUnsupportedOrAmbiguousInput(t *testing.T) {
	tests := []struct {
		name string
		detection map[string]any
		reason string
	}{
		{"windash named", map[string]any{"selection": map[string]any{"message|windash": "cmd"}, "condition": "selection"}, "unsupported modifier: windash"},
		{"unreferenced bad selection", map[string]any{"selection": map[string]any{"message": "x"}, "unused": map[string]any{"message|base64": "x"}, "condition": "selection"}, "base64"},
		{"empty modifier", map[string]any{"selection": map[string]any{"message|": "x"}, "condition": "selection"}, "unsupported modifier"},
		{"duplicate modifier", map[string]any{"selection": map[string]any{"message|contains|contains": "x"}, "condition": "selection"}, "duplicate modifier: contains"},
		{"chained string modes", map[string]any{"selection": map[string]any{"message|contains|startswith": "x"}, "condition": "selection"}, "contains with startswith"},
		{"regex with contains", map[string]any{"selection": map[string]any{"message|re|contains": "x"}, "condition": "selection"}, "re with contains"},
		{"regex flag without regex", map[string]any{"selection": map[string]any{"message|i": "x"}, "condition": "selection"}, "i requires preceding re"},
		{"regex flag order", map[string]any{"selection": map[string]any{"message|i|re": "x"}, "condition": "selection"}, "i requires preceding re"},
		{"regex lookahead", map[string]any{"selection": map[string]any{"message|re": "a(?=b)"}, "condition": "selection"}, "unsupported regular-expression feature"},
		{"regex noncapturing group", map[string]any{"selection": map[string]any{"message|re": "(?:a)"}, "condition": "selection"}, "unsupported regular-expression feature"},
		{"regex backreference", map[string]any{"selection": map[string]any{"message|re": `(a)\1`}, "condition": "selection"}, "backreference"},
		{"regex invalid", map[string]any{"selection": map[string]any{"message|re": "["}, "condition": "selection"}, "invalid regular expression"},
		{"regex cased ambiguity", map[string]any{"selection": map[string]any{"message|re|cased": "x"}, "condition": "selection"}, "cased with re"},
		{"CIDR invalid", map[string]any{"selection": map[string]any{"source.ip|cidr": "192.0.2.0/33"}, "condition": "selection"}, "invalid cidr"},
		{"CIDR requires prefix", map[string]any{"selection": map[string]any{"source.ip|cidr": "192.0.2.1"}, "condition": "selection"}, "invalid cidr"},
		{"CIDR IPv6 invalid", map[string]any{"selection": map[string]any{"source.ip|cidr": "2001:db8::/129"}, "condition": "selection"}, "invalid cidr"},
		{"typed string modifier", map[string]any{"selection": map[string]any{"status|contains": 401}, "condition": "selection"}, "requires string"},
		{"scalar all", map[string]any{"selection": map[string]any{"message|all": "x"}, "condition": "selection"}, "all requires a list"},
		{"null list", map[string]any{"selection": map[string]any{"message": []any{"", nil}}, "condition": "selection"}, "null cannot be part"},
		{"null modifier", map[string]any{"selection": map[string]any{"message|contains": nil}, "condition": "selection"}, "null does not support"},
		{"exists typed", map[string]any{"selection": map[string]any{"message|exists": "true"}, "condition": "selection"}, "requires a boolean"},
		{"exists all", map[string]any{"selection": map[string]any{"message|exists|all": []any{true, false}}, "condition": "selection"}, "exists with all"},
		{"map value", map[string]any{"selection": map[string]any{"message": map[string]any{"nested": "x"}}, "condition": "selection"}, "unsupported field value type"},
		{"nested field list", map[string]any{"selection": map[string]any{"message": []any{[]any{"x"}}}, "condition": "selection"}, "unsupported field value type"},
		{"keyword all type", map[string]any{"selection": map[string]any{"|all": []any{1, 2}}, "condition": "selection"}, "keyword |all requires string"},
		{"empty map", map[string]any{"selection": map[string]any{}, "condition": "selection"}, "selection map"},
		{"empty list", map[string]any{"selection": []any{}, "condition": "selection"}, "selection must be"},
		{"nonfinite number", map[string]any{"selection": map[string]any{"number": math.NaN()}, "condition": "selection"}, "finite number"},
		{"numeric exponent bomb", map[string]any{"selection": map[string]any{"number": json.Number("1e999999999")}, "condition": "selection"}, "finite number"},
		{"missing selection", map[string]any{"condition": "selection"}, "requires 1 to 256"},
		{"unknown selection", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "missing"}, "unknown condition selection: missing"},
		{"empty selector", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "all of missing*"}, "matches no selection"},
		{"empty them", map[string]any{"_hidden": map[string]any{"message": "x"}, "condition": "1 of them"}, "matches no selection"},
		{"wildcard not direct reference", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "selection*"}, "unexpected condition operand"},
		{"legacy aggregation syntax", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "selection | count() > 5"}, "unsupported condition token"},
		{"legacy timeframe", map[string]any{"selection": map[string]any{"message": "x"}, "timeframe": "10m", "condition": "selection"}, "unsupported detection feature: timeframe"},
		{"condition trailing junk", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "selection selection"}, "unexpected condition token"},
		{"condition unclosed", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "(selection"}, "missing closing parenthesis"},
		{"condition no operand", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "selection and"}, "missing an operand"},
		{"condition dangling close", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "selection)"}, "unexpected condition token"},
		{"condition empty", map[string]any{"selection": map[string]any{"message": "x"}, "condition": " "}, "empty condition"},
		{"condition list bad type", map[string]any{"selection": map[string]any{"message": "x"}, "condition": []any{"selection", 2}}, "string or a list"},
		{"condition not selection", map[string]any{"selection": map[string]any{"message": "x"}, "condition": "not"}, "missing an operand"},
		{"reserved selection", map[string]any{"and": map[string]any{"message": "x"}, "condition": "and"}, "invalid selection name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := compileDetection(test.detection)
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("error = %v, want reason containing %q", err, test.reason)
			}
			if compiled != nil {
				t.Fatal("refused detection returned a partially compiled predicate")
			}
		})
	}
}

func TestDetectionCompilationLimits(t *testing.T) {
	selection := map[string]any{"message": "x"}
	tooManySelections := map[string]any{"condition": "1 of them"}
	for i := 0; i < 257; i++ {
		tooManySelections["s"+strconv.Itoa(i)] = selection
	}
	tooManyFields := make(map[string]any)
	for i := 0; i < 257; i++ {
		tooManyFields["field"+strconv.Itoa(i)] = "x"
	}
	var nested any = selection
	for i := 0; i < 17; i++ {
		nested = []any{nested}
	}
	tooManyValues := make([]any, 1025)
	for i := range tooManyValues {
		tooManyValues[i] = "x"
	}
	largeNodeTree := make([]any, 1024)
	for i := range largeNodeTree {
		fields := make(map[string]any)
		for j := 0; j < 32; j++ {
			fields["f"+strconv.Itoa(j)] = "x"
		}
		largeNodeTree[i] = fields
	}
	largePatterns := make([]any, 17)
	for i := range largePatterns {
		largePatterns[i] = strings.Repeat("x", 64*1024)
	}
	cases := []map[string]any{
		tooManySelections,
		{"selection": tooManyFields, "condition": "selection"},
		{"selection": nested, "condition": "selection"},
		{"selection": map[string]any{"message": tooManyValues}, "condition": "selection"},
		{"selection": map[string]any{"message": strings.Repeat("x", 64*1024+1)}, "condition": "selection"},
		{"selection": selection, "condition": strings.Repeat(" ", 8193)},
		{"selection": selection, "condition": strings.Repeat("not ", 1024)+"selection"},
		{"selection": selection, "condition": strings.Repeat("(", 65)+"selection"+strings.Repeat(")", 65)},
		{"selection": largeNodeTree, "condition": "selection"},
		{"selection": map[string]any{"message": largePatterns}, "condition": "selection"},
		{"selection": selection, "condition": []string{strings.Repeat(" ", 4100)+"selection", strings.Repeat(" ", 4100)+"selection"}},
	}
	for i, detection := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if compiled, err := compileDetection(detection); err == nil || compiled != nil {
				t.Fatalf("over-limit detection was not fully refused: %v", err)
			}
		})
	}
}

func TestDetectionCompiledPredicateIsImmutableAndDeterministic(t *testing.T) {
	values := []any{"first", "second"}
	selection := map[string]any{"message": values}
	detection := map[string]any{"selection": selection, "condition": "selection"}
	compiled, err := compileDetection(detection)
	if err != nil {
		t.Fatal(err)
	}
	values[0] = "mutated"
	selection["message"] = "replacement"
	detection["condition"] = "not selection"
	for i := 0; i < 100; i++ {
		if !compiled(map[string]any{"message": "FIRST"}) || compiled(map[string]any{"message": "mutated"}) {
			t.Fatal("compiled predicate retained mutable source values or changed results")
		}
	}
	bad := map[string]any{
		"z": map[string]any{"message|windash": "x"},
		"a": map[string]any{"message|base64": "x"},
		"condition": "1 of them",
	}
	for i := 0; i < 100; i++ {
		_, err := compileDetection(bad)
		if err == nil || !strings.Contains(err.Error(), "base64") {
			t.Fatalf("refusal order is not deterministic: %v", err)
		}
	}
}
