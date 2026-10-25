// cmd/job_handlers.go
// Job execution helpers used by test.go for diagnostic testing
package cmd

import (
	"errors"
	"fmt"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"os"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// A map to hold all our registered job handlers.
var jobHandlers map[string]jobs.JobHandler

var diagnosticJobHandlersOnce sync.Once

// Path and permission discovery must happen at command execution, never during
// package initialization (which runs before tests can select fixture paths).
func ensureDiagnosticJobHandlers() {
	diagnosticJobHandlersOnce.Do(func() {
		jobHandlers = buildDiagnosticJobHandlers(loadNodePermissions().Shell, jobs.IndexesDirFor(network.GetNodeConfigDir()))
	})
}

// nodePasscodeVerifier verifies a presented passcode against the node's persisted
// bcrypt passcode (aceteam#6524). It reloads permissions on every call so a
// passcode set/rotated via APPLY_DEVICE_CONFIG or the control center takes effect
// without a worker restart, mirroring the gateway's per-request passcode gate.
// Wired into the SHELL_COMMAND handler so an ENABLED shell still fails closed
// unless the correct node passcode is presented. VerifyPasscode itself fails
// closed (no passcode set, or empty/wrong pin, returns false).
// nodePasscodeThrottle is the process-wide wrong-passcode throttle shared by
// every passcode-gated job surface (SHELL_COMMAND, the WORKFLOW_RUN Shell node,
// the desktop handlers).
var nodePasscodeThrottle = jobs.NewPasscodeThrottle()

func nodePasscodeVerifier(pin string) bool {
	return loadNodePermissions().VerifyPasscode(pin)
}

// nodeHasPasscode reports whether a node passcode is configured (aceteam#6524).
// It reloads permissions on every call, mirroring nodePasscodeVerifier, so the
// SHELL_COMMAND handler can distinguish "no passcode set" (passcode_not_set)
// from "wrong passcode presented" (passcode_invalid) without a worker restart.
// HasPasscode reports only presence, never the hash.
func nodeHasPasscode() bool {
	return loadNodePermissions().HasPasscode()
}

// nodePasscodeState reads the node's passcode configuration once per job, for
// the desktop gate and the SHELL_COMMAND / workflow Shell gates. Unlike
// nodeHasPasscode it never turns a read failure into "no passcode": a
// permissions file it cannot read or parse, or a master-PIN vault it cannot
// stat, is returned as an error so the gate refuses.
func nodePasscodeState() (jobs.PasscodeState, error) {
	return passcodeStateFrom(nodePermissionsDir(), masterVault())
}

// passcodeStateFrom is nodePasscodeState's pure core, taking the permissions
// directory and vault explicitly so tests never read this machine's node state.
func passcodeStateFrom(permissionsDir string, vault *nodevault.Vault) (jobs.PasscodeState, error) {
	perms, err := config.LoadPermissionsStrict(permissionsDir)
	if err != nil {
		return jobs.PasscodeState{}, err
	}
	vaultSet, err := vault.Configured()
	if err != nil {
		return jobs.PasscodeState{}, err
	}
	if vaultSet {
		return jobs.PasscodeState{Set: true, Check: func(pin string) jobs.PasscodeVerdict { return vaultPasscodeVerdict(vault, pin) }}, nil
	}
	// The vault was just checked, so verify the legacy hash directly rather
	// than through the master-PIN hooks, which consult the vault again. The
	// legacy check uses the same node-wide legacy passcode lockout as the
	// network surfaces (#1398), resolved from the vault's node directory so
	// tests stay off this machine's state.
	lockout := vault.LegacyPasscodeLockout()
	return jobs.PasscodeState{Set: perms.PasscodeHash != "", Check: func(pin string) jobs.PasscodeVerdict {
		return legacyPasscodeVerdict(perms, lockout, pin)
	}}, nil
}

// legacyPasscodeVerdict checks pin against the legacy bcrypt hash under the
// node-wide lockout, reporting a lockout as PasscodeLockedOut rather than a
// wrong passcode, as vaultPasscodeVerdict does. An empty pin or an unset hash
// is a mismatch and is not counted.
func legacyPasscodeVerdict(perms *config.Permissions, lockout *nodevault.Lockout, pin string) jobs.PasscodeVerdict {
	err := perms.CheckLegacyPasscode(pin, lockout)
	switch {
	case err == nil:
		return jobs.PasscodeMatch
	case nodevault.IsLockedOut(err):
		return jobs.PasscodeLockedOut
	case errors.Is(err, nodevault.ErrWrongPIN), errors.Is(err, config.ErrPasscodeMismatch):
		return jobs.PasscodeMismatch
	default:
		return jobs.PasscodeUnavailable
	}
}

// vaultPasscodeVerdict checks pin against the master-PIN vault, reporting a
// vault lockout as PasscodeLockedOut rather than a wrong passcode. An empty pin
// is a mismatch and is not counted by the vault.
func vaultPasscodeVerdict(vault *nodevault.Vault, pin string) jobs.PasscodeVerdict {
	if pin == "" {
		return jobs.PasscodeMismatch
	}
	err := vault.VerifyPIN(pin)
	switch {
	case err == nil:
		return jobs.PasscodeMatch
	case nodevault.IsLockedOut(err):
		return jobs.PasscodeLockedOut
	case errors.Is(err, nodevault.ErrWrongPIN):
		return jobs.PasscodeMismatch
	default:
		// Any other vault error (I/O, a corrupt header) says nothing about the
		// presented passcode, so it must not read as, or count as, a wrong one.
		return jobs.PasscodeUnavailable
	}
}

// nodeShellEnabled is the live shell kill-switch. Reloading the small
// permissions file per SHELL_COMMAND makes both local Control Center toggles
// and APPLY_DEVICE_CONFIG effective on the next job without a worker restart.
func nodeShellEnabled() bool {
	return loadNodePermissions().Shell
}

func nodeDesktopEnabled() bool {
	return loadNodePermissions().Desktop
}

func nodeFilesEnabled() bool {
	return loadNodePermissions().Files
}

// executeJob finds the right handler and runs a job.
func executeJob(client *nexus.Client, job *nexus.Job) (string, error) {
	ensureDiagnosticJobHandlers()
	var output []byte
	var err error
	var status string

	handler, ok := jobHandlers[job.Type]
	if !ok {
		err = fmt.Errorf("unsupported job type: %s", job.Type)
	} else {
		jobCtx := jobs.JobContext{}
		output, err = handler.Execute(jobCtx, job)
	}

	if err != nil {
		status = "FAILURE"
		errorMsg := fmt.Sprintf("Execution Error: %v", err)
		// Combine the error and any command output for a full report
		if len(output) > 0 {
			errorMsg = fmt.Sprintf("%s\n---\n%s", errorMsg, string(output))
		}
		output = []byte(errorMsg)
		fmt.Fprintf(os.Stderr, "     - [Job %s] ❌ Execution failed: %v\n", job.ID, err)
	} else {
		status = "SUCCESS"
		fmt.Printf("     - [Job %s] ✅ Execution successful.\n", job.ID)
	}

	update := nexus.JobStatusUpdate{
		Status: status,
		Output: string(output),
	}

	if reportErr := client.UpdateJobStatus(job.ID, update); reportErr != nil {
		fmt.Fprintf(os.Stderr, "     - [Job %s] ⚠️ CRITICAL: Failed to report status back to Nexus: %v\n", job.ID, reportErr)
	}
	return status, err
}

// buildDiagnosticJobHandlers only constructs handlers from explicit inputs.
// Runtime permission/passcode callbacks still reload current policy per job.
func buildDiagnosticJobHandlers(shellEnabled bool, indexesDir string) map[string]jobs.JobHandler {
	// Honor the same default-deny kill-switch as the worker path: SHELL_COMMAND
	// is refused unless the node has explicitly opted in via the persisted
	// `shell` permission. Without this the legacy Nexus/diagnostic path would
	// run commands as root regardless of the permission (aceteam #6149, Phase 0).
	shellHandler := jobs.NewShellCommandHandler("")
	shellHandler.Disabled = !shellEnabled
	shellHandler.Enabled = nodeShellEnabled
	// Even on this legacy Nexus/diagnostic path an enabled shell is passcode-gated
	// (aceteam#6524): executeJob polls remote jobs and reports back, so leaving the
	// verifier nil here would run enabled shell with no passcode. Fail closed.
	// Strict State preserves I/O failures and vault lockouts as distinct refusal
	// reasons; the same process throttle protects diagnostic and worker jobs.
	shellHandler.State = nodePasscodeState
	shellHandler.Throttle = nodePasscodeThrottle

	// Per-org index namespaces (aceteam#10876 C2): point the FILE_INDEX /
	// FILE_SEMANTIC_SEARCH handlers at the machine-convergent indexes dir so a
	// payload `index` namespace resolves here too, not just on the worker path.
	fileIndexHandler := jobs.NewFileIndexHandler("", "")
	fileIndexHandler.IndexesDir = indexesDir
	fileSemanticSearchHandler := jobs.NewFileSemanticSearchHandler("", "")
	fileSemanticSearchHandler.IndexesDir = indexesDir

	// Register all job handlers for test command
	return map[string]jobs.JobHandler{
		"SHELL_COMMAND":        shellHandler,
		"TMUX_SESSION":         jobs.NewTmuxSessionHandler(""),
		"DOWNLOAD_MODEL":       &jobs.DownloadModelHandler{},
		"OLLAMA_PULL":          &jobs.OllamaPullHandler{},
		"LLAMACPP_INFERENCE":   &jobs.LlamaCppInferenceHandler{},
		"VLLM_INFERENCE":       &jobs.VLLMInferenceHandler{},
		"OLLAMA_INFERENCE":     &jobs.OllamaInferenceHandler{},
		"embedding":            &jobs.EmbeddingHandler{},
		"SANDBOX_SUSPEND":      &jobs.SandboxSuspendHandler{},
		"SANDBOX_RESUME":       &jobs.SandboxResumeHandler{},
		"MODEL_CACHE_PULL":     &jobs.ModelCachePullHandler{},
		"MODEL_CACHE_EVICT":    &jobs.ModelCacheEvictHandler{},
		"IOS_BUILD":            jobs.NewIOSBuildHandler(""),
		"ANDROID_BUILD":        jobs.NewAndroidBuildHandler(""),
		"GOMOBILE_BUILD":       jobs.NewGomobileBuildHandler(""),
		"COBROWSE":             jobs.NewCobrowseHandler(),
		"COBROWSE_SESSION":     jobs.NewCobrowseSessionHandler(),
		"FILE_INDEX":           fileIndexHandler,
		"FILE_SEMANTIC_SEARCH": fileSemanticSearchHandler,
		"HTTP_PROXY":           &jobs.HTTPProxyHandler{},
		"WEB_FETCH":            &jobs.WebFetchHandler{},
	}
}
