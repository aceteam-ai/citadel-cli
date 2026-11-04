package declaredsession

import (
	"errors"
	"strings"
	"testing"
)

const fixtureConfig = "sessions:\n  - name: automated-citadel\n    harness: codex\n    command: [harness, exec, literal]\n    cwd: /tmp\n"

func TestConfigDefaultsAndIndependentRevisions(t *testing.T) {
	first, err := DecodeConfig([]byte(fixtureConfig))
	if err != nil || len(first.Sessions) != 1 {
		t.Fatalf("config refused: %v", err)
	}
	d := first.Sessions[0]
	if d.Mode != "interactive" || !d.Enabled || d.Restart != (RestartPolicy{Policy: "on-failure", MinSeconds: 1, MaxSeconds: 300, ResetAfter: 600}) || d.Attach != (AttachPolicy{Local: "interactive", Web: "readonly", MaxViewers: 4}) || d.ScrollbackBytes != 262144 || d.Log != (LogPolicy{RotateBytes: 16777216, Keep: 5}) || !d.AceTeam.Register || d.AceTeam.Takeover || !validDigest(d.Revision) {
		t.Fatal("DoR defaults changed")
	}
	other := fixtureConfig + "  - name: another\n    harness: other\n    command: [other]\n    cwd: /tmp\n"
	second, err := DecodeConfig([]byte(other))
	if err != nil || second.FileRevision == first.FileRevision || second.Sessions[0].Revision != d.Revision {
		t.Fatalf("unrelated declaration changed revision: %v", err)
	}
	third, err := DecodeConfig([]byte(strings.Replace(fixtureConfig, "literal", "changed", 1)))
	if err != nil || third.Sessions[0].Revision == d.Revision {
		t.Fatalf("command edit did not change revision: %v", err)
	}
	if _, exists := first.Lookup("missing"); exists {
		t.Fatal("lookup invented a declaration")
	}
}

func TestConfigStrictNamesAndSchema(t *testing.T) {
	for _, name := range []string{"a", "automated-citadel", "a..b", strings.Repeat("a", 64)} {
		if err := ValidateName(name); err != nil {
			t.Fatalf("lease-compatible name refused: %q", name)
		}
	}
	for _, name := range []string{"", ".", "..", "_a", "-a", "A", " a", "a ", "a/b", `a\b`, strings.Repeat("a", 65)} {
		if !errors.Is(ValidateName(name), ErrConfig) {
			t.Fatalf("noncanonical name accepted: %q", name)
		}
	}
	for _, raw := range []string{
		"", "sessions: null", "sessions: {}", "sessions: []\nother: true", fixtureConfig + "---\nsessions: []",
		fixtureConfig + "    Name: other\n", fixtureConfig + "    name: duplicate\n",
		fixtureConfig + "    env: {TOKEN: null}\n", fixtureConfig + "    env: {TOKEN: 1}\n",
		fixtureConfig + "    env: {TOKEN: a, TOKEN: b}\n", fixtureConfig + "    env: {PATH: /bin}\n",
		fixtureConfig + "    enabled: yes\n", fixtureConfig + "    enabled: null\n",
		fixtureConfig + "    restart: {policy: on-failure, max_s: 0}\n",
		fixtureConfig + "    attach: {web: control}\n", fixtureConfig + "    attach: {max_viewers: 33}\n",
		fixtureConfig + "    log: {rotate_bytes: 4095}\n", fixtureConfig + "    log: {keep: 17}\n",
		fixtureConfig + "    scrollback_bytes: 4194305\n",
		strings.Replace(fixtureConfig, "command: [harness, exec, literal]", "command: harness", 1),
		strings.Replace(fixtureConfig, "command: [harness, exec, literal]", "command: []", 1),
		strings.Replace(fixtureConfig, "cwd: /tmp", "cwd: relative", 1),
		strings.Replace(fixtureConfig, "harness: codex", "harness: !custom codex", 1),
		fixtureConfig + "    env: &e {TOKEN: a}\n    aceteam: *e\n",
		fixtureConfig + "    restart: {<<: {policy: never}}\n",
	} {
		if _, err := DecodeConfig([]byte(raw)); !errors.Is(err, ErrConfig) {
			t.Fatalf("invalid schema accepted: %q err=%v", raw, err)
		}
	}
	if _, err := DecodeConfig([]byte(strings.Repeat(" ", MaxConfigBytes+1))); !errors.Is(err, ErrConfig) {
		t.Fatal("oversized config accepted")
	}
}

func TestLiteralEnvFileStrictness(t *testing.T) {
	got, err := parseEnvFile([]byte("# fixture\nTOKEN=literal $HOME $(not-executed)\nEMPTY=\n"))
	if err != nil || got["TOKEN"] != "literal $HOME $(not-executed)" || got["EMPTY"] != "" {
		t.Fatal("env file expanded or lost literal values")
	}
	for _, raw := range []string{"export TOKEN=x", "TOKEN", "TOKEN=x\nTOKEN=y", "TOKEN=\"quoted\"", "TOKEN='quoted'", " BAD=x", "1BAD=x", "TOKEN=x\x00y", "PATH=/bin", "ACETEAM_SESSION=other", "ACETEAM_API_KEY=synthetic-secret", strings.Repeat("x", MaxEnvBytes+1)} {
		if _, err := parseEnvFile([]byte(raw)); !errors.Is(err, ErrConfig) {
			t.Fatalf("unsafe env accepted: %q", raw)
		}
	}
}

func TestRecordDuplicateKeysRefused(t *testing.T) {
	for _, raw := range []string{`{"version":1,"version":1}`, `{"holder":{"pid":1,"pid":2}}`, `{"version":1,"VERSION":2}`, `{"holder":{"pid":1,"PID":2}}`, `{"holder":{"ſtart_ticks":1}}`, `{"Version":1}`, `{"version":1} {}`, `[]`} {
		if err := decodeRecord([]byte(raw), new(State)); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("ambiguous record accepted: %s", raw)
		}
	}
}
