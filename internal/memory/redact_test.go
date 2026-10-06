package memory

import (
	"strings"
	"testing"
)

func TestRedactSensitiveText(t *testing.T) {
	key := "act_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	session := "session-private-123"
	got := RedactSensitiveText("bearer="+key+" session="+session+" other=act_abcdefghijklmno", key, session)
	for _, secret := range []string{key, session, "act_abcdefghijklmno"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret survived redaction: %q", got)
		}
	}
	if strings.Count(got, redactedValue) != 3 {
		t.Fatalf("unexpected redaction output: %q", got)
	}
}

func TestRedactSensitiveJSON_CatchesEscapedBearer(t *testing.T) {
	key := "act_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	escaped := `{"jsonrpc":"2.0","result":{"text":"\u0061\u0063\u0074\u005f0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`
	got, err := RedactSensitiveJSON([]byte(escaped), key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), key) || strings.Contains(string(got), `\u0061\u0063\u0074`) {
		t.Fatalf("escaped bearer survived structural redaction: %s", got)
	}
}
