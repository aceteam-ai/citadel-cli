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
