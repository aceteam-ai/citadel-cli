// internal/jobs/service_handler_trust_remote_code_test.go
//
// citadel#848: vllm compose never passed --trust-remote-code, so models
// shipping custom code (gte-multilingual-base, some Qwen/InternLM) crashed
// Exited(1) at model-config creation. --trust-remote-code executes arbitrary
// code from the model repo, so it MUST be opt-in, default OFF, and
// non-sticky (one deploy's opt-in must not silently outlive the model that
// needed it once a DIFFERENT model is later deployed to the same service).
// These tests pin: (1) the embedded compose template's interpolation shape
// and its byte-compatibility with the pre-#848 command when the toggle is
// unset, and (2) the SERVICE_START payload -> sibling-env persistence seam
// (persistServiceTrustRemoteCode / parseTrustRemoteCodeIntent), mirroring the
// existing model-selection tests in service_handler_model_test.go.
//
// Docker itself is never invoked here (sandboxed dev machines in this repo
// may be running a live production node's Docker daemon -- see CLAUDE.md).
// Test (1) simulates the documented Docker Compose interpolation rule for
// ${VAR:+word} (compose-go's substitution: "word" is substituted only when
// VAR is set to a NON-EMPTY value -- unset AND set-but-empty both count as
// "null" and yield nothing, same as POSIX ${VAR:+word}) plus the
// whitespace-collapsing word-split compose applies to a string `command:`.
// The strongest evidence this mechanism works in practice on this exact
// fleet is already sitting in the same folded `command:` block, unmodified
// by this change: `--model ${VLLM_MODEL:-Qwen/Qwen3-8B}` is the sibling
// colon-form interpolation (#530) that has shipped and served real requests
// on citadel nodes since before #848 -- compose-go's interpolation switch
// handles `:-` and `:+` through the same code path, and citadel only ever
// invokes Compose V2 (`docker compose`, never the legacy `docker-compose`
// V1), which supports `:+`. `git diff services/compose/vllm.yml` is the
// direct confirmation that nothing else in the command changed.
package jobs

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// vllmComposeCommand parses the embedded vllm compose file and returns the
// raw (still-interpolated) `command:` string for the vllm service, exactly as
// YAML folding produces it -- the same string docker compose's own YAML
// parser would hand to its interpolation step.
func vllmComposeCommand(t *testing.T) string {
	t.Helper()
	content, ok := embeddedservices.ServiceMap["vllm"]
	if !ok {
		t.Fatal("vllm missing from embedded ServiceMap")
	}
	var doc struct {
		Services struct {
			VLLM struct {
				Command string `yaml:"command"`
			} `yaml:"vllm"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("unmarshal embedded vllm.yml: %v", err)
	}
	if doc.Services.VLLM.Command == "" {
		t.Fatal("vllm compose has no command:")
	}
	return doc.Services.VLLM.Command
}

// expandTrustRemoteCodeToggle simulates Docker Compose's documented
// ${VAR:+word} interpolation rule for the single variable this template uses
// it for, then applies the same whitespace word-split compose performs on a
// string `command:` (extra/collapsed whitespace never produces an empty
// argument token). trustRemoteCodeEnv models the sibling env-file value
// compose would see: "" for both "unset" and "set but empty" (both are
// POSIX-null under the colon form -- this is what makes the explicit
// clear-by-empty-value in persistServiceTrustRemoteCode's Disable branch
// actually turn the flag off, not just omit it).
func expandTrustRemoteCodeToggle(t *testing.T, command, trustRemoteCodeEnv string) []string {
	t.Helper()
	expanded := os.Expand(command, func(key string) string {
		if key == "VLLM_TRUST_REMOTE_CODE:+--trust-remote-code" {
			if trustRemoteCodeEnv != "" {
				return "--trust-remote-code"
			}
			return ""
		}
		// Leave every other interpolation (${VLLM_MODEL:-...}) untouched --
		// this test only cares about the trust-remote-code toggle.
		return "${" + key + "}"
	})
	return strings.Fields(expanded)
}

// preTrustRemoteCodeArgs is the exact argument list the vllm command produced
// before #848 (i.e. with VLLM_MODEL left as its literal ${...:-...} default
// token, since this test does not simulate that interpolation).
var preTrustRemoteCodeArgs = []string{
	"--host", "0.0.0.0",
	"--port", "8000",
	"--model", "${VLLM_MODEL:-Qwen/Qwen3-8B}",
	"--enable-auto-tool-choice",
	"--tool-call-parser", "hermes",
	"--max-model-len", "16384",
	"--gpu-memory-utilization", "0.85",
	"--enforce-eager",
}

// TestVLLMComposeTrustRemoteCodeOptIn pins the embedded template's contract
// (#848): the interpolation is present, it is byte-compatible with the
// pre-#848 argument list both when the var is unset AND when it is
// set-but-empty (the mechanism persistServiceTrustRemoteCode's Disable branch
// relies on to actually turn the flag back off), and it appends exactly one
// token (--trust-remote-code) when set to a non-empty value -- nothing else
// in the command changes in any case.
func TestVLLMComposeTrustRemoteCodeOptIn(t *testing.T) {
	command := vllmComposeCommand(t)

	if !strings.Contains(command, "${VLLM_TRUST_REMOTE_CODE:+--trust-remote-code}") {
		t.Fatalf("embedded vllm compose does not interpolate --trust-remote-code via ${VLLM_TRUST_REMOTE_CODE:+--trust-remote-code}:\n%s", command)
	}

	// "" here stands in for BOTH truly-unset and set-but-empty: compose's
	// ${VAR:+word} interpolation (the colon form) treats them identically --
	// both are POSIX-null -- which is exactly the property
	// persistServiceTrustRemoteCode's Disable branch relies on to actually
	// turn the flag off by writing an empty value rather than deleting the
	// line. This test cannot distinguish the two inputs (os.Expand's callback
	// always fires), which is itself the point: neither input is special.
	unset := expandTrustRemoteCodeToggle(t, command, "")
	if !reflect.DeepEqual(unset, preTrustRemoteCodeArgs) {
		t.Errorf("empty/unset VLLM_TRUST_REMOTE_CODE changed the command; not byte-compatible with pre-#848 shape.\n got:  %v\n want: %v", unset, preTrustRemoteCodeArgs)
	}
	for _, tok := range unset {
		if tok == "" {
			t.Errorf("empty/unset expansion produced an empty argument token: %v", unset)
		}
		if tok == "--trust-remote-code" {
			t.Errorf("--trust-remote-code present despite VLLM_TRUST_REMOTE_CODE being empty/unset")
		}
	}

	for _, on := range []string{"1", "true", "yes"} {
		set := expandTrustRemoteCodeToggle(t, command, on)
		want := append(append([]string{}, preTrustRemoteCodeArgs...), "--trust-remote-code")
		if !reflect.DeepEqual(set, want) {
			t.Errorf("VLLM_TRUST_REMOTE_CODE=%q: got %v, want %v", on, set, want)
		}
	}
}

// TestParseTrustRemoteCodeIntent pins the tri-state contract for the
// SERVICE_START payload's optional trust_remote_code field (#848): absent or
// blank is Unspecified (leave persisted state alone -- NOT "off", since that
// would make a plain restart or an unrelated model's deploy silently clear a
// sibling deploy's opt-in before this field is even wired up); a truthy value
// ("1"/"true"/"yes"/"on", matching the convention used elsewhere in this
// codebase -- energy sampling, self-heal, ...) is Enable; any other non-blank
// value is Disable.
func TestParseTrustRemoteCodeIntent(t *testing.T) {
	cases := []struct {
		value   string
		present bool
		want    trustRemoteCodeIntent
	}{
		{present: false, want: trustRemoteCodeUnspecified},
		{value: "", present: true, want: trustRemoteCodeUnspecified},
		{value: "1", present: true, want: trustRemoteCodeEnable},
		{value: "true", present: true, want: trustRemoteCodeEnable},
		{value: "TRUE", present: true, want: trustRemoteCodeEnable},
		{value: " yes ", present: true, want: trustRemoteCodeEnable},
		{value: "On", present: true, want: trustRemoteCodeEnable},
		{value: "0", present: true, want: trustRemoteCodeDisable},
		{value: "false", present: true, want: trustRemoteCodeDisable},
		{value: "no", present: true, want: trustRemoteCodeDisable},
		{value: "off", present: true, want: trustRemoteCodeDisable},
		{value: "garbage", present: true, want: trustRemoteCodeDisable},
	}
	for _, c := range cases {
		payload := map[string]string{}
		if c.present {
			payload["trust_remote_code"] = c.value
		}
		if got := parseTrustRemoteCodeIntent(payload); got != c.want {
			t.Errorf("parseTrustRemoteCodeIntent(%q, present=%v) = %v, want %v", c.value, c.present, got, c.want)
		}
	}
}

// TestPersistServiceTrustRemoteCode mirrors TestPersistServiceModel: Enable
// writes the sibling <name>.env, changed=true only on the first write, and an
// unmapped engine (no --trust-remote-code parameter) is a graceful no-op --
// no file, no error.
func TestPersistServiceTrustRemoteCode(t *testing.T) {
	h, dir := newModelTestHandler(t)
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}
	envPath := dir + "/services/vllm.env"

	changed, err := h.persistServiceTrustRemoteCode(JobContext{}, svc, trustRemoteCodeEnable)
	if err != nil {
		t.Fatalf("persistServiceTrustRemoteCode(Enable): %v", err)
	}
	if !changed {
		t.Error("first Enable changed = false, want true")
	}
	if v, ok := compose.ReadEnvVar(envPath, "VLLM_TRUST_REMOTE_CODE"); !ok || v != "1" {
		t.Errorf("persisted VLLM_TRUST_REMOTE_CODE = %q, %v, want (1, true)", v, ok)
	}

	// Re-persisting Enable reports changed=false so an identical re-dispatched
	// SERVICE_START does not force an unnecessary container recreate.
	if changed, err = h.persistServiceTrustRemoteCode(JobContext{}, svc, trustRemoteCodeEnable); err != nil || changed {
		t.Errorf("identical re-Enable = (changed=%v, err=%v), want (false, nil)", changed, err)
	}

	// Unspecified never touches an already-persisted value (the "plain
	// restart" / "unrelated deploy" case).
	if changed, err = h.persistServiceTrustRemoteCode(JobContext{}, svc, trustRemoteCodeUnspecified); err != nil || changed {
		t.Errorf("Unspecified after Enable = (changed=%v, err=%v), want (false, nil)", changed, err)
	}
	if v, _ := compose.ReadEnvVar(envPath, "VLLM_TRUST_REMOTE_CODE"); v != "1" {
		t.Errorf("Unspecified cleared a persisted opt-in: %q", v)
	}

	// Disable is the SECURITY-load-bearing case (#848): it must actually turn
	// the flag off by writing an EMPTY value (not deleting the line), which
	// TestVLLMComposeTrustRemoteCodeOptIn proves compose's ${...:+...}
	// interpolation treats identically to unset.
	if changed, err = h.persistServiceTrustRemoteCode(JobContext{}, svc, trustRemoteCodeDisable); err != nil || !changed {
		t.Errorf("Disable after Enable = (changed=%v, err=%v), want (true, nil)", changed, err)
	}
	if v, ok := compose.ReadEnvVar(envPath, "VLLM_TRUST_REMOTE_CODE"); !ok || v != "" {
		t.Errorf("after Disable, VLLM_TRUST_REMOTE_CODE = %q, %v, want (\"\", true)", v, ok)
	}

	// Disable when already off (never set, or already cleared) is a no-op --
	// no spurious recreate for a service that was never opted in.
	if changed, err = h.persistServiceTrustRemoteCode(JobContext{}, svc, trustRemoteCodeDisable); err != nil || changed {
		t.Errorf("Disable when already off = (changed=%v, err=%v), want (false, nil)", changed, err)
	}
}

// TestPersistServiceTrustRemoteCode_UnmappedEngine: ollama has no
// --trust-remote-code parameter, so the flag is ignored gracefully regardless
// of intent.
func TestPersistServiceTrustRemoteCode_UnmappedEngine(t *testing.T) {
	h, dir := newModelTestHandler(t)
	svc := manifestService{Name: "ollama", Type: "docker", ComposeFile: "services/ollama.yml"}

	changed, err := h.persistServiceTrustRemoteCode(JobContext{}, svc, trustRemoteCodeEnable)
	if err != nil {
		t.Fatalf("persistServiceTrustRemoteCode(ollama, Enable): %v", err)
	}
	if changed {
		t.Error("persist(ollama, Enable) changed = true, want false")
	}
	if _, statErr := os.Stat(dir + "/services/ollama.env"); statErr == nil {
		t.Error("an env file was created for an engine with no --trust-remote-code parameter")
	}
}

// TestServiceStartTrustRemoteCodeFlow drives the full Execute path (PATH
// neutered so nothing real is launched -- same technique as
// TestServiceStartModelFlow) and asserts the complete opt-in/opt-out
// lifecycle: (1) trust_remote_code=true persists VLLM_TRUST_REMOTE_CODE=1 to
// the sibling env BEFORE the compose-up attempt; (2) a plain restart (no
// trust_remote_code in the payload) leaves the persisted flag untouched --
// opt-in only, never silently cleared by an unrelated dispatch; and (3) a
// later deploy that explicitly sends trust_remote_code=false clears it -- the
// non-sticky guarantee that keeps this from becoming a de-facto permanent,
// unconditional --trust-remote-code once any one model on the node needed it.
func TestServiceStartTrustRemoteCodeFlow(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no docker, no native binaries
	h, dir := newModelTestHandler(t)
	envPath := dir + "/services/vllm.env"

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "job-trust-1",
		Type: "SERVICE_START",
		Payload: map[string]string{
			"service":           "vllm",
			"trust_remote_code": "true",
		},
	})
	if err != nil {
		t.Fatalf("Execute SERVICE_START with trust_remote_code=true: %v", err)
	}
	if !strings.Contains(string(out), "docker compose up failed") {
		t.Errorf("expected compose-up failure with neutered PATH, got: %s", out)
	}
	if v, ok := compose.ReadEnvVar(envPath, "VLLM_TRUST_REMOTE_CODE"); !ok || v != "1" {
		t.Fatalf("trust_remote_code not persisted by SERVICE_START: %q, %v", v, ok)
	}

	// Plain restart: no trust_remote_code in the payload must not clear the
	// persisted opt-in.
	if _, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "job-trust-2",
		Type:    "SERVICE_START",
		Payload: map[string]string{"service": "vllm"},
	}); err != nil {
		t.Fatalf("Execute plain SERVICE_START: %v", err)
	}
	if v, _ := compose.ReadEnvVar(envPath, "VLLM_TRUST_REMOTE_CODE"); v != "1" {
		t.Errorf("plain restart cleared persisted trust_remote_code: %q", v)
	}

	// A later deploy (e.g. a DIFFERENT model that does not need custom code)
	// explicitly opts back out -- this must actually clear it.
	if _, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "job-trust-3",
		Type: "SERVICE_START",
		Payload: map[string]string{
			"service":           "vllm",
			"trust_remote_code": "false",
		},
	}); err != nil {
		t.Fatalf("Execute SERVICE_START with trust_remote_code=false: %v", err)
	}
	if v, ok := compose.ReadEnvVar(envPath, "VLLM_TRUST_REMOTE_CODE"); !ok || v != "" {
		t.Errorf("explicit trust_remote_code=false did not clear the opt-in: %q, %v", v, ok)
	}

	// A node that never opted in must never see the var at all -- the
	// default-OFF contract (services/compose/vllm.yml's ${...:+...}).
	if args := compose.EnvFileArgs(dir + "/services/ollama.yml"); args != nil {
		t.Errorf("EnvFileArgs for ollama = %v, want nil (trust_remote_code never applies to ollama)", args)
	}
}
