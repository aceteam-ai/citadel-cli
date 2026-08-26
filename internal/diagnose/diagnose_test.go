package diagnose

import (
	"strings"
	"testing"
)

func TestExtractRootError_PythonTraceback(t *testing.T) {
	log := strings.Join([]string{
		"INFO 08-25 12:00:00 model_runner.py:100] Loading model...",
		"Traceback (most recent call last):",
		`  File "/vllm/engine.py", line 42, in load`,
		"    raise ValueError(msg)",
		"ValueError: The checkpoint you are trying to load has model type `new_model` " +
			"but Transformers does not recognize this architecture.",
	}, "\n")

	got := ExtractRootError(log)
	want := "ValueError: The checkpoint you are trying to load has model type `new_model` " +
		"but Transformers does not recognize this architecture."
	if got != want {
		t.Fatalf("ExtractRootError() = %q, want %q", got, want)
	}
}

func TestExtractRootError_FallsBackToLastNonEmptyLine(t *testing.T) {
	log := "starting up\nlistening on :8000\n\n"
	got := ExtractRootError(log)
	if got != "listening on :8000" {
		t.Fatalf("ExtractRootError() = %q, want last non-empty line", got)
	}
}

func TestExtractRootError_Empty(t *testing.T) {
	if got := ExtractRootError(""); got != "" {
		t.Fatalf("ExtractRootError(\"\") = %q, want empty", got)
	}
	if got := ExtractRootError("   \n\n  "); got != "" {
		t.Fatalf("ExtractRootError(whitespace) = %q, want empty", got)
	}
}

func TestDetectHints_TrustRemoteCode(t *testing.T) {
	log := "ValueError: Loading this model requires you to execute custom code contained " +
		"in the model repository... set `trust_remote_code=True` to remove this error."
	hints := DetectHints(log)
	if len(hints) != 1 || !strings.Contains(hints[0], "trust-remote-code") {
		t.Fatalf("DetectHints() = %v, want a trust_remote_code hint", hints)
	}
}

func TestDetectHints_MultiplePatterns(t *testing.T) {
	log := "CUDA out of memory. Also: permission denied opening /models/foo"
	hints := DetectHints(log)
	if len(hints) != 2 {
		t.Fatalf("DetectHints() = %v, want 2 hints", hints)
	}
}

func TestDetectHints_NoMatch(t *testing.T) {
	if hints := DetectHints("all good, server started"); len(hints) != 0 {
		t.Fatalf("DetectHints() = %v, want none", hints)
	}
}

func TestCheckVRAMFit(t *testing.T) {
	cases := []struct {
		name               string
		freeMB, needMB     int
		haveFree, haveNeed bool
		wantVerdict        string
	}{
		{"fits", 20000, 16000, true, true, VRAMFits},
		{"insufficient", 6300, 16000, true, true, VRAMInsufficient},
		{"unknown no free signal", 0, 16000, false, true, VRAMUnknown},
		{"unknown no need declared", 20000, 0, true, false, VRAMUnknown},
		{"unknown neither", 0, 0, false, false, VRAMUnknown},
		{"exact fit", 16000, 16000, true, true, VRAMFits},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CheckVRAMFit(c.freeMB, c.haveFree, c.needMB, c.haveNeed)
			if got.Verdict != c.wantVerdict {
				t.Fatalf("CheckVRAMFit() verdict = %q, want %q (got %+v)", got.Verdict, c.wantVerdict, got)
			}
		})
	}
}

const vllmComposeYAML = `
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
      --api-key ${VLLM_API_KEY}
`

func TestResolveCompose_MissingRequiredHostPort(t *testing.T) {
	env := map[string]string{
		// CITADEL_VLLM_HOST_PORT deliberately absent.
		"VLLM_MODEL": "prism-ml/Bonsai-27B",
	}
	cmd, checks := resolveCompose("vllm", vllmComposeYAML, env, nil)

	if !strings.Contains(cmd, "<MISSING:CITADEL_VLLM_HOST_PORT>") {
		// The command block doesn't reference the host port var itself, so
		// nothing to assert there; but the env check must catch it.
		_ = cmd
	}

	var found bool
	for _, ec := range checks {
		if ec.Var == "CITADEL_VLLM_HOST_PORT" {
			found = true
			if !ec.Required {
				t.Fatalf("CITADEL_VLLM_HOST_PORT should be Required (uses :? guard)")
			}
			if ec.Verdict != EnvMissing {
				t.Fatalf("CITADEL_VLLM_HOST_PORT verdict = %q, want %q", ec.Verdict, EnvMissing)
			}
		}
	}
	if !found {
		t.Fatalf("expected CITADEL_VLLM_HOST_PORT among env checks, got %+v", checks)
	}

	if !strings.Contains(cmd, "prism-ml/Bonsai-27B") {
		t.Fatalf("resolved command = %q, want it to contain the interpolated VLLM_MODEL", cmd)
	}
}

func TestResolveCompose_EmptyRequiredVar(t *testing.T) {
	env := map[string]string{"CITADEL_VLLM_HOST_PORT": ""}
	_, checks := resolveCompose("vllm", vllmComposeYAML, env, nil)
	for _, ec := range checks {
		if ec.Var == "CITADEL_VLLM_HOST_PORT" && ec.Verdict != EnvEmpty {
			t.Fatalf("CITADEL_VLLM_HOST_PORT verdict = %q, want %q for an empty-but-set var", ec.Verdict, EnvEmpty)
		}
	}
}

func TestResolveCompose_DefaultFallback(t *testing.T) {
	env := map[string]string{"CITADEL_VLLM_HOST_PORT": "8100"}
	_, checks := resolveCompose("vllm", vllmComposeYAML, env, nil)
	for _, ec := range checks {
		if ec.Var == "VLLM_MODEL" {
			if ec.Verdict != EnvOK {
				t.Fatalf("VLLM_MODEL verdict = %q, want %q (falls back to :- default)", ec.Verdict, EnvOK)
			}
			if ec.Value != "Qwen/Qwen3-8B" {
				t.Fatalf("VLLM_MODEL value = %q, want default %q", ec.Value, "Qwen/Qwen3-8B")
			}
		}
	}
}

func TestResolveCompose_SecretRedaction(t *testing.T) {
	env := map[string]string{
		"CITADEL_VLLM_HOST_PORT": "8100",
		"VLLM_API_KEY":           "sk-supersecret-do-not-leak",
	}
	cmd, checks := resolveCompose("vllm", vllmComposeYAML, env, nil)

	if strings.Contains(cmd, "sk-supersecret-do-not-leak") {
		t.Fatalf("resolved command leaked a secret value: %q", cmd)
	}
	if !strings.Contains(cmd, "***REDACTED***") {
		t.Fatalf("resolved command = %q, want a redacted marker for VLLM_API_KEY", cmd)
	}

	for _, ec := range checks {
		if ec.Var == "VLLM_API_KEY" && strings.Contains(ec.Value, "supersecret") {
			t.Fatalf("env check leaked secret value: %+v", ec)
		}
	}
}

func TestResolveCompose_EffectiveCommandInterpolated(t *testing.T) {
	env := map[string]string{
		"CITADEL_VLLM_HOST_PORT": "8100",
		"VLLM_MODEL":             "BAAI/bge-large-en",
	}
	cmd, _ := resolveCompose("vllm", vllmComposeYAML, env, nil)
	if !strings.Contains(cmd, "--model BAAI/bge-large-en") {
		t.Fatalf("resolved command = %q, want the interpolated model flag", cmd)
	}
	if strings.Contains(cmd, "${") {
		t.Fatalf("resolved command still has unresolved tokens: %q", cmd)
	}
}

// TestResolveCompose_EnvFileKeysForceRedactionRegardlessOfName pins that a
// value sourced from the install-time <name>.env sibling is redacted even
// when its var name doesn't match secretVarRe -- that file is documented as
// secret-bearing with no naming convention citadel controls, so name-pattern
// matching alone must not be the only redaction path.
func TestResolveCompose_EnvFileKeysForceRedactionRegardlessOfName(t *testing.T) {
	env := map[string]string{
		"CITADEL_VLLM_HOST_PORT": "8100",
		"VLLM_API_KEY":           "unnamed-secret-value-123", // name matches secretVarRe too, but...
	}
	// ...pretend it came from the env file and, crucially, that a
	// differently-named var also came from there and must be caught by
	// fileKeys alone.
	env["ACETEAM_PAT"] = "acet-not-secret-shaped-by-name-alone"
	fileKeys := map[string]bool{"VLLM_API_KEY": true, "ACETEAM_PAT": true}

	cmd, checks := resolveCompose("vllm", vllmComposeYAML, env, fileKeys)
	if strings.Contains(cmd, "unnamed-secret-value-123") {
		t.Fatalf("resolved command leaked an env-file-sourced secret: %q", cmd)
	}

	for _, ec := range checks {
		if ec.Var == "VLLM_API_KEY" && strings.Contains(ec.Value, "unnamed-secret") {
			t.Fatalf("env check leaked env-file-sourced secret: %+v", ec)
		}
	}

	// Now confirm a name NOT matching secretVarRe is still redacted purely
	// because it's flagged via fileKeys, using interpolate directly against
	// a synthetic token.
	resolved := interpolate("${ACETEAM_PAT}", env, fileKeys)
	if strings.Contains(resolved, "acet-not-secret-shaped-by-name-alone") {
		t.Fatalf("interpolate() = %q, want ACETEAM_PAT redacted via fileKeys despite its name not matching secretVarRe", resolved)
	}
	if !strings.Contains(resolved, "REDACTED") {
		t.Fatalf("interpolate() = %q, want a redacted marker", resolved)
	}
}

func TestDiagnose_ContainerNeverStarted(t *testing.T) {
	in := Input{
		ServiceName: "bonsai",
		Managed:     true,
		Container:   ContainerState{Exists: false, Name: "citadel-bonsai"},
	}
	r := Diagnose(in)
	if r.Container.Exists {
		t.Fatalf("expected Container.Exists=false")
	}
	if !strings.Contains(r.MostLikelyCause, "never been started") {
		t.Fatalf("MostLikelyCause = %q, want a never-started cause", r.MostLikelyCause)
	}
	if !strings.Contains(r.SuggestedAction, "citadel run") {
		t.Fatalf("SuggestedAction = %q, want a 'citadel run' suggestion", r.SuggestedAction)
	}
}

func TestDiagnose_ExitedWithTraceback(t *testing.T) {
	log := "Traceback (most recent call last):\n  ...\nValueError: NewModel architecture is not supported"
	in := Input{
		ServiceName: "vllm",
		Managed:     true,
		Container:   ContainerState{Exists: true, Name: "citadel-vllm", Status: "exited", ExitCode: 1},
		LogTail:     log,
	}
	r := Diagnose(in)
	if r.RootError == "" {
		t.Fatalf("expected a non-empty RootError")
	}
	if !strings.Contains(r.MostLikelyCause, "not supported") && !strings.Contains(r.MostLikelyCause, "ValueError") {
		t.Fatalf("MostLikelyCause = %q, want it to reference the hint or root error", r.MostLikelyCause)
	}
}

func TestDiagnose_VRAMInsufficientTakesPriority(t *testing.T) {
	in := Input{
		ServiceName:  "vllm",
		Managed:      true,
		Container:    ContainerState{Exists: true, Name: "citadel-vllm", Status: "exited", ExitCode: 1},
		LogTail:      "some unrelated log line",
		FreeVRAMMB:   6300,
		HaveFreeVRAM: true,
		NeedVRAMMB:   16000,
		HaveNeedVRAM: true,
	}
	r := Diagnose(in)
	if r.VRAM.Verdict != VRAMInsufficient {
		t.Fatalf("VRAM.Verdict = %q, want %q", r.VRAM.Verdict, VRAMInsufficient)
	}
	if !strings.Contains(r.MostLikelyCause, "VRAM") {
		t.Fatalf("MostLikelyCause = %q, want the VRAM shortfall to take priority", r.MostLikelyCause)
	}
}

func TestDiagnose_MissingRequiredEnvTakesPriorityOverLog(t *testing.T) {
	in := Input{
		ServiceName: "vllm",
		Managed:     true,
		ComposeRaw:  vllmComposeYAML,
		ResolvedEnv: map[string]string{"VLLM_MODEL": "Qwen/Qwen3-8B"}, // host port missing
		Container:   ContainerState{Exists: true, Name: "citadel-vllm", Status: "exited", ExitCode: 1},
		LogTail:     "Traceback...\nValueError: something else entirely",
	}
	r := Diagnose(in)
	if !strings.Contains(r.MostLikelyCause, "CITADEL_VLLM_HOST_PORT") {
		t.Fatalf("MostLikelyCause = %q, want the missing required env var to take priority over the log-derived cause", r.MostLikelyCause)
	}
}

func TestDiagnose_RunningNoObviousProblem(t *testing.T) {
	in := Input{
		ServiceName: "vllm",
		Managed:     true,
		Container:   ContainerState{Exists: true, Name: "citadel-vllm", Status: "running", ExitCode: 0},
	}
	r := Diagnose(in)
	if !strings.Contains(r.MostLikelyCause, "no obvious problem") {
		t.Fatalf("MostLikelyCause = %q, want a no-obvious-problem verdict for a running container", r.MostLikelyCause)
	}
}

// TestDiagnose_RunningWithBenignHintPatternIsNotFlagged pins the ordering fix:
// a healthy vLLM startup routinely logs capability notices that match the
// "not supported" hint regex (e.g. flash-attention fallback messages). A
// running container must not have its cause overridden by that hint.
func TestDiagnose_RunningWithBenignHintPatternIsNotFlagged(t *testing.T) {
	in := Input{
		ServiceName: "vllm",
		Managed:     true,
		Container:   ContainerState{Exists: true, Name: "citadel-vllm", Status: "running", ExitCode: 0},
		LogTail:     "INFO: flash attention 2 not supported for this GPU, falling back to xformers",
	}
	r := Diagnose(in)
	if !strings.Contains(r.MostLikelyCause, "no obvious problem") {
		t.Fatalf("MostLikelyCause = %q, want the running-container verdict to win over a benign hint match", r.MostLikelyCause)
	}
	// The hint is still surfaced in the report for context, just not
	// promoted to the cause.
	if len(r.Hints) == 0 {
		t.Fatalf("expected the hint to still be reported for context")
	}
}

func TestRedactSecretsFromLog(t *testing.T) {
	env := map[string]string{
		"VLLM_API_KEY": "sk-supersecret-do-not-leak",
		"VLLM_MODEL":   "Qwen/Qwen3-8B", // not secret-shaped; must survive
	}
	in := Input{
		ServiceName: "vllm",
		Container:   ContainerState{Exists: true, Status: "exited", ExitCode: 1},
		LogTail:     "starting with args --api-key sk-supersecret-do-not-leak --model Qwen/Qwen3-8B",
		ResolvedEnv: env,
	}
	r := Diagnose(in)
	if strings.Contains(r.LogTail, "sk-supersecret-do-not-leak") {
		t.Fatalf("LogTail leaked a secret value: %q", r.LogTail)
	}
	if !strings.Contains(r.LogTail, "***REDACTED***") {
		t.Fatalf("LogTail = %q, want a redacted marker", r.LogTail)
	}
	if !strings.Contains(r.LogTail, "Qwen/Qwen3-8B") {
		t.Fatalf("LogTail = %q, want the non-secret model name preserved", r.LogTail)
	}
}

// TestDiagnose_LogTailDoesNotOverRedactEnvFileSourcedValues pins the
// deliberate asymmetry: EnvFileKeys forces redaction in EnvChecks/
// ComposeCommand (a value displayed as a value), but must NOT extend to the
// raw log-tail scrub -- otherwise a root error that legitimately mentions an
// env-file-sourced, non-secret value (a model name) would come out mangled,
// corrupting ExtractRootError's read of the same text.
func TestDiagnose_LogTailDoesNotOverRedactEnvFileSourcedValues(t *testing.T) {
	in := Input{
		ServiceName: "vllm",
		ComposeRaw:  vllmComposeYAML,
		Container:   ContainerState{Exists: true, Status: "exited", ExitCode: 1},
		LogTail:     "Traceback...\nValueError: model BAAI/bge-large-en is an embedding model, not chat-capable",
		ResolvedEnv: map[string]string{
			"CITADEL_VLLM_HOST_PORT": "8100",
			"VLLM_MODEL":             "BAAI/bge-large-en",
		},
		// Pretend VLLM_MODEL came from the install-time env file: it must
		// still be redacted in EnvChecks/ComposeCommand...
		EnvFileKeys: map[string]bool{"VLLM_MODEL": true},
	}
	r := Diagnose(in)

	if strings.Contains(r.ComposeCommand, "BAAI/bge-large-en") {
		t.Fatalf("ComposeCommand = %q, want the env-file-sourced value redacted", r.ComposeCommand)
	}
	for _, ec := range r.EnvChecks {
		if ec.Var == "VLLM_MODEL" && strings.Contains(ec.Value, "BAAI/bge-large-en") {
			t.Fatalf("EnvChecks leaked the env-file-sourced value: %+v", ec)
		}
	}

	// ...but the SAME value appearing in the log must survive intact, so
	// ExtractRootError reads the real error rather than a mangled one.
	if !strings.Contains(r.LogTail, "BAAI/bge-large-en") {
		t.Fatalf("LogTail = %q, want the env-file-sourced value NOT scrubbed from the raw log", r.LogTail)
	}
	if !strings.Contains(r.RootError, "BAAI/bge-large-en") {
		t.Fatalf("RootError = %q, want it to contain the un-mangled model name", r.RootError)
	}
}

func TestDiagnose_UnmanagedServiceDoesNotPanic(t *testing.T) {
	in := Input{ServiceName: "totally-unknown-thing"}
	r := Diagnose(in) // must not panic
	if r.Managed {
		t.Fatalf("expected Managed=false")
	}
	if r.MostLikelyCause == "" || r.SuggestedAction == "" {
		t.Fatalf("expected a non-empty cause/action even for a fully empty Input")
	}
}

func TestDiagnose_LogTailBounded(t *testing.T) {
	huge := strings.Repeat("x", maxLogTailBytes*3)
	in := Input{ServiceName: "vllm", LogTail: huge}
	r := Diagnose(in)
	if len(r.LogTail) != maxLogTailBytes {
		t.Fatalf("LogTail length = %d, want bounded to %d", len(r.LogTail), maxLogTailBytes)
	}
}

func TestDiagnose_JSONShapeHasStableTopLevelKeys(t *testing.T) {
	in := Input{
		ServiceName: "vllm",
		Managed:     true,
		Container:   ContainerState{Exists: true, Name: "citadel-vllm", Status: "exited", ExitCode: 1},
	}
	r := Diagnose(in)
	if r.ServiceName != "vllm" {
		t.Fatalf("ServiceName = %q", r.ServiceName)
	}
	if r.Container.Name != "citadel-vllm" {
		t.Fatalf("Container.Name = %q", r.Container.Name)
	}
	if r.VRAM.Verdict != VRAMUnknown {
		t.Fatalf("VRAM.Verdict = %q, want %q when no GPU signal was supplied", r.VRAM.Verdict, VRAMUnknown)
	}
}

func TestExtractCommand_ListForm(t *testing.T) {
	yamlDoc := `
services:
  foo:
    command: ["--host", "0.0.0.0", "--port", "8000"]
`
	got := extractCommand("foo", yamlDoc)
	want := "--host 0.0.0.0 --port 8000"
	if got != want {
		t.Fatalf("extractCommand() = %q, want %q", got, want)
	}
}

func TestExtractCommand_UnknownServiceOrBadYAML(t *testing.T) {
	if got := extractCommand("nope", vllmComposeYAML); got != "" {
		t.Fatalf("extractCommand(unknown service) = %q, want empty", got)
	}
	if got := extractCommand("vllm", "not: [valid yaml"); got != "" {
		t.Fatalf("extractCommand(bad yaml) = %q, want empty", got)
	}
}
