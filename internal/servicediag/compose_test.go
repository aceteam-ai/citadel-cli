package servicediag

import "testing"

const sampleCompose = `
services:
  vllm:
    image: vllm/vllm-openai:latest
    container_name: citadel-vllm
    ports:
      - "${CITADEL_VLLM_HOST_PORT:?citadel must supply CITADEL_VLLM_HOST_PORT}:8000"
    command: >-
      --host 0.0.0.0
      --port 8000
      --model ${VLLM_MODEL:-Qwen/Qwen3-8B}
    environment:
      - HF_TOKEN=${HF_TOKEN}
      - LOG_LEVEL=info
`

func TestMissingRequiredVars_Absent(t *testing.T) {
	env := map[string]string{}
	checks := MissingRequiredVars(sampleCompose, env)
	if len(checks) != 1 {
		t.Fatalf("MissingRequiredVars() = %v, want 1 check", checks)
	}
	if checks[0].Var != "CITADEL_VLLM_HOST_PORT" {
		t.Errorf("Var = %q, want CITADEL_VLLM_HOST_PORT", checks[0].Var)
	}
	if checks[0].Verdict != VerdictFail || checks[0].Reason != "missing" {
		t.Errorf("got Verdict=%q Reason=%q, want fail/missing", checks[0].Verdict, checks[0].Reason)
	}
}

func TestMissingRequiredVars_PresentButEmpty(t *testing.T) {
	env := map[string]string{"CITADEL_VLLM_HOST_PORT": ""}
	checks := MissingRequiredVars(sampleCompose, env)
	if len(checks) != 1 {
		t.Fatalf("MissingRequiredVars() = %v, want 1 check", checks)
	}
	if checks[0].Verdict != VerdictFail || checks[0].Reason != "empty" {
		t.Errorf("got Verdict=%q Reason=%q, want fail/empty", checks[0].Verdict, checks[0].Reason)
	}
}

func TestMissingRequiredVars_PresentAndSet(t *testing.T) {
	env := map[string]string{"CITADEL_VLLM_HOST_PORT": "8213"}
	checks := MissingRequiredVars(sampleCompose, env)
	if len(checks) != 1 {
		t.Fatalf("MissingRequiredVars() = %v, want 1 check", checks)
	}
	if checks[0].Verdict != VerdictOK {
		t.Errorf("got Verdict=%q, want ok", checks[0].Verdict)
	}
}

func TestMissingRequiredVars_NoGuards(t *testing.T) {
	if checks := MissingRequiredVars("services:\n  ollama:\n    image: ollama/ollama\n", nil); checks != nil {
		t.Errorf("MissingRequiredVars() = %v, want nil", checks)
	}
}

func TestSubstituteVars(t *testing.T) {
	env := map[string]string{"VLLM_MODEL": "my-org/my-model", "CITADEL_VLLM_HOST_PORT": "8213"}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"default_used_when_unset", "${UNSET_VAR:-fallback}", "fallback"},
		{"value_wins_over_default", "${VLLM_MODEL:-Qwen/Qwen3-8B}", "my-org/my-model"},
		{"required_set", "${CITADEL_VLLM_HOST_PORT:?must supply}", "8213"},
		{"required_unset", "${MISSING_REQUIRED:?must supply}", "<unset:MISSING_REQUIRED>"},
		{"plain_set", "${VLLM_MODEL}", "my-org/my-model"},
		{"plain_unset", "${NOPE}", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := substituteVars(tt.in, env); got != tt.want {
				t.Errorf("substituteVars(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildComposeInfo(t *testing.T) {
	env := map[string]string{
		"VLLM_MODEL":             "my-org/my-model",
		"CITADEL_VLLM_HOST_PORT": "8213",
		"HF_TOKEN":               "hf_supersecret",
	}
	in := Input{
		ServiceName:     "vllm",
		ComposeContent:  []byte(sampleCompose),
		ComposeSource:   "manifest",
		ComposeFilePath: "/home/user/citadel-node/services/vllm.yml",
		ResolvedEnv:     env,
	}
	ci := buildComposeInfo(in)
	if ci.ParseError != "" {
		t.Fatalf("unexpected ParseError: %s", ci.ParseError)
	}
	wantCmd := "--host 0.0.0.0 --port 8000 --model my-org/my-model"
	if ci.Command != wantCmd {
		t.Errorf("Command = %q, want %q", ci.Command, wantCmd)
	}
	if ci.Env["LOG_LEVEL"] != "info" {
		t.Errorf("Env[LOG_LEVEL] = %q, want info", ci.Env["LOG_LEVEL"])
	}
	if ci.Env["HF_TOKEN"] != redactedPlaceholder {
		t.Errorf("Env[HF_TOKEN] = %q, want redacted", ci.Env["HF_TOKEN"])
	}
	if ci.Source != "manifest" || ci.ComposeFilePath == "" {
		t.Errorf("Source/ComposeFilePath not preserved: %+v", ci)
	}
}

func TestBuildComposeInfo_NoContent(t *testing.T) {
	ci := buildComposeInfo(Input{ServiceName: "vllm"})
	if ci.Command != "" || ci.Env != nil || ci.ParseError != "" {
		t.Errorf("expected empty ComposeInfo for no content, got %+v", ci)
	}
}

func TestBuildComposeInfo_InvalidYAML(t *testing.T) {
	ci := buildComposeInfo(Input{ServiceName: "vllm", ComposeContent: []byte("services: [ this is not valid: yaml")})
	if ci.ParseError == "" {
		t.Errorf("expected ParseError for invalid YAML, got %+v", ci)
	}
}

func TestParseDotEnv(t *testing.T) {
	content := []byte("# a comment\nFOO=bar\n\nBAZ=qux\nMALFORMED_LINE\n")
	got := ParseDotEnv(content)
	if got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Errorf("ParseDotEnv() = %v", got)
	}
	if _, ok := got["MALFORMED_LINE"]; ok {
		t.Errorf("ParseDotEnv() should skip lines with no '='")
	}
}
