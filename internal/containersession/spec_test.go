package containersession

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

func TestSessionUUIDStableAndPerSession(t *testing.T) {
	a, b := SessionUUID("lane1"), SessionUUID("lane2")
	if a != SessionUUID("lane1") || a == b {
		t.Fatal("uuid must be stable per name and differ between sessions")
	}
	if len(a) != 36 || a[14] != '5' {
		t.Fatalf("not a v5-shaped uuid: %s", a)
	}
}

func TestLaunchCommandResumesFromTranscriptOnDisk(t *testing.T) {
	d := fixtureDecl(t)
	argv := LaunchCommand(d)
	id := SessionUUID("lane1")
	if argv[0] != "/bin/sh" || argv[1] != "-c" || argv[len(argv)-2] != "--model" || argv[len(argv)-1] != "opus" {
		t.Fatalf("argv = %q", argv)
	}
	script := argv[2]
	for _, want := range []string{
		"exec claude --resume " + id, "exec claude --session-id " + id,
		"$HOME\"/.claude/projects/*/" + id + ".jsonl", "mkdir -p '/home/agent/work/repo'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "--continue") || strings.Contains(script, " -c ") {
		t.Error("most-recent-session flags are forbidden")
	}
	// Declared args are positional parameters, never interpolated into the script.
	if strings.Contains(script, "opus") {
		t.Error("declared args leaked into the script text")
	}
	d.Harness = "codex"
	d.Command = []string{"codex"}
	script = LaunchCommand(d)[2]
	if !strings.Contains(script, "exec codex resume --last") || !strings.Contains(script, ".codex/sessions") {
		t.Errorf("codex script:\n%s", script)
	}
}

func TestRenderedConfigDecodesAndKeepsSecretsOut(t *testing.T) {
	d := fixtureDecl(t)
	raw, err := RenderConfig(d)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := declaredsession.DecodeConfig(raw)
	if err != nil {
		t.Fatalf("holder would refuse the rendered config: %v\n%s", err, raw)
	}
	got, _ := snap.Lookup("lane1")
	if !got.IsContainer() || got.EnvFile != declaredsession.ContainerCredEnvFile || got.Command[0] != "/bin/sh" || len(got.Credentials) != 2 {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestNewSpecRefusals(t *testing.T) {
	d := fixtureDecl(t)
	if _, err := NewSpec(d, "/opt/c", "/n/sessions/lane1", 0, 1000); err == nil {
		t.Error("root uid must be refused")
	}
	if _, err := NewSpec(d, "relative", "/n/sessions/lane1", 1000, 1000); err == nil {
		t.Error("relative binary must be refused")
	}
	host := d
	host.Runtime = nil
	host.Credentials = nil
	if _, err := NewSpec(host, "/opt/c", "/n/sessions/lane1", 1000, 1000); err != ErrNotContainer {
		t.Errorf("host declaration err = %v", err)
	}
	quoted := d
	quoted.Cwd = "/home/agent/work/a'b"
	if _, err := NewSpec(quoted, "/opt/c", "/n/sessions/lane1", 1000, 1000); err == nil {
		t.Error("cwd with a quote must be refused")
	}
}

func TestContainerDeclarationValidation(t *testing.T) {
	cases := map[string]string{
		"image off allowlist": "image: docker.io/library/ubuntu:24.04",
		"unknown size":        "size: huge",
	}
	for name, repl := range cases {
		key := strings.SplitN(repl, ":", 2)[0]
		var out []string
		for _, line := range strings.Split(fixtureConfig, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), key+":") {
				line = "      " + repl
			}
			out = append(out, line)
		}
		if _, err := declaredsession.DecodeConfig([]byte(strings.Join(out, "\n"))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, mutate := range map[string]func(string) string{
		"cwd outside work root": func(s string) string { return strings.Replace(s, "/home/agent/work/repo", "/home/agent/elsewhere", 1) },
		"resume flag declared":  func(s string) string { return strings.Replace(s, "--model, opus", "--resume, x", 1) },
		"credential into work": func(s string) string {
			return strings.Replace(s, "/home/agent/.claude/.credentials.json", "/home/agent/work/cred", 1)
		},
		"credential path+env": func(s string) string {
			return strings.Replace(s, "env: GH_TOKEN", "env: GH_TOKEN\n        path: /home/agent/x", 1)
		},
		"reserved env name": func(s string) string { return strings.Replace(s, "env: GH_TOKEN", "env: HOME", 1) },
		"credential outside config dir": func(s string) string {
			return strings.Replace(s, "/home/agent/.claude/.credentials.json", "/home/agent/other/cred", 1)
		},
		"credential in persisted dir": func(s string) string {
			return strings.Replace(s, "/home/agent/.claude/.credentials.json", "/home/agent/.claude/projects/cred", 1)
		},
		"colon in credential path": func(s string) string {
			return strings.Replace(s, "/home/agent/.claude/.credentials.json", "/home/agent/.claude/a:b", 1)
		},
		"comma in credential path": func(s string) string {
			return strings.Replace(s, "/home/agent/.claude/.credentials.json", "/home/agent/.claude/a,b", 1)
		},
		"comma in cwd":        func(s string) string { return strings.Replace(s, "/home/agent/work/repo", "/home/agent/work/a,b", 1) },
		"unsupported harness": func(s string) string { return strings.Replace(s, "claude-code", "ace-cli", 1) },
	} {
		if _, err := declaredsession.DecodeConfig([]byte(mutate(fixtureConfig))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
