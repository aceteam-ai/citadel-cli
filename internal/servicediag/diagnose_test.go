package servicediag

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeInspector is an injectable Inspector for tests, so Diagnose never
// touches a real docker daemon.
type fakeInspector struct {
	state    ContainerState
	stateErr error
	logs     []string
	logsErr  error
}

func (f fakeInspector) Inspect(string) (ContainerState, error) { return f.state, f.stateErr }
func (f fakeInspector) LogTail(string, int) ([]string, error)  { return f.logs, f.logsErr }

func TestDiagnose_ExtractsExitCodeAndRootErrorFromLogs(t *testing.T) {
	insp := fakeInspector{
		state: ContainerState{Found: true, Status: "exited", ExitCode: 1, Running: false},
		logs: []string{
			"INFO: loading model",
			"Traceback (most recent call last):",
			`  File "server.py", line 10, in <module>`,
			"ValueError: The checkpoint you are trying to load has model type `newmodel` but Transformers does not recognize this architecture.",
		},
	}
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm"}
	rep := Diagnose(in, insp)

	if !rep.Container.Found || rep.Container.Status != "exited" || rep.Container.ExitCode != 1 {
		t.Fatalf("Container state not captured correctly: %+v", rep.Container)
	}
	wantRootErr := "ValueError: The checkpoint you are trying to load has model type `newmodel` but Transformers does not recognize this architecture."
	if rep.Logs.RootError != wantRootErr {
		t.Errorf("Logs.RootError = %q, want %q", rep.Logs.RootError, wantRootErr)
	}
	if len(rep.Hints) == 0 {
		t.Error("expected at least one hint for a NewModel-not-supported error")
	}
	if rep.Verdict == "" || rep.NextAction == "" {
		t.Error("expected a synthesized Verdict and NextAction")
	}
}

// TestDiagnose_RunningContainerWithErrorInLogs_NoFalseExitedVerdict pins the
// fix for a reported BLOCK: a HEALTHY, currently-running container whose log
// tail contains an old/transient/recovered error line (a startup retry, a
// recovered OOM, ...) must NOT get a verdict claiming "container exited with:
// ..." -- that directly contradicts the [RUNNING] status printed in the same
// report. See synthesize's doc comment for the reasoning.
func TestDiagnose_RunningContainerWithErrorInLogs_NoFalseExitedVerdict(t *testing.T) {
	insp := fakeInspector{
		state: ContainerState{Found: true, Running: true, Status: "running", ExitCode: 0},
		logs: []string{
			"INFO: starting up",
			"ERROR: transient connection retry failed, retrying...",
			"INFO: server ready and serving requests",
		},
	}
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm"}
	rep := Diagnose(in, insp)

	if !rep.Container.Running {
		t.Fatalf("test setup: expected Container.Running=true, got %+v", rep.Container)
	}
	if rep.Logs.RootError == "" {
		t.Fatalf("test setup: expected a matched RootError from the log tail, got none")
	}
	if strings.Contains(rep.Verdict, "exited") {
		t.Errorf("Verdict falsely claims the container exited while Running=true: %q", rep.Verdict)
	}
	if !strings.Contains(rep.Verdict, "running") {
		t.Errorf("Verdict should acknowledge the container is running, got %q", rep.Verdict)
	}
	if !strings.Contains(rep.Verdict, rep.Logs.RootError) {
		t.Errorf("Verdict should still surface the matched log line informationally, got %q (root error %q)", rep.Verdict, rep.Logs.RootError)
	}
}

// TestDiagnose_RunningContainerWithVRAMCheckFail_NoFalseInsufficientVerdict
// covers the same false-contradiction bug as the RootError case above, but
// via the VRAM-fit check: DeclaredVRAMNeedMB is a coarse cold-start budget
// for the engine TYPE, not a live footprint, so it does not know the
// target's own running instance is part of what's already occupying "free"
// VRAM. A healthy running service holding most of the GPU's VRAM legitimately
// fails VRAMFitCheck -- the top verdict must not claim "insufficient free
// VRAM to start this service" directly under a [RUNNING] status.
func TestDiagnose_RunningContainerWithVRAMCheckFail_NoFalseInsufficientVerdict(t *testing.T) {
	insp := fakeInspector{state: ContainerState{Found: true, Running: true, Status: "running", ExitCode: 0}}
	in := Input{
		ServiceName:           "vllm",
		ContainerName:         "citadel-vllm",
		FreeVRAMBytes:         1 * 1024 * 1024 * 1024, // 1GB free
		FreeVRAMKnown:         true,
		DeclaredVRAMNeedMB:    20 * 1024, // 20GB coarse budget
		DeclaredVRAMNeedKnown: true,
	}
	rep := Diagnose(in, insp)

	var vram *PreflightCheck
	for i := range rep.Checks {
		if rep.Checks[i].Name == VRAMFitCheckName {
			vram = &rep.Checks[i]
		}
	}
	if vram == nil || vram.Verdict != VerdictFail {
		t.Fatalf("test setup: expected a failed vram_fit check, got %+v", rep.Checks)
	}
	if strings.Contains(rep.Verdict, "insufficient free VRAM to start") {
		t.Errorf("Verdict falsely claims insufficient VRAM to start while Running=true: %q", rep.Verdict)
	}
	if strings.Contains(rep.Verdict, "to start") {
		t.Errorf("Verdict should not use 'to start' phrasing for an already-running container: %q", rep.Verdict)
	}
	if strings.Contains(rep.Verdict, "no obvious problem") {
		t.Errorf("Verdict should not claim no problem was detected while a preflight check FAILs: %q", rep.Verdict)
	}
}

// TestDiagnose_StoppedContainerWithErrorInLogs_StillGetsExitedVerdict is the
// control for the above: a NON-running container's matched error line should
// still produce the causal "exited with" verdict -- the fix must not
// over-correct into never using that phrasing.
func TestDiagnose_StoppedContainerWithErrorInLogs_StillGetsExitedVerdict(t *testing.T) {
	insp := fakeInspector{
		state: ContainerState{Found: true, Running: false, Status: "exited", ExitCode: 1},
		logs: []string{
			"INFO: starting up",
			"RuntimeError: CUDA out of memory",
		},
	}
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm"}
	rep := Diagnose(in, insp)

	if !strings.Contains(rep.Verdict, "exited with") {
		t.Errorf("expected a causal 'exited with' verdict for a stopped container, got %q", rep.Verdict)
	}
}

func TestDiagnose_RequiredEnvMissingTakesPriorityInVerdict(t *testing.T) {
	insp := fakeInspector{state: ContainerState{Found: false}}
	in := Input{
		ServiceName:    "vllm",
		ContainerName:  "citadel-vllm",
		ComposeContent: []byte(sampleCompose),
		ResolvedEnv:    map[string]string{},
		FreeVRAMBytes:  0,
		FreeVRAMKnown:  false,
	}
	rep := Diagnose(in, insp)

	var failed *PreflightCheck
	for i := range rep.Checks {
		if strings.HasPrefix(rep.Checks[i].Name, "required_env:") && rep.Checks[i].Verdict == VerdictFail {
			failed = &rep.Checks[i]
		}
	}
	if failed == nil {
		t.Fatalf("expected a failed required_env check, got checks: %+v", rep.Checks)
	}
	if !strings.Contains(rep.Verdict, "required compose variable") {
		t.Errorf("Verdict = %q, want it to call out the missing required var", rep.Verdict)
	}
}

// TestDiagnose_RedactsSecretValueFromLogsAndRootError pins the fix for a
// reported BLOCK: raw container log lines and the derived RootError were
// printed completely unredacted, so any secret that appears in application
// logs (an entrypoint echoing env, a stack trace with a credentialed URL, an
// auth-failure message embedding the token) leaked straight through -- even
// though ComposeInfo.Env was already redacted.
func TestDiagnose_RedactsSecretValueFromLogsAndRootError(t *testing.T) {
	insp := fakeInspector{
		state: ContainerState{Found: true, Running: false, Status: "exited", ExitCode: 1},
		logs: []string{
			"INFO: starting with token hf_supersecretvalue123",
			"AuthError: request to https://example.com/hf_supersecretvalue123/models failed",
		},
	}
	in := Input{
		ServiceName:   "vllm",
		ContainerName: "citadel-vllm",
		ResolvedEnv:   map[string]string{"HF_TOKEN": "hf_supersecretvalue123"},
	}
	rep := Diagnose(in, insp)

	for _, line := range rep.Logs.Lines {
		if strings.Contains(line, "hf_supersecretvalue123") {
			t.Errorf("log line %q still contains the secret value", line)
		}
	}
	if strings.Contains(rep.Logs.RootError, "hf_supersecretvalue123") {
		t.Errorf("RootError %q still contains the secret value", rep.Logs.RootError)
	}
	if strings.Contains(rep.Verdict, "hf_supersecretvalue123") {
		t.Errorf("Verdict %q still contains the secret value", rep.Verdict)
	}
	if !strings.Contains(rep.Logs.RootError, redactedPlaceholder) {
		t.Errorf("RootError %q should carry the redaction placeholder", rep.Logs.RootError)
	}
}

// TestDiagnose_DoesNotOverRedactNonSecretRootError is the asymmetry control:
// a NON-secret root-error message (a model name, a plain numeric, an
// arch-mismatch string) must reach the Report/Verdict untouched even when
// ResolvedEnv carries an unrelated secret. Redaction must never blank real
// error text it wasn't asked to.
func TestDiagnose_DoesNotOverRedactNonSecretRootError(t *testing.T) {
	insp := fakeInspector{
		state: ContainerState{Found: true, Running: false, Status: "exited", ExitCode: 1},
		logs: []string{
			"Traceback (most recent call last):",
			"ValueError: The checkpoint you are trying to load has model type `newmodel` but Transformers does not recognize this architecture.",
		},
	}
	in := Input{
		ServiceName:   "vllm",
		ContainerName: "citadel-vllm",
		ResolvedEnv:   map[string]string{"HF_TOKEN": "hf_supersecretvalue123", "PORT": "8213"},
	}
	rep := Diagnose(in, insp)

	wantRootErr := "ValueError: The checkpoint you are trying to load has model type `newmodel` but Transformers does not recognize this architecture."
	if rep.Logs.RootError != wantRootErr {
		t.Errorf("RootError = %q, want unchanged %q (unrelated secret in env must not over-redact)", rep.Logs.RootError, wantRootErr)
	}
	if strings.Contains(rep.Verdict, redactedPlaceholder) {
		t.Errorf("Verdict = %q, unexpectedly contains a redaction placeholder for a non-secret message", rep.Verdict)
	}
}

func TestDiagnose_JSONShape(t *testing.T) {
	insp := fakeInspector{state: ContainerState{Found: true, Running: true, Status: "running"}}
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm", ManifestServiceNames: []string{"vllm"}}
	rep := Diagnose(in, insp)

	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, key := range []string{"service", "managed", "container", "logs", "compose", "checks", "verdict", "next_action"} {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON output missing top-level field %q; got keys: %v", key, m)
		}
	}
	if m["service"] != "vllm" {
		t.Errorf("service = %v, want vllm", m["service"])
	}
}

// --- Degraded paths: never panic, never fail the whole command. ---

func TestDiagnose_DockerUnavailable(t *testing.T) {
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm"}
	rep := Diagnose(in, nil) // nil Inspector == docker/podman unavailable

	if rep.Container.Error == "" {
		t.Error("expected Container.Error to explain docker is unavailable")
	}
	if rep.Logs.Error == "" {
		t.Error("expected Logs.Error to explain docker is unavailable")
	}
	if rep.Verdict == "" {
		t.Error("expected a Verdict even when docker is unavailable")
	}
}

func TestDiagnose_ServiceNotManaged(t *testing.T) {
	insp := fakeInspector{state: ContainerState{Found: false}}
	in := Input{ServiceName: "totally-unknown-service", ContainerName: "citadel-totally-unknown-service"}
	rep := Diagnose(in, insp)

	if rep.Managed {
		t.Error("expected Managed=false for an unrecognized service name")
	}
	// Diagnose still returns a full, non-panicking report -- it is the cmd
	// layer's job to short-circuit with a friendlier message before calling
	// Diagnose for an unmanaged name.
	if rep.Verdict == "" {
		t.Error("expected a Verdict even for an unmanaged service")
	}
}

func TestDiagnose_NoLogAvailable(t *testing.T) {
	insp := fakeInspector{state: ContainerState{Found: true, Running: true, Status: "running"}, logs: nil}
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm"}
	rep := Diagnose(in, insp)

	if rep.Logs.Available {
		t.Error("expected Logs.Available=false when no log lines were returned")
	}
	if rep.Logs.RootError != "" {
		t.Errorf("expected no RootError when no logs are available, got %q", rep.Logs.RootError)
	}
}

func TestDiagnose_InspectErrorDoesNotPanic(t *testing.T) {
	insp := fakeInspector{stateErr: errors.New("docker daemon unreachable")}
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm"}

	rep := Diagnose(in, insp) // must not panic
	if rep.Container.Error == "" {
		t.Error("expected Container.Error to carry the inspect failure")
	}
	if rep.Logs.Error == "" {
		t.Error("expected logs to also degrade to unknown when container state is unknown")
	}
}

func TestDiagnose_ContainerNotFound_NoLogAttempt(t *testing.T) {
	calls := 0
	insp := countingInspector{fakeInspector{state: ContainerState{Found: false}}, &calls}
	in := Input{ServiceName: "vllm", ContainerName: "citadel-vllm"}
	rep := Diagnose(in, insp)

	if calls != 0 {
		t.Errorf("LogTail should not be called for a not-found container; called %d times", calls)
	}
	if rep.Logs.Available {
		t.Error("expected Logs.Available=false for a not-found container")
	}
}

type countingInspector struct {
	fakeInspector
	logTailCalls *int
}

func (c countingInspector) LogTail(name string, max int) ([]string, error) {
	*c.logTailCalls++
	return c.fakeInspector.LogTail(name, max)
}
