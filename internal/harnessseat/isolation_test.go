package harnessseat

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureIdentity(t *testing.T, harness Harness) Identity {
	t.Helper()
	home := t.TempDir()
	return Identity{ID: "fixture-seat", Harness: harness, Owner: "fixture-user", HomeDir: home, ConfigDir: filepath.Join(home, "isolated-seat")}
}

func TestChildEnvironmentStripsInheritedCredentialsAndHomes(t *testing.T) {
	for _, harness := range []Harness{Claude, Codex, Copilot, Muse} {
		t.Run(string(harness), func(t *testing.T) {
			i := fixtureIdentity(t, harness)
			inherited := []string{
				"GITHUB_TOKEN=fixture-secret", "GH_TOKEN=fixture-secret", "COPILOT_GITHUB_TOKEN=fixture-secret",
				"COPILOT_PROVIDER_API_KEY=fixture-secret", "OPENAI_API_KEY=fixture-secret", "CODEX_ACCESS_TOKEN=fixture-secret",
				"ANTHROPIC_API_KEY=fixture-secret", "ANTHROPIC_AUTH_TOKEN=fixture-secret", "CLAUDE_CODE_OAUTH_TOKEN=fixture-secret",
				"META_API_KEY=fixture-secret", "MUSE_AUTH_PATH=/untrusted", "HOME=/untrusted", "PATH=/untrusted",
				"CODEX_HOME=/untrusted", "CLAUDE_CONFIG_DIR=/untrusted", "COPILOT_HOME=/untrusted", "GH_CONFIG_DIR=/untrusted",
				"XDG_CONFIG_HOME=/untrusted", "LD_PRELOAD=/untrusted", "BASH_ENV=/untrusted", "LANG=C.UTF-8",
			}
			env, err := ChildEnvironment(i, "/fixture/bin", inherited)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(env, "\n")
			if strings.Contains(joined, "fixture-secret") || strings.Contains(joined, "/untrusted") {
				t.Fatal("inherited authority escaped into the child environment")
			}
			values := map[string]string{}
			for _, entry := range env {
				key, value, ok := strings.Cut(entry, "=")
				if !ok {
					t.Fatal("invalid environment entry")
				}
				if _, duplicate := values[key]; duplicate {
					t.Fatal("duplicate environment key")
				}
				values[key] = value
			}
			if values["HOME"] != i.HomeDir || values["PATH"] != "/fixture/bin" || values["LANG"] != "C.UTF-8" {
				t.Fatal("owner environment was not retained")
			}
			key := map[Harness]string{Claude: "CLAUDE_CONFIG_DIR", Codex: "CODEX_HOME", Copilot: "COPILOT_HOME", Muse: "XDG_CONFIG_HOME"}[harness]
			if values[key] != i.ConfigDir {
				t.Fatal("seat-specific home not selected")
			}
			if harness == Copilot && values["GH_CONFIG_DIR"] != filepath.Join(i.ConfigDir, "gh") {
				t.Fatal("Copilot must not inherit the shared GitHub CLI home")
			}
			if harness == Muse && values["MUSE_LOGIN"] != "0" {
				t.Fatal("Muse must not start a login flow")
			}
		})
	}
}

func TestDistinctSeatsHaveDistinctConfigEnvironments(t *testing.T) {
	for _, harness := range []Harness{Claude, Codex, Copilot, Muse} {
		a := fixtureIdentity(t, harness)
		b := a
		b.ID, b.ConfigDir = "other-seat", filepath.Join(a.HomeDir, "other-seat")
		x, err := ChildEnvironment(a, "/fixture/bin", nil)
		if err != nil {
			t.Fatal(err)
		}
		y, err := ChildEnvironment(b, "/fixture/bin", x)
		if err != nil || strings.Contains(strings.Join(y, "\n"), a.ConfigDir) {
			t.Fatal("second seat inherited first seat configuration")
		}
	}
}

func TestProjectionAndDigestsDoNotExposeLocalIdentity(t *testing.T) {
	i := fixtureIdentity(t, Codex)
	salt := []byte(strings.Repeat("s", 32))
	p, err := i.Projection(salt, "fixture-node")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]any{i, p})
	for _, private := range []string{i.ID, i.Owner, i.ConfigDir, i.HomeDir, "fixture-node"} {
		if strings.Contains(string(data), private) {
			t.Fatal("local identity leaked in JSON")
		}
	}
	first, err := AccountDigest(salt, Codex, "fixture-account")
	if err != nil || len(first) != 64 || strings.Contains(first, "fixture-account") {
		t.Fatal("invalid account digest")
	}
	again, _ := AccountDigest(salt, Codex, "fixture-account")
	otherSalt, _ := AccountDigest([]byte(strings.Repeat("t", 32)), Codex, "fixture-account")
	if first != again || first == otherSalt || first == p.SeatRef {
		t.Fatal("digest stability, salt or domain separation failed")
	}
	if err := ValidateBindings([]AccountBinding{{SeatID: "one", Harness: Codex, Digest: first}, {SeatID: "two", Harness: Codex, Digest: first}}); err != ErrDuplicateAccount {
		t.Fatalf("duplicate account accepted: %v", err)
	}
}

func TestIdentityRefusesUnknownAndUnsafeInputs(t *testing.T) {
	base := fixtureIdentity(t, Codex)
	for _, change := range []func(*Identity){
		func(i *Identity) { i.ID = "../escape" }, func(i *Identity) { i.Harness = "unknown" },
		func(i *Identity) { i.Owner = "" }, func(i *Identity) { i.ConfigDir = "relative" },
		func(i *Identity) { i.HomeDir = "relative" }, func(i *Identity) { i.ConfigDir = filepath.Clean(string(filepath.Separator)) },
	} {
		i := base
		change(&i)
		if _, err := ChildEnvironment(i, "/fixture/bin", nil); err == nil {
			t.Fatal("unsafe identity accepted")
		}
	}
	if _, err := AccountDigest([]byte("short"), Codex, "fixture-account"); err == nil {
		t.Fatal("short salt accepted")
	}
}
